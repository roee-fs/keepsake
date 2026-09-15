"""Every branch of the startup check, proved by making it fire.

A test that only shows `verify` passing on a correct database proves nothing about
the branch it is named for, so each rejection here misconfigures the database (and
restores it) or connects as a role that is genuinely exempt from RLS.
"""

from collections.abc import Iterator

import psycopg
import pytest

from keepsake.store.pool import Store
from keepsake.store.verify import MisconfiguredDatabase, verify

TABLE_OWNER_ROLE = "okf_tableowner"
TABLE_OWNER_PASSWORD = "owns"
OWNED_TABLE = "owned_elsewhere"


def _verify(dsn: str, schema: str = "okf") -> None:
    """Run the check against `dsn` without leaking the pool it opens."""
    store = Store(dsn)
    try:
        verify(store, schema)
    finally:
        store.close()


def _execute(dsn: str, *statements: str) -> None:
    """Run DDL the checks react to. The statements are literals built in this file."""
    with psycopg.connect(dsn, autocommit=True) as conn:
        for statement in statements:
            conn.execute(statement)  # ty: ignore[no-matching-overload]


@pytest.fixture
def table_owner_dsn(migrated: bool, pg_dsn: str, admin_dsn: str) -> Iterator[str]:
    """A role owning one table in the schema, but not the schema itself.

    Its table has RLS enabled and forced, so only the ownership branch can reject it.
    """
    _execute(
        admin_dsn,
        f"CREATE ROLE {TABLE_OWNER_ROLE} LOGIN NOSUPERUSER NOBYPASSRLS "
        f"PASSWORD '{TABLE_OWNER_PASSWORD}'",
        f"GRANT USAGE ON SCHEMA okf TO {TABLE_OWNER_ROLE}",
        f"CREATE TABLE okf.{OWNED_TABLE} (tenant_id uuid NOT NULL)",
        f"ALTER TABLE okf.{OWNED_TABLE} OWNER TO {TABLE_OWNER_ROLE}",
        f"ALTER TABLE okf.{OWNED_TABLE} ENABLE ROW LEVEL SECURITY",
        f"ALTER TABLE okf.{OWNED_TABLE} FORCE ROW LEVEL SECURITY",
    )
    yield pg_dsn.replace("okf_app:app@", f"{TABLE_OWNER_ROLE}:{TABLE_OWNER_PASSWORD}@")
    _execute(
        admin_dsn,
        f"DROP TABLE okf.{OWNED_TABLE}",
        f"REVOKE USAGE ON SCHEMA okf FROM {TABLE_OWNER_ROLE}",
        f"DROP ROLE {TABLE_OWNER_ROLE}",
    )


def test_passes_for_the_unprivileged_role(migrated: bool, pg_dsn: str) -> None:
    _verify(pg_dsn)


def test_passes_although_the_bookkeeping_table_has_no_forced_rls(
    migrated: bool, pg_dsn: str
) -> None:
    """A correct install would crash-loop if alembic's table were held to the rule."""
    with psycopg.connect(pg_dsn) as conn:
        row = conn.execute(
            "SELECT relrowsecurity, relforcerowsecurity FROM pg_class c "
            "JOIN pg_namespace n ON n.oid = c.relnamespace "
            "WHERE n.nspname = 'okf' AND c.relname = 'alembic_version'"
        ).fetchone()
    assert row == (False, False), "the exemption no longer covers a real case"
    _verify(pg_dsn)


def test_rejects_a_superuser(migrated: bool, admin_dsn: str) -> None:
    with pytest.raises(MisconfiguredDatabase, match="must not connect as a superuser"):
        _verify(admin_dsn)


def test_rejects_a_bypassrls_role(migrated: bool, bypassrls_dsn: str) -> None:
    """BYPASSRLS is what an operator grants when isolation queries start failing."""
    with pytest.raises(MisconfiguredDatabase, match="BYPASSRLS"):
        _verify(bypassrls_dsn)


def test_rejects_the_schema_owner(migrated: bool, owner_dsn: str) -> None:
    with pytest.raises(MisconfiguredDatabase, match="owns the schema"):
        _verify(owner_dsn)


def test_rejects_a_table_owner(table_owner_dsn: str) -> None:
    """Owning a table in someone else's schema is its own way out of RLS."""
    with pytest.raises(MisconfiguredDatabase, match="owns okf.owned_elsewhere"):
        _verify(table_owner_dsn)


def test_rejects_a_missing_schema(migrated: bool, pg_dsn: str) -> None:
    with pytest.raises(MisconfiguredDatabase, match="no schema named okf_absent"):
        _verify(pg_dsn, "okf_absent")


def test_rejects_a_table_with_rls_disabled(
    migrated: bool, pg_dsn: str, owner_dsn: str
) -> None:
    _execute(owner_dsn, "ALTER TABLE okf.concept DISABLE ROW LEVEL SECURITY")
    try:
        with pytest.raises(MisconfiguredDatabase, match="row-level security disabled"):
            _verify(pg_dsn)
    finally:
        _execute(owner_dsn, "ALTER TABLE okf.concept ENABLE ROW LEVEL SECURITY")


def test_rejects_a_table_without_forced_rls(
    migrated: bool, pg_dsn: str, owner_dsn: str
) -> None:
    _execute(owner_dsn, "ALTER TABLE okf.concept NO FORCE ROW LEVEL SECURITY")
    try:
        with pytest.raises(MisconfiguredDatabase, match="FORCE"):
            _verify(pg_dsn)
    finally:
        _execute(owner_dsn, "ALTER TABLE okf.concept FORCE ROW LEVEL SECURITY")
