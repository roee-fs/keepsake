"""Database fixtures. The point of them is that `pg_dsn` is not privileged.

Postgres exempts superusers, BYPASSRLS roles and table owners from row-level
security, so tests connecting as any of those would prove nothing about tenant
isolation. `pg_dsn` checks itself, so the check cannot be skipped.
"""

import uuid
from collections.abc import Iterator

import psycopg
import pytest
from psycopg import sql
from testcontainers.community.postgres import PostgresContainer

from keepsake.cli import migrate
from keepsake.server.tools import Tools
from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store

OWNER_ROLE = "okf_owner"
OWNER_PASSWORD = "owner"
APP_ROLE = "okf_app"
APP_PASSWORD = "app"
BYPASSRLS_ROLE = "okf_bypassrls"
BYPASSRLS_PASSWORD = "bypassrls"


def assert_role_unprivileged(dsn: str) -> None:
    """Raise AssertionError if `dsn` logs in as a role exempt from RLS."""
    with psycopg.connect(dsn) as conn:
        row = conn.execute(
            "SELECT usename, usesuper, usebypassrls "
            "FROM pg_user WHERE usename = current_user"
        ).fetchone()
    assert row is not None, "connected role is missing from pg_user"
    # The DSN carries a password, so name the role instead.
    assert row[1] is False, f"role {row[0]!r} is a superuser; RLS would not apply"
    assert row[2] is False, f"role {row[0]!r} has BYPASSRLS; RLS would not apply"


def _dsn(pg: PostgresContainer, user: str, password: str) -> str:
    """A URL, not a keyword string: one test appends a `?options=` query to it."""
    host = pg.get_container_host_ip()
    port = pg.get_exposed_port(5432)
    return f"postgresql://{user}:{password}@{host}:{port}/{pg.dbname}"


@pytest.fixture(scope="session")
def _pg() -> Iterator[PostgresContainer]:
    with PostgresContainer("postgres:17", driver=None) as pg:
        yield pg


@pytest.fixture(scope="session")
def admin_dsn(_pg: PostgresContainer) -> str:
    """The container's superuser. For fixture setup only, never for tests."""
    return _dsn(_pg, _pg.username, _pg.password)


@pytest.fixture(scope="session")
def owner_dsn(_pg: PostgresContainer, admin_dsn: str) -> str:
    """Owns the schema and its tables. Exempt from RLS, so migrations only."""
    create_role = sql.SQL("CREATE ROLE {} LOGIN PASSWORD {}")
    with psycopg.connect(admin_dsn, autocommit=True) as conn:
        conn.execute(
            create_role.format(sql.Identifier(OWNER_ROLE), sql.Literal(OWNER_PASSWORD))
        )
        conn.execute(
            create_role.format(sql.Identifier(APP_ROLE), sql.Literal(APP_PASSWORD))
        )
        conn.execute(
            sql.SQL("GRANT CREATE ON DATABASE {} TO {}").format(
                sql.Identifier(_pg.dbname), sql.Identifier(OWNER_ROLE)
            )
        )
    return _dsn(_pg, OWNER_ROLE, OWNER_PASSWORD)


@pytest.fixture(scope="session")
def pg_dsn(_pg: PostgresContainer, owner_dsn: str) -> str:
    """The unprivileged role the application uses. RLS only applies to this one."""
    _ = owner_dsn  # Creates both roles.
    dsn = _dsn(_pg, APP_ROLE, APP_PASSWORD)
    # Enforced here so no test can take the DSN without the check.
    assert_role_unprivileged(dsn)
    return dsn


@pytest.fixture(scope="session")
def bypassrls_dsn(_pg: PostgresContainer, admin_dsn: str) -> str:
    """A non-superuser that is still exempt from RLS. Only the guard test uses it."""
    with psycopg.connect(admin_dsn, autocommit=True) as conn:
        conn.execute(
            sql.SQL("CREATE ROLE {} LOGIN NOSUPERUSER BYPASSRLS PASSWORD {}").format(
                sql.Identifier(BYPASSRLS_ROLE), sql.Literal(BYPASSRLS_PASSWORD)
            )
        )
    return _dsn(_pg, BYPASSRLS_ROLE, BYPASSRLS_PASSWORD)


@pytest.fixture(scope="session")
def migrated(owner_dsn: str) -> bool:
    """Runs the migration as the owner, through the command an operator runs.

    The migration grants the app role itself.
    """
    migrate(owner_dsn)
    return True


@pytest.fixture
def tenant() -> uuid.UUID:
    """A tenant of its own per test: the database outlives the function-scoped store."""
    return uuid.uuid4()


@pytest.fixture
def store(migrated: bool, pg_dsn: str) -> Iterator[Store]:
    """A store on the unprivileged role. Its pool is closed, not leaked per test."""
    store = Store(pg_dsn)
    yield store
    store.close()


@pytest.fixture
def concepts(store: Store) -> ConceptStore:
    return ConceptStore(store)


@pytest.fixture
def tools(concepts: ConceptStore) -> Tools:
    """Bound to a tenant of its own, as the server binds one at startup."""
    return Tools(concepts, uuid.uuid4(), "test")
