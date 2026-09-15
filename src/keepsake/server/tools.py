"""The seven MCP tools. Validation runs here so every write path shares it.

No tool takes a tenant: the server binds one at startup, so an agent cannot name
the wrong one. Nothing a tool raises or returns to the agent carries a body it
was not already entitled to read.
"""

from __future__ import annotations

import inspect
import json
from collections.abc import Awaitable, Callable
from typing import Any
from uuid import UUID

import mcp_types as types
from mcp.server.context import ServerRequestContext
from mcp.server.lowlevel import Server

from keepsake.store.concepts import ConceptStore, Conflict
from okf_core import Concept, extract_links, validate

_STRING: dict[str, Any] = {"type": "string"}
_INTEGER: dict[str, Any] = {"type": "integer"}
_OBJECT: dict[str, Any] = {"type": "object"}

_CONCEPT_FIELDS: dict[str, Any] = {
    "type": _STRING,
    "title": _STRING,
    "description": _STRING,
    "body": _STRING,
    "frontmatter": _OBJECT,
}


class ToolError(ValueError):
    """Surfaced to the agent. MUST NOT contain a concept body."""


# An argument the caller omitted, distinct from one it sent as null.
_UNSET = object()


def _schema(properties: dict[str, Any], *required: str) -> dict[str, Any]:
    return {"type": "object", "properties": properties, "required": list(required)}


def _results(item: dict[str, Any]) -> dict[str, Any]:
    """The envelope a list answer travels in, declared so the wrapper is discoverable."""
    return _schema({"results": {"type": "array", "items": item}}, "results")


# Written for an agent reading them cold, with no other documentation.
_TOOLS: tuple[types.Tool, ...] = (
    types.Tool(
        name="okf_list",
        description=(
            "List the concepts stored here, with a count of each type. Pass `prefix` "
            "to scope to one part of the tree (`detect/` lists everything beneath "
            "`detect`); omit it to see everything. This is the cheapest way to learn "
            "the shape of the knowledge base before searching it."
        ),
        input_schema=_schema({"prefix": _STRING}),
    ),
    types.Tool(
        name="okf_search",
        description=(
            "Find concepts by keyword, ranked by relevance. Returns cards — path, "
            "type, title, description, score — and never a body; read a promising "
            "path with okf_read.\n\n"
            "Matching is lexical, not semantic: the index holds the words that were "
            "actually written, so distinctive keywords ('dormant', 'PKCE', "
            "'indextime') find far more than a natural-language question does. Terms "
            "are OR-ed, so every extra term broadens the result instead of narrowing "
            "it: add terms to cast wider, drop them to focus. `limit` is required — "
            "ask for the fewest results you can use. `prefix` confines the search to "
            "one part of the tree."
        ),
        input_schema=_schema(
            {"query": _STRING, "limit": _INTEGER, "prefix": _STRING}, "query", "limit"
        ),
        output_schema=_results(
            _schema(
                {
                    "path": _STRING,
                    "type": _STRING,
                    "title": _STRING,
                    "description": _STRING,
                    "score": {"type": "number"},
                },
                "path",
                "type",
                "title",
                "description",
                "score",
            )
        ),
    ),
    types.Tool(
        name="okf_grep",
        description=(
            "Search concept text with a POSIX regular expression, case-insensitively. "
            "Returns each matching path with a short snippet around the match. Use it "
            "when you know the exact string or shape you want — an identifier, a "
            "config key, a URL — and okf_search's word matching is too loose. "
            "`limit` is required."
        ),
        input_schema=_schema(
            {"pattern": _STRING, "limit": _INTEGER}, "pattern", "limit"
        ),
        output_schema=_results(
            _schema({"path": _STRING, "snippet": _STRING}, "path", "snippet")
        ),
    ),
    types.Tool(
        name="okf_read",
        description=(
            "Read one concept in full: body, frontmatter, the concepts it links to, "
            "and the concepts that link back to it. Returns null if nothing is stored "
            "at that path. Take paths from okf_list, okf_search or okf_grep."
        ),
        input_schema=_schema({"path": _STRING}, "path"),
    ),
    types.Tool(
        name="okf_create",
        description=(
            "Store a new concept. `path` is relative and carries no `.md` suffix "
            "(`detect/dormant-rules`). `type` is required and says what kind of thing "
            "this is — Concept, Runbook, Decision. Fails if the path is taken; change "
            "an existing concept with okf_update.\n\n"
            "Links are read out of `body`, never declared separately, so relate a "
            "concept by linking to it inline: `[dormant rules](/detect/dormant-"
            "rules.md)`."
        ),
        input_schema=_schema({"path": _STRING} | _CONCEPT_FIELDS, "path", "type"),
    ),
    types.Tool(
        name="okf_update",
        description=(
            "Change an existing concept. Fields you leave out keep the values they "
            "have. Pass `expected_version` (from okf_read) to make the write a "
            "compare-and-swap: if anything has been written since, nothing changes and "
            "you get back the current version and body to merge against. Leave it out "
            "only when overwriting whatever is there is acceptable."
        ),
        input_schema=_schema(
            {"path": _STRING, "expected_version": _INTEGER} | _CONCEPT_FIELDS, "path"
        ),
    ),
    types.Tool(
        name="okf_relate",
        description=(
            "Record that one concept relates to another by appending a link from "
            "`from_path` to `to_path`. The edge then shows up as an outbound link on "
            "the source and as a backlink on the target."
        ),
        input_schema=_schema(
            {"from_path": _STRING, "to_path": _STRING}, "from_path", "to_path"
        ),
    ),
)


