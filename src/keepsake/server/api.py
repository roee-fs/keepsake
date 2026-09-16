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

from fastapi import APIRouter, Depends, FastAPI, HTTPException, Request, Response
from pydantic import BaseModel

from keepsake.server.auth import COOKIE_NAME, Auth, require_session
from keepsake.store.concepts import ConceptStore

# Long enough to outlast a port-forward session; short enough that a leaked cookie
# does not stay valid indefinitely.
_SESSION_TTL = 12 * 60 * 60

# concept-detail has no `limit` of its own; this bounds how many of one
# concept's own revisions come back.
_HISTORY_LIMIT = 50


class _Credentials(BaseModel):
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
def login(credentials: _Credentials, request: Request, response: Response) -> None:
    auth: Auth = request.app.state.auth
    if not auth.check_password(credentials.password):
        raise HTTPException(status_code=401)
    _set_session_cookie(response, auth.issue(_SESSION_TTL), request)


# Every other route lives here, so the guard cannot be forgotten per-route.
guarded = APIRouter(dependencies=[Depends(require_session)])


@guarded.delete("/session", status_code=204)
def logout(response: Response) -> None:
    response.delete_cookie(COOKIE_NAME)


@guarded.get("/tenants")
def tenants(store: _Store) -> list[TenantCount]:
    return [TenantCount(tenant_id=t, concepts=n) for t, n in store.tenants()]


@guarded.get("/stats")
def stats(store: _Store, tenant: UUID | None = None) -> TotalsOut:
    return TotalsOut(**asdict(store.totals(tenant)))


@guarded.get("/stats/timeseries")
def stats_timeseries(
    store: _Store, tenant: UUID | None = None, days: int = 30
) -> list[DailyWrite]:
    return [DailyWrite(date=d, count=n) for d, n in store.daily_writes(tenant, days)]


@guarded.get("/concepts")
def list_concepts(
    store: _Store,
    tenant: UUID | None = None,
    prefix: str = "",
    limit: int = 50,
    offset: int = 0,
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
def search(store: _Store, tenant: UUID, q: str, limit: int = 20) -> list[HitOut]:
    return [HitOut(**asdict(h)) for h in store.search(tenant, q, limit, prefix=None)]


@guarded.get("/grep")
def grep(store: _Store, tenant: UUID, pattern: str, limit: int = 20) -> list[GrepHit]:
    try:
        rows = store.grep(tenant, pattern, limit)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return [GrepHit(path=path, snippet=snippet) for path, snippet in rows]


@guarded.get("/activity")
def activity(
    store: _Store, tenant: UUID | None = None, limit: int = 50
) -> list[RevisionOut]:
    return [RevisionOut(**asdict(r)) for r in store.activity(tenant, limit)]


@guarded.get("/graph")
def graph(
    store: _Store,
    tenant: UUID | None = None,
    prefix: str = "",
    limit: int = 500,
) -> GraphOut:
    g = store.graph(tenant, prefix, limit)
    return GraphOut(
        nodes=[NodeOut(**asdict(n)) for n in g.nodes],
        edges=g.edges,
        truncated=g.truncated,
    )


def create_api(concepts: ConceptStore, auth: Auth) -> FastAPI:
    """The FastAPI app to mount at /api. `app.openapi()` is what Task 7 dumps —
    the docs/redoc/openapi HTTP routes are disabled so every reachable route in
    the table above is covered by the session guard."""
    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
    app.state.concepts = concepts
    app.state.auth = auth
    app.include_router(public)
    app.include_router(guarded)
    return app
