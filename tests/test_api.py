"""The read-only JSON API the admin console calls.

Every route but `POST /api/session` requires a session cookie. The test that checks
that drives itself from the mounted app's own route table, so a route added later
stays covered.
"""

import re
import uuid
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import Any

import pytest
from starlette.applications import Starlette
from starlette.routing import Mount
from starlette.testclient import TestClient

from keepsake.server.app import Config, build_app
from keepsake.server.auth import COOKIE_NAME
from keepsake.store.concepts import ConceptStore
from okf_core import Concept

PASSWORD = "test-admin-password"  # Matches the conftest `_admin_password` default.


def _api_app(app: Starlette) -> Any:
    """The FastAPI sub-app mounted at /api."""
    mount = next(r for r in app.routes if isinstance(r, Mount) and r.path == "/api")
    return mount.app


def _concrete_path(template: str) -> str:
    """A route template with any `{param}` replaced by a literal segment."""
    return re.sub(r"\{[^}]+\}", "x", template)


@pytest.fixture
def app(migrated: bool, pg_dsn: str) -> Iterator[Starlette]:
    built = build_app(Config(dsn=pg_dsn, tenant_id=uuid.uuid4()))
    yield built
    built.state.store.close()


@pytest.fixture
def client(app: Starlette) -> Iterator[TestClient]:
    with TestClient(app) as c:
        yield c


@pytest.fixture
def logged_in(client: TestClient) -> TestClient:
    response = client.post("/api/session", json={"password": PASSWORD})
    assert response.status_code == 204
    return client


@pytest.fixture
def seeded(app: Starlette, tenant: uuid.UUID) -> uuid.UUID:
    """A tenant with one concept, seeded through the app's own store."""
    concepts = ConceptStore(app.state.store)
    concepts.create(
        tenant,
        Concept(path="detect/dormant", type="Concept", title="Dormant Rule"),
        "test",
    )
    return tenant


@pytest.fixture
def other_tenant(app: Starlette) -> uuid.UUID:
    """A second tenant, so an admin-scope read is distinguishable from re-reading
    `seeded` alone."""
    tid = uuid.uuid4()
    concepts = ConceptStore(app.state.store)
    concepts.create(
        tid, Concept(path="other/thing", type="Concept", title="Other"), "test"
    )
    return tid


def _leaf_routes(app: Starlette) -> Iterator[Any]:
    """Flattens FastAPI's lazily-wrapped `_IncludedRouter` entries to their routes."""
    for route in _api_app(app).routes:
        yield from (
            route.effective_candidates()
            if hasattr(route, "effective_candidates")
            else [route]
        )


def test_every_route_except_login_requires_a_session(
    client: TestClient, app: Starlette
) -> None:
    routes = list(_leaf_routes(app))
    # A FastAPI change to `app.routes`'s shape could make this iterate zero routes
    # and pass vacuously; the floor catches that.
    assert len(routes) >= 10
    for route in routes:
        for method in route.methods:
            if (route.path, method) == ("/session", "POST"):
                continue
            response = client.request(method, "/api" + _concrete_path(route.path))
            assert response.status_code == 401, f"{method} {route.path} was not guarded"


def test_a_bad_password_is_rejected_and_sets_no_cookie(client: TestClient) -> None:
    response = client.post("/api/session", json={"password": "wrong"})
    assert response.status_code == 401
    assert COOKIE_NAME not in response.cookies


def test_an_oversized_login_body_is_refused_before_it_is_read(
    client: TestClient,
) -> None:
    """The one unauthenticated route MUST NOT buffer whatever a caller sends."""
    response = client.post("/api/session", json={"password": "x" * 100_000})
    assert response.status_code == 413


def test_a_good_password_sets_an_httponly_strict_cookie(client: TestClient) -> None:
    response = client.post("/api/session", json={"password": PASSWORD})
    assert response.status_code == 204
    cookie_header = response.headers["set-cookie"]
    assert COOKIE_NAME in response.cookies
    assert "HttpOnly" in cookie_header
    assert "samesite=strict" in cookie_header.lower()
    # Plain HTTP in tests, so no Secure attribute.
    assert "secure" not in cookie_header.lower()


