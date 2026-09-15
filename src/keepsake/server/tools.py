"""The seven MCP tools. Validation runs here so every write path shares it.

No tool takes a tenant: the server binds one at startup, so an agent cannot name
the wrong one. Nothing a tool raises or returns to the agent carries a body it
was not already entitled to read.
"""

from __future__ import annotations

import inspect
import json
from collections import Counter
from collections.abc import Callable
from functools import partial
from typing import Any
from uuid import UUID

import jsonschema
import mcp_types as types
import psycopg
from anyio import CapacityLimiter, to_thread
from mcp.server.context import ServerRequestContext
from mcp.server.lowlevel import Server

from keepsake.store import POOL_SIZE
from keepsake.store.concepts import ConceptStore, Conflict
from okf_core import Concept, extract_links, validate

_STRING: dict[str, Any] = {"type": "string"}
_OBJECT: dict[str, Any] = {"type": "object"}

# The most results a single call may ask for. Uncapped, one call materialises the
# whole corpus into one response; advertised, an agent can see the ceiling it has.
MAX_LIMIT = 200
_LIMIT: dict[str, Any] = {"type": "integer", "minimum": 0, "maximum": MAX_LIMIT}
_VERSION: dict[str, Any] = {"type": "integer", "minimum": 1}

# How many times `relate` re-reads and re-appends past a concurrent writer. Each round
# has exactly one winner, so this is the number of agents that may relate one source at
# once before the slowest is told to try again.
_RELATE_ATTEMPTS = 20

# The database is unreachable, failing over, or the pool timed out waiting for it —
# psycopg_pool's PoolTimeout and PoolClosed both derive from OperationalError, as does
# the "terminating connection due to administrator command" a restart produces. Its
# siblings under DatabaseError — integrity, programming, data — are defects here and
# MUST keep surfacing as such.
_UNAVAILABLE = psycopg.OperationalError

_CONCEPT_FIELDS: dict[str, Any] = {
    "type": _STRING,
    "title": _STRING,
    "description": _STRING,
    "body": _STRING,
    "frontmatter": _OBJECT,
}


class ToolError(ValueError):
    """Surfaced to the agent. MUST NOT contain a concept body."""


