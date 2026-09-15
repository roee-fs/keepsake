"""Refuse to start when the database configuration silently disables isolation."""

from collections import defaultdict

from keepsake.store import SCHEMA
from keepsake.store.pool import Store

# The one GUC a policy may key on. A policy ignoring it isolates nothing.
_GUC = "okf.current_tenant"

# Alembic's bookkeeping table holds no tenant data and carries no policy, so forcing
# RLS on it would deny alembic its own version row.
_EXEMPT = "alembic_version"

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

_TABLES = """
    SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
           pg_catalog.pg_has_role(c.relowner, 'MEMBER')
    FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = %s AND c.relkind IN ('r', 'p') AND c.relname <> %s
"""

_POLICIES = """
    SELECT c.relname, p.polname,
           coalesce(pg_catalog.pg_get_expr(p.polqual, p.polrelid), '')
    FROM pg_catalog.pg_policy p
    JOIN pg_catalog.pg_class c ON c.oid = p.polrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = %s
"""


class MisconfiguredDatabase(RuntimeError):
    """Raised at startup. Crashing loudly beats serving cross-tenant reads."""


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

        policies: defaultdict[str, list[tuple[str, str]]] = defaultdict(list)
        for table, policy, qual in conn.execute(_POLICIES, (schema,)).fetchall():
            policies[table].append((policy, qual))

        for name, enabled, forced, owned in conn.execute(
            _TABLES, (schema, _EXEMPT)
        ).fetchall():
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
            if not policies[name]:
                raise MisconfiguredDatabase(
                    f"{schema}.{name} has no row-level security policy"
                )
            # Permissive policies are ORed, so one that ignores the GUC opens the table
            # however strict its siblings are.
            for policy, qual in policies[name]:
                if _GUC not in qual:
                    raise MisconfiguredDatabase(
                        f"{schema}.{name} policy {policy} does not read {_GUC}: "
                        "it does not restrict rows to one tenant"
                    )