def test_a_good_password_sets_secure_over_tls(app: Starlette) -> None:
    with TestClient(app, base_url="https://testserver") as https_client:
        response = https_client.post("/api/session", json={"password": PASSWORD})
    assert response.status_code == 204
    assert "secure" in response.headers["set-cookie"].lower()


def test_logout_clears_the_cookie(logged_in: TestClient) -> None:
    response = logged_in.delete("/api/session")
    assert response.status_code == 204
    assert logged_in.cookies.get(COOKIE_NAME) is None


def test_openapi_schema_is_served_behind_the_session_guard(
    logged_in: TestClient,
) -> None:
    response = logged_in.get("/api/openapi.json")
    assert response.status_code == 200
    assert "openapi" in response.json()


def test_docs_ui_is_served_behind_the_session_guard(logged_in: TestClient) -> None:
    response = logged_in.get("/api/docs")
    assert response.status_code == 200
    assert "text/html" in response.headers["content-type"]


def test_mcp_refuses_a_browser_origin(client: TestClient) -> None:
    """A DNS-rebound page reaches /mcp as same-origin; only its Origin header shows."""
    response = client.post(
        "/mcp",
        json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
        headers={"Accept": "application/json", "Origin": "http://evil.example:8000"},
    )
    assert response.status_code == 403


