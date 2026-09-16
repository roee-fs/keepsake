"""The read-only JSON API the admin console calls, mounted at /api.

Every route but `POST /api/session` carries the session guard as a router-level
dependency rather than a per-route decorator, so a route added later cannot ship
unguarded. `tenant` is a query parameter here — never on /mcp, where an agent names
no scope identifier of its own. `search`, `grep` and the single-concept read take a
required `tenant`, matching the store methods they call: a path is only unique
within a tenant, so "every tenant" is not a meaningful scope for them. The rest
default to `None`, which the store already routes to `admin_scope()`.
"""

from dataclasses import asdict
from datetime import date, datetime
from typing import Annotated, Any
from uuid import UUID

from fastapi import APIRouter, Depends, FastAPI, HTTPException, Query, Request, Response
from fastapi.openapi.docs import get_swagger_ui_html
from fastapi.responses import HTMLResponse
from pydantic import BaseModel

from keepsake.server.auth import COOKIE_NAME, Auth, require_session
from keepsake.store.concepts import ConceptStore

# Long enough to outlast a port-forward session; short enough that a leaked cookie
# does not stay valid indefinitely.
_SESSION_TTL = 12 * 60 * 60

# concept-detail has no `limit` of its own; this bounds how many of one
# concept's own revisions come back.
_HISTORY_LIMIT = 50


class Credentials(BaseModel):
    password: str


class TenantCount(BaseModel):
    tenant_id: UUID
    concepts: int


class SummaryOut(BaseModel):
    path: str
    type: str
    title: str
    description: str
    version: int
    updated_at: datetime
    tenant_id: UUID


class ConceptPage(BaseModel):
    items: list[SummaryOut]
    total: int


class TotalsOut(BaseModel):
    concepts: int
    by_type: dict[str, int]
    revisions: int
    links: int
    orphans: int


class RevisionOut(BaseModel):
    path: str
    version: int
    op: str
    updated_by: str
    created_at: datetime


class ConceptDetail(BaseModel):
    path: str
    type: str
    title: str
    description: str
    body: str
    frontmatter: dict[str, Any]
    links: tuple[str, ...]
    version: int
    backlinks: list[str]
    revisions: list[RevisionOut]


class HitOut(BaseModel):
    path: str
    type: str
    title: str
    description: str
    score: float


class GrepHit(BaseModel):
    path: str
    snippet: str


class NodeOut(BaseModel):
    path: str
    type: str
    title: str
    exists: bool


class GraphOut(BaseModel):
    nodes: list[NodeOut]
    edges: list[tuple[str, str]]
    truncated: bool


class DailyWrite(BaseModel):
    date: date
    count: int


def _store(request: Request) -> ConceptStore:
    return request.app.state.concepts


_Store = Annotated[ConceptStore, Depends(_store)]


def _set_session_cookie(response: Response, cookie: str, request: Request) -> None:
    response.set_cookie(
        COOKIE_NAME,
        cookie,
        max_age=_SESSION_TTL,
        httponly=True,
        samesite="strict",
        # A hard-coded True would break the port-forward flow this console exists for.
        secure=request.url.scheme == "https",
    )


# Unguarded: this is the one route that establishes the session the rest depend on.
public = APIRouter()


@public.post("/session", status_code=204)
def login(credentials: Credentials, request: Request, response: Response) -> None:
    auth: Auth = request.app.state.auth
    if not auth.check_password(credentials.password):
        raise HTTPException(status_code=401)
    _set_session_cookie(response, auth.issue(_SESSION_TTL), request)


# Every other route lives here, so the guard cannot be forgotten per-route.
guarded = APIRouter(dependencies=[Depends(require_session)])


@guarded.delete("/session", status_code=204)
def logout(response: Response) -> None:
    response.delete_cookie(COOKIE_NAME)


# The brief's Produces line names /api/openapi.json; docs_url=None only turned off
# FastAPI's own unguarded copies, so the schema still needs a guarded route of its own.
@guarded.get("/openapi.json")
def openapi_schema(request: Request) -> dict[str, Any]:
    return request.app.openapi()


@guarded.get("/docs", include_in_schema=False)
def docs() -> HTMLResponse:
    return get_swagger_ui_html(openapi_url="openapi.json", title="keepsake API")


@guarded.get("/tenants")
def tenants(store: _Store) -> list[TenantCount]:
    return [TenantCount(tenant_id=t, concepts=n) for t, n in store.tenants()]


@guarded.get("/stats")
def stats(store: _Store, tenant: UUID | None = None) -> TotalsOut:
    return TotalsOut(**asdict(store.totals(tenant)))


@guarded.get("/stats/timeseries")
def stats_timeseries(
    store: _Store,
    tenant: UUID | None = None,
    days: Annotated[int, Query(ge=1, le=365)] = 30,
) -> list[DailyWrite]:
    return [DailyWrite(date=d, count=n) for d, n in store.daily_writes(tenant, days)]


@guarded.get("/concepts")
def list_concepts(
    store: _Store,
    tenant: UUID | None = None,
    prefix: str = "",
    limit: Annotated[int, Query(ge=1, le=200)] = 50,
    offset: Annotated[int, Query(ge=0)] = 0,
) -> ConceptPage:
    items = store.page(tenant, prefix, limit, offset)
    total = store.count(tenant, prefix)
    return ConceptPage(items=[SummaryOut(**asdict(s)) for s in items], total=total)


@guarded.get("/concepts/{path:path}")
def concept_detail(path: str, tenant: UUID, store: _Store) -> ConceptDetail:
    found = store.read_with_backlinks(tenant, path)
    if found is None:
        raise HTTPException(status_code=404)
    concept, backlinks = found
    history = store.revisions_for(tenant, path, _HISTORY_LIMIT)
    return ConceptDetail(
        **asdict(concept),
        backlinks=backlinks,
        revisions=[RevisionOut(**asdict(r)) for r in history],
    )


@guarded.get("/search")
def search(
    store: _Store, tenant: UUID, q: str, limit: Annotated[int, Query(ge=1, le=100)] = 20
) -> list[HitOut]:
    return [HitOut(**asdict(h)) for h in store.search(tenant, q, limit, prefix=None)]


@guarded.get("/grep")
def grep(
    store: _Store,
    tenant: UUID,
    pattern: str,
    limit: Annotated[int, Query(ge=1, le=100)] = 20,
) -> list[GrepHit]:
    try:
        rows = store.grep(tenant, pattern, limit)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return [GrepHit(path=path, snippet=snippet) for path, snippet in rows]


@guarded.get("/activity")
def activity(
    store: _Store,
    tenant: UUID | None = None,
    limit: Annotated[int, Query(ge=1, le=200)] = 50,
) -> list[RevisionOut]:
    return [RevisionOut(**asdict(r)) for r in store.activity(tenant, limit)]


@guarded.get("/graph")
def graph(
    store: _Store,
    tenant: UUID | None = None,
    prefix: str = "",
    limit: Annotated[int, Query(ge=1, le=2000)] = 500,
) -> GraphOut:
    g = store.graph(tenant, prefix, limit)
    return GraphOut(
        nodes=[NodeOut(**asdict(n)) for n in g.nodes],
        edges=g.edges,
        truncated=g.truncated,
    )


def create_api(concepts: ConceptStore, auth: Auth) -> FastAPI:
    """The FastAPI app to mount at /api. FastAPI's own docs/redoc/openapi routes
    are disabled since they ship unguarded; `openapi_schema`/`docs` above serve
    the same content behind the session guard instead."""
    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
    app.state.concepts = concepts
    app.state.auth = auth
    app.include_router(public)
    app.include_router(guarded)
    return app
