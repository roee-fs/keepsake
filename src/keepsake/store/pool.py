"""Connection pooling and tenant scoping. The GUC is set here and nowhere else."""

from collections.abc import Iterator
from contextlib import contextmanager
from uuid import UUID

import psycopg
from psycopg import sql
from psycopg_pool import ConnectionPool

from keepsake.store import SCHEMA, TENANT_GUC


class Store:
    def __init__(self, dsn: str, schema: str = SCHEMA) -> None:
        self._search_path = sql.SQL("SET LOCAL search_path = {}, pg_catalog").format(
            sql.Identifier(schema)
        )
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
            conn.execute(self._search_path)
            yield conn

    @contextmanager
    def scope(self, tenant_id: UUID) -> Iterator[psycopg.Connection]:
        """Yield a connection scoped to `tenant_id` for the life of one transaction."""
        with self._pool.connection() as conn, conn.transaction():
            conn.execute(self._search_path)
            # set_config's is_local argument is SET LOCAL and, unlike SET, takes a
            # parameter. A session-scoped value would outlive the transaction and be
            # inherited by whoever next takes this connection from the pool.
            conn.execute(
                "SELECT set_config(%s, %s, true)", (TENANT_GUC, str(tenant_id))
            )
            yield conn
