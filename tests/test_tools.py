"""The seven tools an agent sees, and the MCP surface they are advertised on.

The tool surface is the security boundary: no tool names a tenant, so an agent
cannot name the wrong one. These tests hold that line and the bodies-never-leak
line, and they read the advertised schema through a real client session rather
than through Python signatures.
"""

import json
import threading
import time
import urllib.request
import uuid
from collections.abc import AsyncIterator, Iterator
from contextlib import asynccontextmanager
from functools import partial
from typing import Any

import anyio
import pytest
import uvicorn
from mcp.client.session import ClientSession
from mcp.server.lowlevel import Server
from mcp.shared.exceptions import MCPError
from mcp.shared.memory import create_client_server_memory_streams
from starlette.applications import Starlette

from keepsake.server.app import Config, build_app
from keepsake.server.tools import ToolError, Tools, register
from keepsake.store.concepts import ConceptStore
from keepsake.store.verify import MisconfiguredDatabase
from okf_core import Concept

TOOL_NAMES = {
    "okf_list",
    "okf_search",
    "okf_grep",
    "okf_read",
    "okf_create",
    "okf_update",
    "okf_relate",
}

# Distinctive enough that its appearance anywhere is proof, not coincidence.
SECRET = "zqxjkbody"


@asynccontextmanager
async def _session(
    tools: Tools, raise_exceptions: bool = True
) -> AsyncIterator[ClientSession]:
    """A client talking to the registered tools over an in-memory MCP transport.

    `raise_exceptions=False` lets the runner answer an unhandled handler exception with
    a protocol error, which is the whole point of the test that asserts one.
    """
    server: Server[Any] = Server("keepsake-test")
    register(server, tools)
    async with (
        create_client_server_memory_streams() as (
            (client_read, client_write),
            (server_read, server_write),
        ),
        anyio.create_task_group() as tg,
    ):
        tg.start_soon(
            partial(
                server.run,
                server_read,
                server_write,
                server.create_initialization_options(),
                raise_exceptions=raise_exceptions,
            )
        )
        async with ClientSession(client_read, client_write) as session:
            await session.initialize()
            yield session
        tg.cancel_scope.cancel()


@pytest.fixture
def served(migrated: bool, pg_dsn: str) -> Iterator[str]:
    """The built app on a real socket, so the wire format is the one a pod serves."""
    app = build_app(Config(dsn=pg_dsn, tenant_id=uuid.uuid4()))
    server = uvicorn.Server(
        uvicorn.Config(app, host="127.0.0.1", port=0, log_level="warning")
    )
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    while not server.started:
        time.sleep(0.01)
    yield f"http://127.0.0.1:{server.servers[0].sockets[0].getsockname()[1]}/mcp"
    server.should_exit = True
    thread.join(timeout=5)
    app.state.store.close()


async def _seed(tools: Tools, path: str, **kw: Any) -> None:
    await tools.create(path=path, type=kw.pop("type", "Concept"), **kw)


@pytest.mark.asyncio
async def test_create_rejects_a_concept_without_type(tools: Tools) -> None:
    with pytest.raises(ToolError, match="type is required"):
        await tools.create(
            path="a/b", type="", title="x", description="", body="", links=[]
        )


@pytest.mark.asyncio
async def test_create_rejects_a_path_reserved_for_a_generated_bundle_file(
    tools: Tools,
) -> None:
    """There is no delete tool, so one concept at `index` would make the whole tenant
    un-exportable for good."""
    with pytest.raises(ToolError, match="reserved"):
        await _seed(tools, "index")
    await _seed(tools, "architecture/index")


@pytest.mark.asyncio
async def test_create_rejects_a_path_that_is_taken(tools: Tools) -> None:
    await _seed(tools, "a/b", body="first")
    with pytest.raises(ToolError, match="already exists"):
        await _seed(tools, "a/b", body="second")


