"""Database fixtures. The point of them is that `pg_dsn` is not privileged.

Postgres exempts superusers and table owners from row-level security, so tests
that connect as either would prove nothing about tenant isolation.
"""

from collections.abc import Iterator

import psycopg
import pytest
from psycopg import sql
from testcontainers.community.postgres import PostgresContainer

OWNER_ROLE = "okf_owner"
OWNER_PASSWORD = "owner"
APP_ROLE = "okf_app"
APP_PASSWORD = "app"


def assert_role_unprivileged(dsn: str) -> None:
    """Raise AssertionError if `dsn` logs in as a superuser."""
    with psycopg.connect(dsn) as conn:
        row = conn.execute(
            "SELECT usename, usesuper FROM pg_user WHERE usename = current_user"
        ).fetchone()
    assert row is not None, "connected role is missing from pg_user"
    # The DSN carries a password, so name the role instead.
    assert row[1] is False, f"role {row[0]!r} is a superuser; RLS would not apply"


def _dsn(pg: PostgresContainer, user: str, password: str) -> str:
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
    return _dsn(_pg, APP_ROLE, APP_PASSWORD)


@pytest.fixture
def assert_not_privileged(pg_dsn: str) -> None:
    """Guards the guard: a fixture regression must fail loudly, not silently."""
    assert_role_unprivileged(pg_dsn)
