"""The read-only JSON API the admin console calls.

Every route but `POST /api/session` requires a session cookie, and the test that
checks that drives itself from the mounted app's own route table so a route added
later stays covered without anyone remembering to list it here.
"""

import re
import uuid
from collections.abc import Iterator
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
    for route in _leaf_routes(app):
        for method in route.methods:
            if (route.path, method) == ("/session", "POST"):
                continue
            response = client.request(method, "/api" + _concrete_path(route.path))
            assert response.status_code == 401, f"{method} {route.path} was not guarded"


def test_a_bad_password_is_rejected_and_sets_no_cookie(client: TestClient) -> None:
    response = client.post("/api/session", json={"password": "wrong"})
    assert response.status_code == 401
    assert COOKIE_NAME not in response.cookies


def test_a_good_password_sets_an_httponly_strict_cookie(client: TestClient) -> None:
    response = client.post("/api/session", json={"password": PASSWORD})
    assert response.status_code == 204
    cookie_header = response.headers["set-cookie"]
    assert COOKIE_NAME in response.cookies
    assert "HttpOnly" in cookie_header
    assert "samesite=strict" in cookie_header.lower()
    # Plain HTTP in tests, so no Secure attribute.
    assert "secure" not in cookie_header.lower()


def test_logout_clears_the_cookie(logged_in: TestClient) -> None:
    response = logged_in.delete("/api/session")
    assert response.status_code == 204
    assert logged_in.cookies.get(COOKIE_NAME) is None


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


def test_graph_lists_the_seeded_node(logged_in: TestClient, seeded: uuid.UUID) -> None:
    response = logged_in.get("/api/graph", params={"tenant": str(seeded)})
    assert response.status_code == 200
    body = response.json()
    assert body["nodes"][0]["path"] == "detect/dormant"
    assert body["truncated"] is False


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
