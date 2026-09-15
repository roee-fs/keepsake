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
MASKED_ROLE = "okf_masked"
NOINHERIT_ROLE = "okf_noinherit"
NOINHERIT_PASSWORD = "noinherit"

# The migration's policy, restated so the tests that drop or rewrite it restore it.
TENANT_QUAL = "tenant_id = current_setting('okf.current_tenant')::uuid"
RESTORE_POLICY = (
    f"CREATE POLICY tenant_isolation ON okf.concept "
    f"USING ({TENANT_QUAL}) WITH CHECK ({TENANT_QUAL})"
)


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


def _decoy(name: str, columns: str, values: str) -> tuple[str, ...]:
    """DDL for a table shadowing the catalog table `name`, itself passing every check."""
    return (
        f"CREATE TABLE okf.{name} ({columns}, tenant_id uuid)",
        f"INSERT INTO okf.{name} VALUES ({values}, gen_random_uuid())",
        f"ALTER TABLE okf.{name} ENABLE ROW LEVEL SECURITY",
        f"ALTER TABLE okf.{name} FORCE ROW LEVEL SECURITY",
        f"CREATE POLICY tenant_isolation ON okf.{name} USING ({TENANT_QUAL})",
        f"GRANT SELECT ON okf.{name} TO okf_app",
    )


# One per catalog table the check reads: a superuser role, an okf schema the app role
# owns, and an unprotected table in okf.
DECOYS = (
    (
        "pg_roles",
        "rolname name, rolsuper bool, rolbypassrls bool",
        "'okf_app', true, true",
    ),
    ("pg_namespace", "nspname name, nspowner oid", "'okf', 'okf_app'::regrole::oid"),
    (
        "pg_class",
        (
            "oid oid, relname name, relrowsecurity bool, relforcerowsecurity bool, "
            'relowner oid, relnamespace oid, relkind "char"'
        ),
        (
            "0, 'evil', false, false, 'okf_app'::regrole::oid, "
            "(SELECT oid FROM pg_catalog.pg_namespace WHERE nspname = 'okf'), 'r'"
        ),
    ),
)


def _as_role(pg_dsn: str, role: str, password: str) -> str:
    """Swap the app credentials in `pg_dsn` for another role's."""
    return pg_dsn.replace("okf_app:app@", f"{role}:{password}@")


