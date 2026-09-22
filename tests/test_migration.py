"""The migration is the whole tenant-isolation story, so it is checked structurally.

Catalog metadata alone would accept a policy that reads the wrong GUC and isolates
nothing, so the policy expressions are checked for the GUC name as well.
"""

import uuid

import psycopg
import pytest

from keepsake.store import validated_schema

# Alembic's bookkeeping table lives in the okf schema but holds no tenant data.
_TENANT_TABLES = """
    SELECT relname, relrowsecurity, relforcerowsecurity
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'okf' AND c.relkind = 'r' AND c.relname <> 'alembic_version'
"""


def _scope(conn: psycopg.Connection, tenant: uuid.UUID) -> None:
    """set_config(..., is_local => true) is SET LOCAL, and takes a parameter."""
    conn.execute("SELECT set_config('okf.current_tenant', %s, true)", (str(tenant),))


def _seed(conn: psycopg.Connection, tenant: uuid.UUID) -> None:
    conn.execute(
        "INSERT INTO okf.concept (tenant_id, path, type) VALUES (%s, 'a.md', 'note')",
        (tenant,),
    )
    conn.execute(
        "INSERT INTO okf.concept_revision (tenant_id, path, version, op, snapshot) "
        "VALUES (%s, 'a.md', 1, 'create', '{}'::jsonb)",
        (tenant,),
    )


def test_tables_have_rls_enabled_and_forced(migrated: bool, pg_dsn: str) -> None:
    with psycopg.connect(pg_dsn) as conn:
        rows = conn.execute(_TENANT_TABLES).fetchall()
    assert rows, "no tables created"
    for name, enabled, forced in rows:
        assert enabled, f"{name} has RLS disabled"
        assert forced, f"{name} does not FORCE RLS"


def test_policies_read_the_okf_guc(migrated: bool, pg_dsn: str) -> None:
    """A policy on the wrong GUC satisfies every structural check and isolates nothing."""
    with psycopg.connect(pg_dsn) as conn:
        rows = conn.execute(
            "SELECT tablename, policyname, cmd, qual, with_check FROM pg_policies "
            "WHERE schemaname = 'okf'"
        ).fetchall()
    assert {(row[0], row[1]) for row in rows} == {
        (table, policy)
        for table in ("concept", "concept_revision")
        for policy in ("tenant_isolation", "admin_read")
    }
    for table, policy, cmd, qual, with_check in rows:
        if policy == "admin_read":
            # The one cross-tenant policy, so what keeps it off the write path is
            # that it applies to no other command.
            assert cmd == "SELECT", f"{table}.{policy} also applies to {cmd}"
            assert "current_setting('okf.admin'" in qual, (
                f"{table}.{policy} does not read okf.admin: {qual}"
            )
            continue
        for clause, expression in (("USING", qual), ("WITH CHECK", with_check)):
            where = f"{table}.{policy} {clause}"
            # A null expression is one Postgres does not apply: WITH CHECK absent is
            # WITH CHECK (true), which reads one tenant and writes any.
            assert expression is not None, f"{where} has no expression"
            assert "current_setting('okf.current_tenant'" in expression, (
                f"{where} does not read okf.current_tenant: {expression}"
            )
            assert "tenant_id" in expression, f"{where} ignores tenant_id: {expression}"


def test_the_app_role_owns_no_tables(migrated: bool, pg_dsn: str) -> None:
    """Owning a table is the third way out of RLS, alongside SUPERUSER and BYPASSRLS."""
    with psycopg.connect(pg_dsn) as conn:
        rows = conn.execute(
            "SELECT relname, pg_get_userbyid(relowner), "
            "has_table_privilege('okf_app', c.oid, 'TRUNCATE') "
            "FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "
            "WHERE n.nspname = 'okf' AND c.relkind = 'r'"
        ).fetchall()
    assert rows, "no tables created"
    for name, owner, truncatable in rows:
        assert owner == "okf_owner", f"okf.{name} is owned by {owner}"
        # TRUNCATE is not filtered by a policy, so it would empty every tenant.
        assert not truncatable, f"okf_app may TRUNCATE okf.{name}"


def test_alembic_version_table_is_not_in_public(migrated: bool, pg_dsn: str) -> None:
    """Schema-mode installs must not leave bookkeeping in the host database."""
    with psycopg.connect(pg_dsn) as conn:
        rows = conn.execute(
            "SELECT n.nspname FROM pg_class c "
            "JOIN pg_namespace n ON n.oid = c.relnamespace "
            "WHERE c.relname = 'alembic_version'"
        ).fetchall()
    assert [row[0] for row in rows] == ["okf"]


def test_purge_tenant_returns_the_count_and_empties_both_tables(
    migrated: bool, pg_dsn: str
) -> None:
    tenant = uuid.uuid4()
    with psycopg.connect(pg_dsn) as conn, conn.transaction():
        _scope(conn, tenant)
        _seed(conn, tenant)
        purged = conn.execute("SELECT okf.purge_tenant(%s)", (tenant,)).fetchone()
        remaining = conn.execute(
            "SELECT (SELECT count(*) FROM okf.concept), "
            "(SELECT count(*) FROM okf.concept_revision)"
        ).fetchone()
    assert purged == (1,)
    assert remaining == (0, 0)


def test_purge_tenant_refuses_a_tenant_the_session_is_not_scoped_to(
    migrated: bool, pg_dsn: str
) -> None:
    """A silent no-op reads to the operator as a completed purge."""
    scoped, other = uuid.uuid4(), uuid.uuid4()
    with psycopg.connect(pg_dsn) as conn, conn.transaction():
        _scope(conn, scoped)
        with pytest.raises(psycopg.errors.RaiseException):
            conn.execute("SELECT okf.purge_tenant(%s)", (other,))


def test_a_tenant_cannot_read_or_write_another_tenants_rows(
    migrated: bool, pg_dsn: str
) -> None:
    """Raw psycopg, so a failure here indicts the policy and nothing above it."""
    a, b = uuid.uuid4(), uuid.uuid4()
    with psycopg.connect(pg_dsn) as conn, conn.transaction():
        _scope(conn, a)
        _seed(conn, a)

    with psycopg.connect(pg_dsn) as conn, conn.transaction():
        _scope(conn, b)
        assert conn.execute("SELECT count(*) FROM okf.concept").fetchone() == (0,)
        # A WITH CHECK violation is 42501, not a check-constraint violation.
        with pytest.raises(psycopg.errors.InsufficientPrivilege):
            conn.execute(
                "INSERT INTO okf.concept (tenant_id, path, type) "
                "VALUES (%s, 'planted.md', 'note')",
                (a,),
            )

    with psycopg.connect(pg_dsn) as conn, conn.transaction():
        _scope(conn, a)
        assert conn.execute("SELECT okf.purge_tenant(%s)", (a,)).fetchone() == (1,)


@pytest.mark.parametrize(
    "name", ["okf; DROP TABLE concept", "public.okf", "OKF", "", "1okf"]
)
def test_a_schema_name_that_is_not_an_identifier_is_rejected(name: str) -> None:
    """The name is formatted into DDL, so the pattern is the whole defence."""
    with pytest.raises(ValueError):
        validated_schema(name)
