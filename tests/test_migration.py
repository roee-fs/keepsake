"""The migration is the whole tenant-isolation story, so it is checked structurally.

Catalog metadata alone would accept a policy that reads the wrong GUC and isolates
nothing, so the policy expressions are checked for the GUC name as well.
"""

import uuid

import psycopg

# Alembic's bookkeeping table lives in the okf schema but holds no tenant data.
_TENANT_TABLES = """
    SELECT relname, relrowsecurity, relforcerowsecurity
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'okf' AND c.relkind = 'r' AND c.relname <> 'alembic_version'
"""


def test_tables_have_rls_enabled_and_forced(migrated: bool, pg_dsn: str) -> None:
    with psycopg.connect(pg_dsn) as conn:
        rows = conn.execute(_TENANT_TABLES).fetchall()
    assert rows, "no tables created"
    for name, enabled, forced in rows:
        assert enabled, f"{name} has RLS disabled"
        assert forced, f"{name} does not FORCE RLS"


def test_every_policy_has_with_check(migrated: bool, pg_dsn: str) -> None:
    with psycopg.connect(pg_dsn) as conn:
        rows = conn.execute(
            "SELECT tablename, policyname, with_check FROM pg_policies "
            "WHERE schemaname = 'okf'"
        ).fetchall()
    assert rows, "no policies created"
    for table, policy, with_check in rows:
        assert with_check is not None, f"{table}.{policy} has no WITH CHECK"


def test_policies_read_the_okf_guc(migrated: bool, pg_dsn: str) -> None:
    """A policy on the wrong GUC satisfies every structural check and isolates nothing."""
    with psycopg.connect(pg_dsn) as conn:
        rows = conn.execute(
            "SELECT tablename, policyname, qual, with_check FROM pg_policies "
            "WHERE schemaname = 'okf'"
        ).fetchall()
    tables = {row[0] for row in rows}
    assert tables == {"concept", "concept_revision"}
    for table, policy, qual, with_check in rows:
        for clause, expression in (("USING", qual), ("WITH CHECK", with_check)):
            where = f"{table}.{policy} {clause}"
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


def test_alembic_version_is_in_okf_and_closed_to_the_app_role(
    migrated: bool, pg_dsn: str
) -> None:
    """Schema-mode installs must not leave bookkeeping in the host database."""
    with psycopg.connect(pg_dsn) as conn:
        rows = conn.execute(
            "SELECT n.nspname, has_table_privilege('okf_app', c.oid, 'SELECT'), "
            "has_table_privilege('okf_app', c.oid, 'UPDATE') "
            "FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace "
            "WHERE c.relname = 'alembic_version'"
        ).fetchall()
    assert rows == [("okf", False, False)]


def test_purge_tenant_deletes_the_tenants_rows(migrated: bool, pg_dsn: str) -> None:
    tenant = uuid.uuid4()
    with psycopg.connect(pg_dsn) as conn, conn.transaction():
        # set_config(..., is_local => true) is SET LOCAL, and takes a parameter.
        conn.execute(
            "SELECT set_config('okf.current_tenant', %s, true)", (str(tenant),)
        )
        conn.execute(
            "INSERT INTO okf.concept (tenant_id, path, type) VALUES (%s, 'a.md', 'note')",
            (tenant,),
        )
        conn.execute(
            "INSERT INTO okf.concept_revision (tenant_id, path, version, op, snapshot) "
            "VALUES (%s, 'a.md', 1, 'create', '{}'::jsonb)",
            (tenant,),
        )
        conn.execute("SELECT okf.purge_tenant(%s)", (tenant,))
        remaining = conn.execute(
            "SELECT (SELECT count(*) FROM okf.concept), "
            "(SELECT count(*) FROM okf.concept_revision)"
        ).fetchone()
    assert remaining == (0, 0)