@pytest.fixture(params=("the owner itself", "a NOINHERIT member of the owner"))
def table_owner_dsn(
    request: pytest.FixtureRequest, migrated: bool, pg_dsn: str, admin_dsn: str
) -> Iterator[str]:
    """A role that owns one table in the schema, but does not own the schema.

    The table has RLS enabled and forced, so only the ownership branch can reject it.
    The second case inherits nothing until it runs SET ROLE, which it may do at will.
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
    if request.param == "the owner itself":
        yield _as_role(pg_dsn, TABLE_OWNER_ROLE, TABLE_OWNER_PASSWORD)
    else:
        _execute(
            admin_dsn,
            f"CREATE ROLE {NOINHERIT_ROLE} LOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS "
            f"PASSWORD '{NOINHERIT_PASSWORD}'",
            f"GRANT {TABLE_OWNER_ROLE} TO {NOINHERIT_ROLE}",
        )
        yield _as_role(pg_dsn, NOINHERIT_ROLE, NOINHERIT_PASSWORD)
        _execute(
            admin_dsn,
            f"REVOKE {TABLE_OWNER_ROLE} FROM {NOINHERIT_ROLE}",
            f"DROP ROLE {NOINHERIT_ROLE}",
        )
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


def test_rejects_a_noinherit_member_of_the_schema_owner(
    migrated: bool, pg_dsn: str, admin_dsn: str
) -> None:
    """A NOINHERIT member inherits nothing until it runs SET ROLE, and then it owns."""
    _execute(
        admin_dsn,
        f"CREATE ROLE {NOINHERIT_ROLE} LOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS "
        f"PASSWORD '{NOINHERIT_PASSWORD}'",
        f"GRANT okf_owner TO {NOINHERIT_ROLE}",
    )
    try:
        with pytest.raises(MisconfiguredDatabase, match="owns the schema"):
            _verify(_as_role(pg_dsn, NOINHERIT_ROLE, NOINHERIT_PASSWORD))
    finally:
        _execute(
            admin_dsn,
            f"REVOKE okf_owner FROM {NOINHERIT_ROLE}",
            f"DROP ROLE {NOINHERIT_ROLE}",
        )


def test_rejects_a_partitioned_table_without_rls(
    migrated: bool, pg_dsn: str, owner_dsn: str
) -> None:
    """A partitioned parent is relkind 'p', and holds the policy for its partitions."""
    _execute(
        owner_dsn,
        "CREATE TABLE okf.parted (tenant_id uuid NOT NULL) PARTITION BY RANGE (tenant_id)",
    )
    try:
        with pytest.raises(MisconfiguredDatabase, match="okf.parted has row-level"):
            _verify(pg_dsn)
    finally:
        _execute(owner_dsn, "DROP TABLE okf.parted")


def test_shadowing_tables_do_not_change_the_verdict(
    migrated: bool, pg_dsn: str, owner_dsn: str
) -> None:
    """search_path lists pg_catalog after okf, so okf.pg_class wins an unqualified read.

    Each decoy lies in the direction that would reject a correct database, and carries
    RLS and a policy of its own so nothing but the shadowing can fail the check.
    """
    _execute(owner_dsn, *[s for decoy in DECOYS for s in _decoy(*decoy)])
    try:
        _verify(pg_dsn)
    finally:
        _execute(owner_dsn, *[f"DROP TABLE okf.{name}" for name, _, _ in DECOYS])


def test_rejects_a_superuser_reached_by_set_role(
    migrated: bool, pg_dsn: str, admin_dsn: str
) -> None:
    """pg_user omits NOLOGIN roles, so this one reads as absent there, not as super."""
    _execute(
        admin_dsn,
        f"CREATE ROLE {MASKED_ROLE} SUPERUSER NOLOGIN",
        f"GRANT {MASKED_ROLE} TO okf_app",
    )
    try:
        with pytest.raises(
            MisconfiguredDatabase, match="must not connect as a superuser"
        ):
            _verify(f"{pg_dsn}?options=-c%20role%3D{MASKED_ROLE}")
    finally:
        _execute(
            admin_dsn,
            f"REVOKE {MASKED_ROLE} FROM okf_app",
            f"DROP ROLE {MASKED_ROLE}",
        )


def test_rejects_a_permissive_policy(
    migrated: bool, pg_dsn: str, owner_dsn: str
) -> None:
    """USING (true) leaves RLS enabled and forced while serving every tenant's rows."""
    _execute(owner_dsn, "ALTER POLICY tenant_isolation ON okf.concept USING (true)")
    try:
        with pytest.raises(MisconfiguredDatabase, match="does not read okf.current"):
            _verify(pg_dsn)
    finally:
        _execute(
            owner_dsn,
            f"ALTER POLICY tenant_isolation ON okf.concept USING ({TENANT_QUAL})",
        )


def test_rejects_a_permissive_with_check(
    migrated: bool, pg_dsn: str, owner_dsn: str
) -> None:
    """A correct USING with WITH CHECK (true) reads one tenant and writes any."""
    _execute(
        owner_dsn, "ALTER POLICY tenant_isolation ON okf.concept WITH CHECK (true)"
    )
    try:
        with pytest.raises(MisconfiguredDatabase, match="does not read okf.current"):
            _verify(pg_dsn)
    finally:
        _execute(
            owner_dsn,
            f"ALTER POLICY tenant_isolation ON okf.concept WITH CHECK ({TENANT_QUAL})",
        )


def test_accepts_a_policy_with_only_a_with_check_clause(
    migrated: bool, pg_dsn: str, owner_dsn: str
) -> None:
    """FOR INSERT carries no USING, and rejecting it would crash-loop a correct install."""
    _execute(
        owner_dsn,
        f"CREATE POLICY insert_only ON okf.concept FOR INSERT WITH CHECK ({TENANT_QUAL})",
    )
    try:
        _verify(pg_dsn)
    finally:
        _execute(owner_dsn, "DROP POLICY insert_only ON okf.concept")


def test_rejects_a_table_with_no_policy(
    migrated: bool, pg_dsn: str, owner_dsn: str
) -> None:
    _execute(owner_dsn, "DROP POLICY tenant_isolation ON okf.concept")
    try:
        with pytest.raises(MisconfiguredDatabase, match="no row-level security policy"):
            _verify(pg_dsn)
    finally:
        _execute(owner_dsn, RESTORE_POLICY)


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