class Tools:
    # ponytail: every call blocks the event loop on psycopg, and under stateless HTTP
    # that stalls the session manager's own tasks, not just sibling tool calls. Move the
    # store calls to anyio.to_thread once one pod has to serve concurrent agents.
    def __init__(self, concepts: ConceptStore, tenant_id: UUID, actor: str) -> None:
        self._c = concepts
        self._t = tenant_id
        self._actor = actor

    def _concept(self, path: str, **kw: Any) -> Concept:
        body = str(kw.get("body", ""))
        # Absent means leave it alone; anything present must be an object. A falsy
        # non-dict — null, "", [] — would otherwise coerce to {}, which on the update
        # path erases what is stored.
        frontmatter = kw.get("frontmatter", _UNSET)
        if frontmatter is _UNSET:
            frontmatter = {}
        elif not isinstance(frontmatter, dict):
            raise ToolError("frontmatter must be an object")
        c = Concept(
            path=path,
            type=str(kw.get("type", "")),
            title=str(kw.get("title", "")),
            description=str(kw.get("description", "")),
            body=body,
            frontmatter=dict(frontmatter),
            # Derived, never taken from the caller: an edge exists only where a reader
            # of the document would see one. A `links` argument is deliberately ignored.
            links=extract_links(body, path),
        )
        if errors := validate(c):
            raise ToolError("; ".join(errors))
        return c

    async def create(self, path: str, **kw: Any) -> dict[str, Any]:
        version = self._c.create(self._t, self._concept(path, **kw), self._actor)
        if version is None:
            raise ToolError(f"concept already exists at {path}")
        return {"path": path, "version": version}

    async def update(
        self, path: str, expected_version: int | None = None, **kw: Any
    ) -> dict[str, Any]:
        existing = self._c.read(self._t, path)
        if existing is None:
            raise ToolError(f"no concept at {path}")
        merged = {
            "type": existing.type,
            "title": existing.title,
            "description": existing.description,
            "body": existing.body,
            "frontmatter": existing.frontmatter,
        } | kw
        try:
            result = self._c.update(
                self._t, self._concept(path, **merged), self._actor, expected_version
            )
        except KeyError:
            # Another tenant's row is hidden from both statements, and stays that way:
            # a distinguishable answer here would be a cross-tenant existence oracle.
            raise ToolError(f"no concept at {path}") from None
        if isinstance(result, Conflict):
            return {
                "conflict": True,
                "current_version": result.current_version,
                "current_body": result.current_body,
            }
        return {"path": path, "version": result}

    async def search(
        self, query: str, limit: int, prefix: str | None = None
    ) -> list[dict[str, Any]]:
        return [
            {
                "path": h.path,
                "type": h.type,
                "title": h.title,
                "description": h.description,
                "score": h.score,
            }
            for h in self._c.search(self._t, query, limit, prefix)
        ]

    async def grep(self, pattern: str, limit: int) -> list[dict[str, Any]]:
        try:
            rows = self._c.grep(self._t, pattern, limit)
        except ValueError as exc:
            raise ToolError(str(exc)) from None
        return [{"path": p, "snippet": s} for p, s in rows]

    async def list_(self, prefix: str = "") -> dict[str, Any]:
        rows = self._c.list_(self._t, prefix)
        counts: dict[str, int] = {}
        for _, type_ in rows:
            counts[type_] = counts.get(type_, 0) + 1
        return {"paths": [p for p, _ in rows], "counts": counts}

    async def read(self, path: str) -> dict[str, Any] | None:
        c = self._c.read(self._t, path)
        if c is None:
            return None
        return {
            "path": c.path,
            "type": c.type,
            "title": c.title,
            "description": c.description,
            "body": c.body,
            "frontmatter": c.frontmatter,
            "version": c.version,
            "links": list(c.links),
            "backlinks": self._c.backlinks(self._t, path),
        }

    async def relate(self, from_path: str, to_path: str) -> dict[str, Any]:
        source = self._c.read(self._t, from_path)
        if source is None:
            raise ToolError(f"no concept at {from_path}")
        # Rooted, not relative: a bare `to_path` would resolve against the source's
        # own directory and name a concept that does not exist.
        body = f"{source.body}\n\n[{to_path}](/{to_path}.md)\n"
        return await self.update(from_path, expected_version=source.version, body=body)


