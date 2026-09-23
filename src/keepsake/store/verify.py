"""Refuse to start when the database configuration silently disables isolation."""

from collections import defaultdict

from keepsake.store import ADMIN_GUC, ADMIN_POLICY, SCHEMA, TENANT_GUC
from keepsake.store.pool import Store

# Every catalog read below is schema-qualified: search_path names pg_catalog explicitly,
# so it is searched in listed order and a table named okf.pg_class would shadow it.

# pg_roles, not pg_user: pg_user omits NOLOGIN roles, which `SET role` can reach.
_ROLE = (
    "SELECT rolsuper, rolbypassrls FROM pg_catalog.pg_roles "
    "WHERE rolname = current_user"
)

# MEMBER, not USAGE: a NOINHERIT member holds none of the owner's privileges until it
# runs SET ROLE, and may run it at any time.
_SCHEMA_OWNER = (
    "SELECT pg_catalog.pg_has_role(nspowner, 'MEMBER') "
    "FROM pg_catalog.pg_namespace WHERE nspname = %s"
)

# A tenant_id column, not a name: the rule is "every table holding tenant data", and
# an exemption list is how a table that does hold it gets waved through. Alembic's
# bookkeeping table has no such column, so it stays out without being named.
_TABLES = """
    SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
           pg_catalog.pg_has_role(c.relowner, 'MEMBER')
    FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = %s AND c.relkind IN ('r', 'p')
      AND EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
                  WHERE a.attrelid = c.oid AND a.attname = 'tenant_id'
                    AND NOT a.attisdropped)
"""

# Both expressions: USING alone leaves WITH CHECK (true) free to admit another
# tenant's inserts, and an INSERT-only policy carries no USING at all.
_POLICIES = """
    SELECT c.relname, p.polname, p.polcmd, p.polpermissive,
           pg_catalog.pg_get_expr(p.polqual, p.polrelid),
           pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid)
    FROM pg_catalog.pg_policy p
    JOIN pg_catalog.pg_class c ON c.oid = p.polrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = %s
"""


class MisconfiguredDatabase(RuntimeError):
    """Raised at startup. Crashing loudly beats serving cross-tenant reads."""


# pg_policy.polcmd for a policy applying to SELECT alone. '*' is ALL, and a policy
# reaching a write is not the exemption.
_SELECT_ONLY = "r"

# admin_read as migration 0004 writes it, whitespace collapsed. Any looser test, such
# as mentioning the GUC, also passes a policy that admits every row.
_ADMIN_QUAL = (
    f"(tenant_id >= CASE WHEN (current_setting('{ADMIN_GUC}'::text, true) = "
    "'on'::text) THEN '00000000-0000-0000-0000-000000000000'::uuid "
    "ELSE NULL::uuid END)"
)


def _policy_fault(
    policy: str, cmd: str, permissive: bool, expressions: list[str]
) -> str | None:
    """Why a policy fails to confine the rows it admits, or None if it does.

    The sentence is read off a crash-looping pod, so each case names the clause
    that actually failed.
    """
    if not expressions:
        return "applies no expression, so it admits every row"
    if policy != ADMIN_POLICY:
        if all(TENANT_GUC in e for e in expressions):
            return None
        return f"does not read {TENANT_GUC}, so it does not restrict rows to one tenant"
    # The single exemption, for the admin console's cross-tenant read. Pinned to the
    # name, the command, permissiveness and the exact expression: loosen any one and
    # a policy that admits another tenant's rows starts passing this check.
    if cmd != _SELECT_ONLY:
        return (
            f"is the {ADMIN_POLICY} exemption but is not FOR SELECT, so it would "
            "admit another tenant's rows to a write"
        )
    if not permissive:
        return (
            f"is the {ADMIN_POLICY} exemption but is RESTRICTIVE, so it hides every "
            "row from every tenant"
        )
    if [" ".join(e.split()) for e in expressions] != [_ADMIN_QUAL]:
        return (
            f"is the {ADMIN_POLICY} exemption but does not read exactly {_ADMIN_QUAL}"
        )
    return None


def verify(store: Store, schema: str = SCHEMA) -> None:
    """Raise unless row-level security actually constrains the connected role."""
    with store.raw() as conn:
        role = conn.execute(_ROLE).fetchone()
        if role is None:
            raise MisconfiguredDatabase(
                "okf cannot verify the connected role: it is absent from pg_roles"
            )
        if role[0]:
            raise MisconfiguredDatabase(
                "okf must not connect as a superuser: RLS does not apply to superusers"
            )
        if role[1]:
            raise MisconfiguredDatabase(
                "okf must not connect as a role with BYPASSRLS: "
                "RLS does not apply to such a role"
            )

        owner = conn.execute(_SCHEMA_OWNER, (schema,)).fetchone()
        if owner is None:
            raise MisconfiguredDatabase(f"okf found no schema named {schema}")
        if owner[0]:
            raise MisconfiguredDatabase(
                f"okf must not connect as a role that owns the schema {schema}: "
                "an owner bypasses row-level security"
            )

        policies: defaultdict[str, list[tuple[str, str, bool, list[str]]]] = (
            defaultdict(list)
        )
        for table, policy, cmd, permissive, qual, check in conn.execute(
            _POLICIES, (schema,)
        ).fetchall():
            # A null expression is one Postgres does not apply, not an empty one.
            policies[table].append(
                (policy, cmd, permissive, [e for e in (qual, check) if e is not None])
            )

        for name, enabled, forced, owned in conn.execute(_TABLES, (schema,)).fetchall():
            if owned:
                raise MisconfiguredDatabase(
                    f"okf must not connect as a role that owns {schema}.{name}: "
                    "an owner may disable row-level security on its own table"
                )
            if not enabled:
                raise MisconfiguredDatabase(
                    f"{schema}.{name} has row-level security disabled"
                )
            if not forced:
                raise MisconfiguredDatabase(
                    f"{schema}.{name} does not FORCE row-level security"
                )
            # A permissive tenant policy, not merely a policy: admin_read on its own,
            # or a restrictive tenant policy, leaves the table unreadable by every tenant.
            if not any(
                permissive and TENANT_GUC in e
                for _, _, permissive, exprs in policies[name]
                for e in exprs
            ):
                raise MisconfiguredDatabase(
                    f"{schema}.{name} has no row-level security policy scoping it"
                    f" to one tenant"
                )
            # Permissive policies are ORed, so one that ignores the GUC opens the table
            # however strict its siblings are. Every expression it does apply must
            # read a GUC: reads and writes are gated by different ones.
            for policy, cmd, permissive, expressions in policies[name]:
                fault = _policy_fault(policy, cmd, permissive, expressions)
                if fault is not None:
                    raise MisconfiguredDatabase(
                        f"{schema}.{name} policy {policy} {fault}"
                    )