@pytest.mark.asyncio
async def test_links_come_from_the_body_not_from_the_argument(tools: Tools) -> None:
    await _seed(tools, "a/b", body="see [y](/a/y.md)", links=["forged/edge"])
    concept = await tools.read(path="a/b")
    assert concept is not None
    assert concept["links"] == ["a/y"]


@pytest.mark.asyncio
async def test_update_conflict_returns_current_content(tools: Tools) -> None:
    await tools.create(
        path="a/c", type="Concept", title="t", description="", body="v1", links=[]
    )
    await tools.update(path="a/c", body="v2", expected_version=1)
    result = await tools.update(path="a/c", body="v3", expected_version=1)
    assert result["conflict"] is True
    assert result["current_version"] == 2
    assert result["current_body"] == "v2"


@pytest.mark.asyncio
async def test_update_keeps_the_fields_it_was_not_given(tools: Tools) -> None:
    await _seed(
        tools,
        "a/b",
        title="Original",
        description="D",
        body="v1",
        frontmatter={"owner": "sec"},
    )
    await tools.update(path="a/b", body="v2")
    concept = await tools.read(path="a/b")
    assert concept is not None
    assert (
        concept["title"],
        concept["description"],
        concept["body"],
        concept["frontmatter"],
    ) == ("Original", "D", "v2", {"owner": "sec"})


@pytest.mark.asyncio
async def test_frontmatter_that_is_not_an_object_is_correctable(tools: Tools) -> None:
    with pytest.raises(ToolError, match="frontmatter must be an object"):
        await _seed(tools, "a/b", frontmatter="owner: sec")


@pytest.mark.asyncio
async def test_a_null_frontmatter_is_refused_rather_than_erasing(tools: Tools) -> None:
    """`null` is the likeliest way an agent says "leave it alone"; it must not wipe."""
    await _seed(tools, "a/b", body="v1", frontmatter={"owner": "sec"})
    with pytest.raises(ToolError, match="frontmatter must be an object"):
        await tools.update(path="a/b", body="v2", frontmatter=None)
    concept = await tools.read(path="a/b")
    assert concept is not None
    assert concept["frontmatter"] == {"owner": "sec"}


@pytest.mark.asyncio
@pytest.mark.parametrize("field", ["body", "type", "title", "description"])
async def test_a_null_text_field_is_refused_rather_than_erasing(
    tools: Tools, field: str
) -> None:
    """`str(None)` stored the four characters `None` over what was there, and passed."""
    await _seed(tools, "a/b", title="T", description="D", body="v1")
    with pytest.raises(ToolError, match=f"{field} must be a string"):
        await tools.update(path="a/b", **{field: None})
    concept = await tools.read(path="a/b")
    assert concept is not None
    assert (
        concept["body"],
        concept["type"],
        concept["title"],
        concept["description"],
    ) == (
        "v1",
        "Concept",
        "T",
        "D",
    )


@pytest.mark.asyncio
async def test_update_of_another_tenants_path_is_indistinguishable_from_absent(
    tools: Tools, concepts: ConceptStore
) -> None:
    concepts.create(
        uuid.uuid4(), Concept(path="a/b", type="Concept", body=SECRET), "seed"
    )
    with pytest.raises(ToolError) as taken:
        await tools.update(path="a/b", body="mine")
    with pytest.raises(ToolError) as absent:
        await tools.update(path="nowhere/at/all", body="mine")
    assert SECRET not in str(taken.value)
    assert str(taken.value).replace("a/b", "X") == str(absent.value).replace(
        "nowhere/at/all", "X"
    )


