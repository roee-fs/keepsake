"""Connection pooling and tenant scoping. The GUC is set here and nowhere else."""

from collections.abc import Iterator
from contextlib import contextmanager
from uuid import UUID

import psycopg
from psycopg_pool import ConnectionPool

from keepsake.store import SCHEMA, TENANT_GUC, validated_schema

# search_path is an ordinary GUC, so it travels as a parameter rather than as DDL.
_SET_SEARCH_PATH = "SELECT set_config('search_path', %s, true)"


class Store:
    def __init__(self, dsn: str, schema: str = SCHEMA) -> None:
        self._search_path = f"{validated_schema(schema)}, pg_catalog"
        self._pool = ConnectionPool(dsn, open=True, kwargs={"autocommit": False})

    def close(self) -> None:
        self._pool.close()

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
