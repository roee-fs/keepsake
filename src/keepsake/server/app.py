"""The HTTP entry point. `verify` runs here, before anything can bind a port."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any
from uuid import UUID

from mcp.server.lowlevel import Server
from mcp.server.transport_security import TransportSecuritySettings
from starlette.applications import Starlette

from keepsake.server.tools import Tools, register
from keepsake.store import SCHEMA
from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store
from keepsake.store.verify import verify

# Every request is the one configured tenant, so the revision log records the server
# rather than a caller it has no way to identify.
_ACTOR = "mcp"


@dataclass(frozen=True, slots=True)
class Config:
    dsn: str
    tenant_id: UUID
    schema: str = SCHEMA


def build_app(config: Config) -> Starlette:
    """The MCP app, served at /mcp.

    Raises MisconfiguredDatabase when the database does not isolate tenants. That
    exception MUST reach the caller: crashing is the check.
    """
    store = Store(config.dsn, schema=config.schema)
    try:
        verify(store, config.schema)
    except BaseException:
        store.close()
        raise

    server: Server[Any] = Server("keepsake")
    register(server, Tools(ConceptStore(store), config.tenant_id, _ACTOR))
    app = server.streamable_http_app(
        streamable_http_path="/mcp",
        json_response=True,
        # A session would pin an agent to one replica; several sit behind one Service.
        stateless_http=True,
        # The Host header is a cluster Service name, and no browser can reach the pod,
        # so the localhost-only default would reject every real request.
        transport_security=TransportSecuritySettings(
            enable_dns_rebinding_protection=False
        ),
    )
    # The pool outlives any one request, so the app owns it.
    app.state.store = store
    return app
