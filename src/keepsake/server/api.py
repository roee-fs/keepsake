"""The read-only JSON API the admin console calls, mounted at /api.

Every route but `POST /api/session` inherits the session guard from its router,
so a route added later cannot ship unguarded. `tenant` is a query parameter here,
and never on /mcp, where an agent names no scope of its own. `search`, `grep`,
`graph` and the single-concept read require it, because a path is unique only within a
tenant. The rest default to `None`, which the store routes to `admin_scope()`.
"""

from collections.abc import AsyncIterator
from dataclasses import asdict
from datetime import date, datetime
from typing import Annotated, Any
from uuid import UUID

from anyio import CapacityLimiter
from fastapi import APIRouter, Depends, FastAPI, HTTPException, Query, Request, Response
from fastapi.openapi.docs import get_swagger_ui_html
from fastapi.responses import HTMLResponse
from mcp.server.transport_security import RequestBodyLimitMiddleware
from pydantic import BaseModel

from keepsake.server.auth import COOKIE_NAME, Auth, require_session
from keepsake.store.concepts import ConceptStore

# Long enough to outlast a port-forward session, short enough to bound a leaked cookie.
_SESSION_TTL = 12 * 60 * 60

# The only body is a login. Uncapped, one unauthenticated POST is buffered whole.
_MAX_BODY = 64 * 1024

# concept-detail takes no `limit` of its own; this bounds its revision history.
_HISTORY_LIMIT = 50

# Past a few hundred nodes a force layout is unreadable anyway.
_GRAPH_LIMIT = 500
# Separate from the node cap: one concept can link to thousands of targets.
_GRAPH_EDGE_LIMIT = 5000


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
    tenant_id: UUID


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


class GraphNode(BaseModel):
    path: str
    type: str
    title: str
    # A link target no concept holds: named by an agent, never written.
    missing: bool = False


class Graph(BaseModel):
    nodes: list[GraphNode]
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


# Unguarded: this router establishes the session every other route depends on.
public = APIRouter()


@public.post("/session", status_code=204)
def login(credentials: Credentials, request: Request, response: Response) -> None:
    auth: Auth = request.app.state.auth
    if not auth.check_password(credentials.password):
        raise HTTPException(status_code=401)
    _set_session_cookie(response, auth.issue(_SESSION_TTL), request)


async def _console_slot(request: Request) -> AsyncIterator[None]:
    """Hold the console's one slot on the pool the MCP tools draw from."""
    async with request.app.state.console_slot:
        yield


# After require_session, so a request refused with a 401 never queues for the slot.
guarded = APIRouter(dependencies=[Depends(require_session), Depends(_console_slot)])


@guarded.delete("/session", status_code=204)
def logout(response: Response) -> None:
    response.delete_cookie(COOKIE_NAME)


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


@guarded.get("/graph")
def graph(store: _Store, tenant: UUID) -> Graph:
    rows = store.graph(tenant, _GRAPH_LIMIT + 1)
    truncated = len(rows) > _GRAPH_LIMIT
    rows = rows[:_GRAPH_LIMIT]
    nodes = {
        path: GraphNode(path=path, type=t, title=title) for path, t, title, _ in rows
    }
    # Past the row cap, an absent target may just be a concept that did not fit.
    rows_cut = truncated
    edges = []
    for path, _, _, links in rows:
        for target in links:
            if len(edges) >= _GRAPH_EDGE_LIMIT:
                truncated = True
                break
            if target not in nodes:
                if rows_cut:
                    continue
                if len(nodes) >= _GRAPH_LIMIT:
                    truncated = True
                    continue
                nodes[target] = GraphNode(path=target, type="", title="", missing=True)
            edges.append((path, target))
    return Graph(nodes=list(nodes.values()), edges=edges, truncated=truncated)


@guarded.get("/activity")
def activity(
    store: _Store,
    tenant: UUID | None = None,
    limit: Annotated[int, Query(ge=1, le=200)] = 50,
) -> list[RevisionOut]:
    return [RevisionOut(**asdict(r)) for r in store.activity(tenant, limit)]


def create_api(concepts: ConceptStore, auth: Auth) -> FastAPI:
    """Build the FastAPI app to mount at /api.

    FastAPI's own docs/redoc/openapi routes ship unguarded, so they are disabled
    and `openapi_schema`/`docs` above serve the same content behind the guard.
    """
    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
    app.state.concepts = concepts
    app.state.auth = auth
    # One: the tools' limiter assumes it owns the pool, and a slow console read must
    # not hold the connections an agent is waiting on.
    app.state.console_slot = CapacityLimiter(1)
    app.add_middleware(RequestBodyLimitMiddleware, max_body_size=_MAX_BODY)
    app.include_router(public)
    app.include_router(guarded)
    return app