def _failed(message: str) -> types.CallToolResult:
    """A tool error, not a protocol error: the agent sees it and can correct itself."""
    return types.CallToolResult(
        content=[types.TextContent(text=message)], is_error=True
    )


def register(server: Server[Any], tools: Tools) -> None:
    """Advertise the seven okf_* tools on `server` and route their calls to `tools`."""
    dispatch: dict[str, Callable[..., Awaitable[Any]]] = {
        "okf_list": tools.list_,
        "okf_search": tools.search,
        "okf_grep": tools.grep,
        "okf_read": tools.read,
        "okf_create": tools.create,
        "okf_update": tools.update,
        "okf_relate": tools.relate,
    }

    async def on_list_tools(
        ctx: ServerRequestContext[Any], params: types.PaginatedRequestParams | None
    ) -> types.ListToolsResult:
        return types.ListToolsResult(tools=list(_TOOLS))

    async def on_call_tool(
        ctx: ServerRequestContext[Any], params: types.CallToolRequestParams
    ) -> types.CallToolResult:
        call = dispatch.get(params.name)
        if call is None:
            # An error result, not a protocol error: an agent can correct itself from a
            # tool result and cannot from a transport failure.
            return _failed(f"no such tool: {params.name}")
        arguments = params.arguments or {}
        try:
            inspect.signature(call).bind(**arguments)
        except TypeError as exc:
            # Bound before the call so only an argument mistake reports as one; a
            # TypeError from inside a tool is a defect here and must surface as one.
            return _failed(f"{params.name}: {exc}")
        try:
            result = await call(**arguments)
        except ToolError as exc:
            return _failed(str(exc))
        # Structured content is object-only through protocol 2025-11-25, so the tools
        # that answer with a list hand it back under one key.
        structured = {"results": result} if isinstance(result, list) else result
        return types.CallToolResult(
            content=[types.TextContent(text=json.dumps(structured))],
            structured_content=structured,
        )

    server.add_request_handler(
        "tools/list", types.PaginatedRequestParams, on_list_tools
    )
    server.add_request_handler("tools/call", types.CallToolRequestParams, on_call_tool)