def _schema(properties: dict[str, Any], *required: str) -> dict[str, Any]:
    # Closed, and enforced before dispatch: an argument the tool does not read is a
    # caller believing something it asked for took effect. `links` is the one that
    # bites — it is derived from the body, and silently ignoring it taught an agent
    # that it had recorded edges it had not.
    return {
        "type": "object",
        "properties": properties,
        "required": list(required),
        "additionalProperties": False,
    }


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
            {"query": _STRING, "limit": _LIMIT, "prefix": _STRING}, "query", "limit"
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
        input_schema=_schema({"pattern": _STRING, "limit": _LIMIT}, "pattern", "limit"),
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
            {"path": _STRING, "expected_version": _VERSION} | _CONCEPT_FIELDS, "path"
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
    """The tool bodies. Every one is synchronous, because psycopg is: `register`
    runs them on a worker thread rather than on the event loop."""

    def __init__(self, concepts: ConceptStore, tenant_id: UUID, actor: str) -> None:
        self._c = concepts
        self._t = tenant_id
        self._actor = actor

    @staticmethod
    def _text(kw: dict[str, Any], name: str) -> str:
        """An absent field is empty; a present one must already be a string.

        `str()` would coerce instead, so `null` became the four characters `None` and
        overwrote what is stored, having passed validation.
        """
        value = kw.get(name, "")
        if not isinstance(value, str):
            raise ToolError(f"{name} must be a string")
        return value

    def _concept(self, path: str, **kw: Any) -> Concept:
        body = self._text(kw, "body")
        # Absent means leave it alone; anything present must be an object. A falsy
        # non-dict — null, "", [] — would otherwise coerce to {}, which on the update
        # path erases what is stored.
        frontmatter = kw.get("frontmatter", {})
        if not isinstance(frontmatter, dict):
            raise ToolError("frontmatter must be an object")
        c = Concept(
            path=path,
            type=self._text(kw, "type"),
            title=self._text(kw, "title"),
            description=self._text(kw, "description"),
            body=body,
            frontmatter=dict(frontmatter),
            # Derived, never taken from the caller: an edge exists only where a reader
            # of the document would see one. A `links` argument is deliberately ignored.
            links=extract_links(body, path),
        )
        if errors := validate(c):
            raise ToolError("; ".join(errors))
        return c

    def create(self, path: str, **kw: Any) -> dict[str, Any]:
        version = self._c.create(self._t, self._concept(path, **kw), self._actor)
        if version is None:
            raise ToolError(f"concept already exists at {path}")
        return {"path": path, "version": version}

    def update(
        self, path: str, expected_version: int | None = None, **kw: Any
    ) -> dict[str, Any]:
        existing = self._c.read(self._t, path)
        if existing is None:
            raise ToolError(f"no concept at {path}")
        return self._write(existing, path, expected_version, **kw)

    def _write(
        self, existing: Concept, path: str, expected_version: int | None, **kw: Any
    ) -> dict[str, Any]:
        """Write `kw` over the concept the caller already read."""
        # Off the advertised schema: a sixth field hand-listed here would be accepted
        # by the tool and then dropped on every partial write.
        merged = {f: getattr(existing, f) for f in _CONCEPT_FIELDS} | kw
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

    def search(
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

    def grep(self, pattern: str, limit: int) -> list[dict[str, Any]]:
        try:
            rows = self._c.grep(self._t, pattern, limit)
        except ValueError as exc:
            raise ToolError(str(exc)) from None
        return [{"path": p, "snippet": s} for p, s in rows]

    def list_(self, prefix: str = "") -> dict[str, Any]:
        rows = self._c.list_(self._t, prefix)
        return {"paths": [p for p, _ in rows], "counts": Counter(t for _, t in rows)}

    def read(self, path: str) -> dict[str, Any] | None:
        found = self._c.read_with_backlinks(self._t, path)
        if found is None:
            return None
        c, backlinks = found
        return {
            "path": c.path,
            "type": c.type,
            "title": c.title,
            "description": c.description,
            "body": c.body,
            "frontmatter": c.frontmatter,
            "version": c.version,
            "links": list(c.links),
            "backlinks": backlinks,
        }

    def relate(self, from_path: str, to_path: str) -> dict[str, Any]:
        """Append the edge, retrying past concurrent writers.

        Appending a link commutes, so a compare-and-swap conflict here is nobody's
        decision to make: the agent asked for an edge that is still missing, and a
        conflict hands it back a body it never wanted to merge.
        """
        for _ in range(_RELATE_ATTEMPTS):
            source = self._c.read(self._t, from_path)
            if source is None:
                raise ToolError(f"no concept at {from_path}")
            if to_path in source.links:
                # Idempotent. An agent that retries MUST NOT append the link twice:
                # the body grew on every call and the version moved for no change.
                return {"path": from_path, "version": source.version}
            # Rooted, not relative: a bare `to_path` would resolve against the source's
            # own directory and name a concept that does not exist.
            body = f"{source.body}\n\n[{to_path}](/{to_path}.md)\n"
            # Compare-and-swap, so a writer that slipped in since the read is detected
            # rather than overwritten — and then retried, above.
            result = self._write(source, from_path, source.version, body=body)
            if not result.get("conflict"):
                return result
        raise ToolError(
            f"{from_path} is being rewritten faster than the link could be recorded"
        )


def _failed(message: str) -> types.CallToolResult:
    """A tool error, not a protocol error: the agent sees it and can correct itself."""
    return types.CallToolResult(
        content=[types.TextContent(text=message)], is_error=True
    )


def register(server: Server[Any], tools: Tools) -> None:
    """Advertise the seven okf_* tools on `server` and route their calls to `tools`."""
    handlers: dict[str, Callable[..., Any]] = {
        "okf_list": tools.list_,
        "okf_search": tools.search,
        "okf_grep": tools.grep,
        "okf_read": tools.read,
        "okf_create": tools.create,
        "okf_update": tools.update,
        "okf_relate": tools.relate,
    }
    # Keyed off the advertised names, so a tool cannot be advertised without a handler.
    # The signature is bound per call but never changes, so it is taken once, and the
    # validator is compiled once rather than per call.
    dispatch = {
        t.name: (
            handlers[t.name],
            inspect.signature(handlers[t.name]),
            jsonschema.Draft202012Validator(t.input_schema),
        )
        for t in _TOOLS
    }
    # The tool bodies block on psycopg, so they run on worker threads. Bounded by the
    # pool: a thread past that number holds no connection and only waits for one,
    # which turns pool starvation into a queue instead of a PoolTimeout.
    limiter = CapacityLimiter(POOL_SIZE)

    async def on_list_tools(
        ctx: ServerRequestContext[Any], params: types.PaginatedRequestParams | None
    ) -> types.ListToolsResult:
        return types.ListToolsResult(tools=list(_TOOLS))

    async def on_call_tool(
        ctx: ServerRequestContext[Any], params: types.CallToolRequestParams
    ) -> types.CallToolResult:
        entry = dispatch.get(params.name)
        if entry is None:
            # An error result, not a protocol error: an agent can correct itself from a
            # tool result and cannot from a transport failure.
            return _failed(f"no such tool: {params.name}")
        call, signature, validator = entry
        arguments = params.arguments or {}
        # The advertised schema, enforced. Without this a path that is not a string
        # reached posixpath, a limit that is not an integer reached Postgres, and each
        # came back to the agent as a protocol error it had no way to correct.
        if bad := sorted(validator.iter_errors(arguments), key=lambda e: e.json_path):
            return _failed(
                f"{params.name}: "
                + "; ".join(
                    f"{e.json_path.removeprefix('$.')}: {e.message}" for e in bad
                )
            )
        try:
            signature.bind(**arguments)
        except TypeError as exc:
            # Unreachable for a caller's mistake now that the schema is enforced: what
            # is left is a schema and a handler that disagree, which is a defect here.
            return _failed(f"{params.name}: {exc}")
        try:
            result = await to_thread.run_sync(
                partial(call, **arguments), limiter=limiter
            )
        except ToolError as exc:
            return _failed(str(exc))
        except _UNAVAILABLE:
            # Connection-level failures only — the database is down, failing over, or
            # the pool could not reach it in time. The agent can act on that by waiting
            # and asking again, which it cannot do with a transport failure. Deliberately
            # not psycopg.Error: a constraint violation or a bad statement is a defect
            # here, and turning those into a polite retry would bury them.
            return _failed(
                "the knowledge store is temporarily unavailable; try again shortly"
            )
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
