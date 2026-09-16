"""Connection pooling and tenant scoping. The GUC is set here and nowhere else."""

from collections.abc import Iterator
from contextlib import contextmanager
from uuid import UUID

import psycopg
from psycopg_pool import ConnectionPool

from keepsake.store import ADMIN_GUC, POOL_SIZE, SCHEMA, TENANT_GUC, validated_schema

# search_path is an ordinary GUC, so it travels as a parameter rather than as DDL.
_SET_SEARCH_PATH = "SELECT set_config('search_path', %s, true)"

# The two policies OR into one expression, and tenant_isolation casts TENANT_GUC to
# uuid whichever way that OR is planned: Postgres does not promise short-circuiting
# and says outright not to rely on it to avoid an error. An unset GUC reads NULL or
# '', and ''::uuid raises. This value casts cleanly, matches nothing, and so leaves
# admin_read to supply the true.
_NIL_TENANT = "00000000-0000-0000-0000-000000000000"


class Store:
    def __init__(self, dsn: str, schema: str = SCHEMA) -> None:
        self._search_path = f"{validated_schema(schema)}, pg_catalog"
        # max_size, not just the default min_size: psycopg's pool otherwise never grows
        # past four connections, which caps the whole pod at four concurrent writes.
        self._pool = ConnectionPool(
            dsn,
            open=True,
            min_size=1,
            max_size=POOL_SIZE,
            # Unchecked by default, and a pooled connection does not notice the server
            # going away: every restart or failover then hands each waiting agent one
            # dead connection and "terminating connection due to administrator command",
            # as a transport failure it cannot act on. The check is one round trip and
            # replaces the connection instead.
            check=ConnectionPool.check_connection,
            # The reconnect delay doubles — 1s, 2s, 4s — and is bounded only by this
            # window. At the 300s default a replica that failed for a minute sleeps the
            # next minute too, so it is still refusing writes long after the database
            # came back. Giving up sooner is what shortens that: the pool drops the
            # attempt and the next request starts a fresh one.
            reconnect_timeout=15.0,
            kwargs={"autocommit": False},
        )

    def close(self) -> None:
        self._pool.close()

    def healthy(self, timeout: float = 2.0) -> bool:
        """Whether the pool can hand out a working connection right now.

        Answers rather than raises: this drives a readiness probe, and the one thing
        it must never do is fail to produce a verdict. The timeout is short on purpose
        — the probe reports "not ready" far sooner than the pool's own 30s wait.
        """
        try:
            with self._pool.connection(timeout=timeout) as conn:
                conn.execute("SELECT 1")
        except Exception:  # noqa: BLE001 - a probe reports, it does not propagate
            return False
        return True

    @contextmanager
    def raw(self) -> Iterator[psycopg.Connection]:
        """A connection with no tenant scope. For startup checks only."""
        with self._pool.connection() as conn, conn.transaction():
            # Enforces the docstring rather than advertising it: the pool commits on
            # clean exit, so an unscoped connection is otherwise a usable write path.
            conn.execute("SET TRANSACTION READ ONLY")
            conn.execute(_SET_SEARCH_PATH, (self._search_path,))
            yield conn

    @contextmanager
    def scope(self, tenant_id: UUID) -> Iterator[psycopg.Connection]:
        """Yield a connection scoped to `tenant_id` for the life of one transaction."""
        with self._pool.connection() as conn, conn.transaction():
            # set_config's is_local argument is SET LOCAL and, unlike SET, takes a
            # parameter. A session-scoped value would outlive the transaction and be
            # inherited by whoever next takes this connection from the pool. One
            # statement rather than two: this runs on every read the server serves.
            conn.execute(
                "SELECT set_config('search_path', %s, true),"
                "       set_config(%s, %s, true)",
                (self._search_path, TENANT_GUC, str(tenant_id)),
            )
            yield conn

    @contextmanager
    def admin_scope(self) -> Iterator[psycopg.Connection]:
        """Yield a connection that reads every tenant, for the admin console only.

        Read-only, and the policy behind it is FOR SELECT: an admin has no write
        path into a tenant it did not name. The read is gated far more weakly —
        okf.admin is self-asserted, so anything holding the app DSN can set it and
        read every tenant, with or without this method. That follows from gating on
        a GUC with no TO clause, which is what keeps the policy working in
        postgres.mode: existing, where the role has a name we do not know.
        Authentication therefore happens above this method and never inside it.
        """
        with self._pool.connection() as conn, conn.transaction():
            conn.execute("SET TRANSACTION READ ONLY")
            conn.execute(
                "SELECT set_config('search_path', %s, true),"
                "       set_config(%s, 'on', true),"
                "       set_config(%s, %s, true)",
                (self._search_path, ADMIN_GUC, TENANT_GUC, _NIL_TENANT),
            )
            yield conn