@pytest.mark.asyncio
async def test_update_of_a_row_that_vanished_mid_write_is_not_found(
    tools: Tools, concepts: ConceptStore, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The store raises KeyError when the row goes away between the read and the write."""
    monkeypatch.setattr(
        concepts, "read", lambda t, path: Concept(path=path, type="Concept")
    )
    with pytest.raises(ToolError, match="^no concept at ghost/path$"):
        await tools.update(path="ghost/path", body="x")


@pytest.mark.asyncio
async def test_search_returns_cards_and_never_a_body(tools: Tools) -> None:
    await _seed(tools, "detect/dormant", title="Dormant Rule", body=SECRET)
    hits = await tools.search(query="dormant", limit=5)
    assert [h["path"] for h in hits] == ["detect/dormant"]
    assert SECRET not in json.dumps(hits)


@pytest.mark.asyncio
async def test_search_terms_are_ored_so_extra_terms_broaden(tools: Tools) -> None:
    await _seed(tools, "a/one", title="Dormant Rule")
    await _seed(tools, "a/two", title="Splunk Cursor")
    hits = await tools.search(query="dormant splunk", limit=5)
    assert {h["path"] for h in hits} == {"a/one", "a/two"}


@pytest.mark.asyncio
async def test_grep_returns_a_snippet_for_every_match(tools: Tools) -> None:
    await _seed(tools, "a/b", body=f"alpha {SECRET} omega")
    assert await tools.grep(pattern="al.ha", limit=5) == [
        {"path": "a/b", "snippet": f"alpha {SECRET} omega"}
    ]


@pytest.mark.asyncio
async def test_grep_reports_an_unusable_pattern_without_a_body(tools: Tools) -> None:
    await _seed(tools, "a/b", body=SECRET)
    with pytest.raises(ToolError) as exc:
        await tools.grep(pattern="alpha(", limit=5)
    assert SECRET not in str(exc.value)


@pytest.mark.asyncio
async def test_read_of_a_missing_path_is_none(tools: Tools) -> None:
    assert await tools.read(path="nothing/here") is None


@pytest.mark.asyncio
async def test_relate_records_an_edge_both_ways(tools: Tools) -> None:
    await _seed(tools, "a/x", body="start")
    await _seed(tools, "b/y", body="target")
    await tools.relate(from_path="a/x", to_path="b/y")
    source = await tools.read(path="a/x")
    target = await tools.read(path="b/y")
    assert source is not None and target is not None
    assert source["links"] == ["b/y"]
    assert target["backlinks"] == ["a/x"]


@pytest.mark.asyncio
async def test_relate_rejects_a_missing_source(tools: Tools) -> None:
    await _seed(tools, "b/y")
    with pytest.raises(ToolError, match="no concept at a/x"):
        await tools.relate(from_path="a/x", to_path="b/y")


@pytest.mark.asyncio
async def test_list_returns_paths_and_counts_by_type(tools: Tools) -> None:
    await _seed(tools, "a/one")
    await _seed(tools, "a/two", type="Runbook")
    await _seed(tools, "b/three")
    assert await tools.list_(prefix="a/") == {
        "paths": ["a/one", "a/two"],
        "counts": {"Concept": 1, "Runbook": 1},
    }


@pytest.mark.asyncio
async def test_the_server_advertises_exactly_the_seven_tools(tools: Tools) -> None:
    async with _session(tools) as session:
        listing = await session.list_tools()
    assert {t.name for t in listing.tools} == TOOL_NAMES


@pytest.mark.asyncio
async def test_search_and_grep_advertise_limit_as_required(tools: Tools) -> None:
    async with _session(tools) as session:
        listing = await session.list_tools()
    advertised = {t.name: t.input_schema for t in listing.tools}
    for name in ("okf_search", "okf_grep"):
        assert "limit" in advertised[name]["required"]


@pytest.mark.asyncio
async def test_search_and_grep_advertise_the_envelope_they_answer_in(
    tools: Tools,
) -> None:
    async with _session(tools) as session:
        listing = await session.list_tools()
    advertised = {t.name: t.output_schema for t in listing.tools}
    for name in ("okf_search", "okf_grep"):
        schema = advertised[name]
        assert schema is not None
        assert schema["properties"]["results"]["type"] == "array"


@pytest.mark.asyncio
async def test_every_tool_is_described_and_search_explains_its_matching(
    tools: Tools,
) -> None:
    async with _session(tools) as session:
        listing = await session.list_tools()
    described = {t.name: (t.description or "").lower() for t in listing.tools}
    assert all(described.values())
    assert "lexical" in described["okf_search"]
    assert "or-ed" in described["okf_search"]
    assert "distinctive" in described["okf_search"]


@pytest.mark.asyncio
async def test_a_tool_call_round_trips_over_the_protocol(tools: Tools) -> None:
    async with _session(tools) as session:
        await session.call_tool(
            "okf_create",
            {
                "path": "e2e/smoke",
                "type": "Concept",
                "title": "Smoke",
                "body": SECRET,
            },
        )
        result = await session.call_tool("okf_search", {"query": "smoke", "limit": 5})
        # call_tool validates a result against the tool's advertised output schema,
        # fetching the listing itself, so a shape contradicting it raises here.
        matched = await session.call_tool("okf_grep", {"pattern": "smoke", "limit": 5})
    assert result.is_error is False
    assert matched.is_error is False
    assert [h["path"] for h in result.structured_content["results"]] == ["e2e/smoke"]
    assert SECRET not in json.dumps(result.structured_content)
    assert SECRET not in json.dumps([c.model_dump() for c in result.content])


@pytest.mark.asyncio
async def test_a_rejected_call_is_an_error_result_not_a_protocol_error(
    tools: Tools,
) -> None:
    async with _session(tools) as session:
        result = await session.call_tool("okf_create", {"path": "a/b", "type": ""})
    assert result.is_error is True
    assert "type is required" in result.content[0].text  # ty: ignore[unresolved-attribute]


@pytest.mark.asyncio
async def test_a_call_missing_a_required_argument_is_an_error_result(
    tools: Tools,
) -> None:
    async with _session(tools) as session:
        result = await session.call_tool("okf_search", {"query": "smoke"})
    assert result.is_error is True


@pytest.mark.asyncio
async def test_an_internal_typeerror_is_not_dressed_up_as_the_agents_mistake(
    tools: Tools, concepts: ConceptStore, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A defect below the boundary must stay a defect: a tidy error result would send
    the agent to retry a request that was never wrong, and hide the bug."""

    def broken(*args: Any, **kw: Any) -> None:
        raise TypeError("a defect below the tool layer")

    monkeypatch.setattr(concepts, "list_", broken)
    async with _session(tools, raise_exceptions=False) as session:
        with pytest.raises(MCPError):
            await session.call_tool("okf_list", {})


@pytest.mark.asyncio
async def test_an_argument_of_the_wrong_shape_is_an_error_result(tools: Tools) -> None:
    """Plausible from an LLM, and a protocol error would give it nothing to act on."""
    async with _session(tools) as session:
        result = await session.call_tool(
            "okf_create",
            {"path": "a/b", "type": "Concept", "frontmatter": "owner: sec"},
        )
    assert result.is_error is True
    assert "frontmatter must be an object" in result.content[0].text  # ty: ignore[unresolved-attribute]


def test_the_endpoint_answers_a_plain_json_post(served: str) -> None:
    """No handshake, no session header, no SSE: what a Service in front of it sends."""
    request = urllib.request.Request(
        served,
        data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}).encode(),
        headers={"Content-Type": "application/json", "Accept": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        advertised = json.loads(response.read())["result"]["tools"]
    assert {t["name"] for t in advertised} == TOOL_NAMES


@pytest.mark.usefixtures("migrated")
def test_build_app_refuses_a_database_that_does_not_isolate(owner_dsn: str) -> None:
    with pytest.raises(MisconfiguredDatabase):
        build_app(Config(dsn=owner_dsn, tenant_id=uuid.uuid4()))


@pytest.mark.usefixtures("migrated")
def test_build_app_serves_mcp_on_an_isolating_database(pg_dsn: str) -> None:
    app = build_app(Config(dsn=pg_dsn, tenant_id=uuid.uuid4()))
    try:
        assert isinstance(app, Starlette)
        assert "/mcp" in [getattr(r, "path", None) for r in app.routes]
    finally:
        app.state.store.close()
