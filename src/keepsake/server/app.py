"""The HTTP entry point. `verify` runs here, before anything can bind a port."""

from __future__ import annotations

import logging
import os
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import Any
from uuid import UUID

from anyio import to_thread
from mcp.server.lowlevel import Server
from mcp.server.transport_security import TransportSecuritySettings
from starlette.applications import Starlette
from starlette.exceptions import HTTPException as StarletteHTTPException
from starlette.requests import Request
from starlette.responses import JSONResponse, Response
from starlette.staticfiles import PathLike, StaticFiles
from starlette.types import Scope

from keepsake.server.api import create_api
from keepsake.server.auth import Auth, admin_password, ui_enabled
from keepsake.server.tools import Tools, register
from keepsake.store import SCHEMA
from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store
from keepsake.store.verify import verify

logger = logging.getLogger(__name__)

# Every request is the one configured tenant, so the revision log records the server
# rather than a caller it has no way to identify.
_ACTOR = "mcp"

# Where the Dockerfile bakes the built console (`COPY --from=ui /ui/dist /app/static`).
# Overridable so a source checkout can point it at a local `frontend/dist`.
_DEFAULT_STATIC_DIR = "/app/static"


class _ConsoleStaticFiles(StaticFiles):
    """Serve the built console, falling back to index.html for unmatched paths.

    A client-side route reloaded as a deep link gets the SPA shell, not a 404.
    """

    async def get_response(self, path: str, scope: Scope) -> Response:
        try:
            return await super().get_response(path, scope)
        except StarletteHTTPException as exc:
            if exc.status_code != 404:
                raise
            # A stale hashed asset URL also 200s as the shell. That is the tradeoff.
            return await super().get_response("index.html", scope)

    def file_response(
        self,
        full_path: PathLike,
        stat_result: os.stat_result,
        scope: Scope,
        status_code: int = 200,
    ) -> Response:
        response = super().file_response(full_path, stat_result, scope, status_code)
        # The shell names this build's hashed assets, so a cached copy outlives an
        # upgrade and loads chunks the new image no longer has.
        if os.path.basename(full_path) == "index.html":
            response.headers["Cache-Control"] = "no-cache"
        return response


@dataclass(frozen=True, slots=True)
class Config:
    dsn: str
    tenant_id: UUID
    schema: str = SCHEMA


async def _readyz(request: Request) -> Response:
    """Whether this replica can serve, which is not the same as being up.

    A pool whose connections all died with the database stays bound to its port and
    keeps taking traffic, so a TCP readiness probe leaves every other agent waiting
    30s for a connection that is not coming. This takes the replica out of the
    Service until its pool has rebuilt.
    """
    store: Store = request.app.state.store
    ready = await to_thread.run_sync(store.healthy)
    return JSONResponse({"ready": ready}, status_code=200 if ready else 503)


def build_app(config: Config) -> Starlette:
    """The MCP app, served at /mcp, with a readiness probe at /readyz.

    Raises MisconfiguredDatabase when the database does not isolate tenants. That
    exception MUST reach the caller: crashing is the check.
    """
    # Read before the pool opens, so a misconfigured console fails before the
    # database is holding connections open.
    password = admin_password()

    store = Store(config.dsn, schema=config.schema)
    try:
        verify(store, config.schema)
    except BaseException:
        store.close()
        raise

    concepts = ConceptStore(store)
    server: Server[Any] = Server("keepsake")
    register(server, Tools(concepts, config.tenant_id, _ACTOR))
    app = server.streamable_http_app(
        streamable_http_path="/mcp",
        json_response=True,
        # A session would pin an agent to one replica; several sit behind one Service.
        stateless_http=True,
        # The Host header is a cluster Service name, and no browser can reach the pod,
        # so the localhost-only default would reject every real request. Restore it when
        # auth stops being `none`: this is a setting that outlives its justification.
        transport_security=TransportSecuritySettings(
            enable_dns_rebinding_protection=False
        ),
    )
    # The pool outlives any one request, so the app owns it.
    app.state.store = store
    app.router.add_route("/readyz", _readyz, methods=["GET"])
    if ui_enabled():
        app.mount("/api", create_api(concepts, Auth(password)))
        static_dir = os.environ.get("KEEPSAKE_STATIC_DIR", _DEFAULT_STATIC_DIR)
        if os.path.isdir(static_dir):
            # The router's fallback, not a Mount("/"), so /mcp/ still redirects to /mcp.
            # Unguarded because gating it would break the login page.
            app.router.default = _ConsoleStaticFiles(directory=static_dir, html=True)
        else:
            logger.info("no console bundle at %s; serving API and MCP only", static_dir)
    # Process exit covers this in a pod, but not in a test or an embedding host, where
    # a pool left open holds its connections until the interpreter goes.
    # Wrapped rather than passed in: the MCP app builds its own lifespan, which runs
    # the session manager, and replacing it would stop the server serving. Starlette
    # 1.x dropped add_event_handler, so this is the seam.
    inner = app.router.lifespan_context

    @asynccontextmanager
    async def _lifespan(scope: Starlette) -> AsyncIterator[None]:
        try:
            async with inner(scope):
                yield
        finally:
            # Process exit covers this in a pod, but not in a test or an embedding
            # host, where a pool left open holds its connections until the
            # interpreter goes.
            store.close()

    app.router.lifespan_context = _lifespan
    return app