def test_tenants_lists_every_tenant_with_a_concept(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get("/api/tenants")
    assert response.status_code == 200
    body = response.json()
    assert {"tenant_id": str(seeded), "concepts": 1} in body


def test_stats_totals_an_absent_tenant_covers_every_tenant(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get("/api/stats")
    assert response.status_code == 200
    assert response.json()["concepts"] >= 1


def test_stats_scoped_to_one_tenant(logged_in: TestClient, seeded: uuid.UUID) -> None:
    response = logged_in.get("/api/stats", params={"tenant": str(seeded)})
    assert response.status_code == 200
    assert response.json()["concepts"] == 1


def test_stats_rejects_a_malformed_tenant(logged_in: TestClient) -> None:
    response = logged_in.get("/api/stats", params={"tenant": "not-a-uuid"})
    assert response.status_code == 422


def test_stats_timeseries_returns_a_thirty_day_default(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get("/api/stats/timeseries", params={"tenant": str(seeded)})
    assert response.status_code == 200
    assert len(response.json()) == 30


def test_concepts_page_lists_a_seeded_concept(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get("/api/concepts", params={"tenant": str(seeded)})
    assert response.status_code == 200
    body = response.json()
    assert body["total"] == 1
    assert body["items"][0]["path"] == "detect/dormant"


def test_concepts_page_rejects_a_negative_offset(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get(
        "/api/concepts", params={"tenant": str(seeded), "offset": -1}
    )
    assert response.status_code == 422


def test_concepts_page_rejects_a_non_positive_limit(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get(
        "/api/concepts", params={"tenant": str(seeded), "limit": -1}
    )
    assert response.status_code == 422


def test_stats_timeseries_rejects_an_unbounded_days(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get(
        "/api/stats/timeseries", params={"tenant": str(seeded), "days": 1_000_000}
    )
    assert response.status_code == 422


def test_concept_detail_carries_backlinks_and_history(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get(
        "/api/concepts/detect/dormant", params={"tenant": str(seeded)}
    )
    assert response.status_code == 200
    body = response.json()
    assert body["path"] == "detect/dormant"
    assert body["backlinks"] == []
    assert len(body["revisions"]) == 1
    assert body["revisions"][0]["op"] == "create"


def test_concept_detail_missing_path_is_404(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get("/api/concepts/no/such", params={"tenant": str(seeded)})
    assert response.status_code == 404


def test_concept_detail_requires_a_tenant(logged_in: TestClient) -> None:
    response = logged_in.get("/api/concepts/detect/dormant")
    assert response.status_code == 422


def test_search_finds_a_seeded_concept(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get(
        "/api/search", params={"tenant": str(seeded), "q": "dormant"}
    )
    assert response.status_code == 200
    assert response.json()[0]["path"] == "detect/dormant"


def test_grep_finds_a_seeded_concept(logged_in: TestClient, seeded: uuid.UUID) -> None:
    response = logged_in.get(
        "/api/grep", params={"tenant": str(seeded), "pattern": "Dormant"}
    )
    assert response.status_code == 200
    assert response.json()[0]["path"] == "detect/dormant"


def test_grep_rejects_an_unusable_pattern(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get(
        "/api/grep", params={"tenant": str(seeded), "pattern": "("}
    )
    assert response.status_code == 400


def test_activity_lists_the_seeded_revision(
    logged_in: TestClient, seeded: uuid.UUID
) -> None:
    response = logged_in.get("/api/activity", params={"tenant": str(seeded)})
    assert response.status_code == 200
    assert response.json()[0]["path"] == "detect/dormant"


@pytest.mark.parametrize(
    ("path", "extract"),
    [
        ("/api/concepts", lambda body: {i["path"] for i in body["items"]}),
        ("/api/activity", lambda body: {r["path"] for r in body}),
    ],
)
def test_admin_scope_mixes_every_tenant_when_none_is_named(
    logged_in: TestClient,
    seeded: uuid.UUID,
    other_tenant: uuid.UUID,
    path: str,
    extract: Callable[[Any], set[str]],
) -> None:
    response = logged_in.get(path)
    assert response.status_code == 200
    assert {"detect/dormant", "other/thing"} <= extract(response.json())


def test_stats_timeseries_admin_scope_mixes_every_tenant(
    logged_in: TestClient, seeded: uuid.UUID, other_tenant: uuid.UUID
) -> None:
    scoped = logged_in.get(
        "/api/stats/timeseries", params={"tenant": str(seeded)}
    ).json()
    admin = logged_in.get("/api/stats/timeseries").json()
    # Both tenants wrote today; a tenant-scoped call only sees its own write.
    assert sum(d["count"] for d in admin) > sum(d["count"] for d in scoped)


def test_ui_disabled_mounts_no_api_but_still_serves_mcp_and_readyz(
    monkeypatch: pytest.MonkeyPatch, migrated: bool, pg_dsn: str
) -> None:
    monkeypatch.setenv("KEEPSAKE_UI", "false")
    built = build_app(Config(dsn=pg_dsn, tenant_id=uuid.uuid4()))
    try:
        assert not any(isinstance(r, Mount) and r.path == "/api" for r in built.routes)
        assert "/mcp" in [getattr(r, "path", None) for r in built.routes]
        with TestClient(built) as c:
            response = c.get("/readyz")
            assert response.status_code == 200
            assert response.json() == {"ready": True}
    finally:
        built.state.store.close()


def test_missing_static_bundle_skips_the_mount_but_api_and_mcp_still_serve(
    monkeypatch: pytest.MonkeyPatch, migrated: bool, pg_dsn: str, tmp_path: Path
) -> None:
    """A source checkout has no built bundle; the API MUST NOT depend on one."""
    monkeypatch.setenv("KEEPSAKE_STATIC_DIR", str(tmp_path / "no-such-dir"))
    built = build_app(Config(dsn=pg_dsn, tenant_id=uuid.uuid4()))
    try:
        assert not any(isinstance(r, Mount) and r.path == "" for r in built.routes)
        assert any(isinstance(r, Mount) and r.path == "/api" for r in built.routes)
        assert "/mcp" in [getattr(r, "path", None) for r in built.routes]
        with TestClient(built) as c:
            assert c.get("/readyz").status_code == 200
            login = c.post("/api/session", json={"password": PASSWORD})
            assert login.status_code == 204
    finally:
        built.state.store.close()


def test_static_bundle_present_serves_the_console_last_with_spa_fallback(
    monkeypatch: pytest.MonkeyPatch, migrated: bool, pg_dsn: str, tmp_path: Path
) -> None:
    """The catch-all must not shadow /api or /mcp, and a deep link must not 404."""
    static_dir = tmp_path / "static"
    static_dir.mkdir()
    (static_dir / "index.html").write_text("<html>console shell</html>")
    monkeypatch.setenv("KEEPSAKE_STATIC_DIR", str(static_dir))
    built = build_app(Config(dsn=pg_dsn, tenant_id=uuid.uuid4()))
    try:
        last = built.routes[-1]
        assert isinstance(last, Mount)
        assert last.path == ""
        with TestClient(built) as c:
            index = c.get("/")
            assert index.status_code == 200
            assert index.text == "<html>console shell</html>"
            # A client-side route, not a real file: it must render the shell, or a
            # deep-link reload breaks.
            deep_link = c.get("/concepts/notes%2Fa.md")
            assert deep_link.status_code == 200
            assert deep_link.text == "<html>console shell</html>"
            assert c.get("/readyz").status_code == 200
            login = c.post("/api/session", json={"password": PASSWORD})
            assert login.status_code == 204
            # Route presence doesn't prove dispatch order, so the request has to be
            # made. POST, not GET: GET opens the transport's SSE stream, which stays
            # open forever and would hang this in-process client. No handshake header,
            # matching what a Service sends (test_tools.py's
            # test_the_endpoint_answers_a_plain_json_post). The assertions pin no
            # status, only that the catch-all did not answer.
            mcp_response = c.post(
                "/mcp",
                json={"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
                headers={"Accept": "application/json"},
            )
            assert mcp_response.text != "<html>console shell</html>"
            content_type = mcp_response.headers.get("content-type", "")
            assert not content_type.startswith("text/html")
    finally:
        built.state.store.close()


def _link(app: Starlette, tenant: uuid.UUID, path: str, *links: str) -> None:
    ConceptStore(app.state.store).create(
        tenant, Concept(path=path, type="Concept", title=path, links=links), "test"
    )


def test_graph_returns_edges_and_marks_a_missing_target(
    app: Starlette, logged_in: TestClient, tenant: uuid.UUID, other_tenant: uuid.UUID
) -> None:
    _link(app, tenant, "a", "b", "ghost")
    _link(app, tenant, "b", "a")

    response = logged_in.get("/api/graph", params={"tenant": str(tenant)})
    assert response.status_code == 200
    body = response.json()
    nodes = {n["path"]: n["missing"] for n in body["nodes"]}
    # other/thing belongs to other_tenant and MUST NOT appear.
    assert nodes == {"a": False, "b": False, "ghost": True}
    assert sorted(map(tuple, body["edges"])) == [("a", "b"), ("a", "ghost"), ("b", "a")]
    assert body["truncated"] is False


def test_graph_past_the_cap_drops_edges_it_cannot_place(
    app: Starlette,
    logged_in: TestClient,
    tenant: uuid.UUID,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr("keepsake.server.api._GRAPH_LIMIT", 1)
    _link(app, tenant, "a", "b")
    _link(app, tenant, "b")

    body = logged_in.get("/api/graph", params={"tenant": str(tenant)}).json()
    # "b" exists past the cap, so it MUST NOT come back as a missing node.
    assert [n["path"] for n in body["nodes"]] == ["a"]
    assert body["edges"] == []
    assert body["truncated"] is True


def test_graph_caps_missing_targets_at_the_node_limit(
    app: Starlette,
    logged_in: TestClient,
    tenant: uuid.UUID,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """One concept can name thousands of targets nobody wrote."""
    monkeypatch.setattr("keepsake.server.api._GRAPH_LIMIT", 3)
    _link(app, tenant, "a", "g1", "g2", "g3", "g4")

    body = logged_in.get("/api/graph", params={"tenant": str(tenant)}).json()
    assert [n["path"] for n in body["nodes"]] == ["a", "g1", "g2"]
    assert body["edges"] == [["a", "g1"], ["a", "g2"]]
    assert body["truncated"] is True


def test_graph_caps_edges(
    app: Starlette,
    logged_in: TestClient,
    tenant: uuid.UUID,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr("keepsake.server.api._GRAPH_EDGE_LIMIT", 2)
    _link(app, tenant, "a", "b", "c")
    _link(app, tenant, "b", "a", "c")
    _link(app, tenant, "c")

    body = logged_in.get("/api/graph", params={"tenant": str(tenant)}).json()
    assert len(body["edges"]) == 2
    assert body["truncated"] is True


def test_graph_requires_a_tenant(logged_in: TestClient) -> None:
    assert logged_in.get("/api/graph").status_code == 422
