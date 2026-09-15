"""Refuse to start when the database configuration silently disables isolation."""

from keepsake.store import SCHEMA
from keepsake.store.pool import Store

# Alembic's bookkeeping table holds no tenant data and carries no policy, so forcing
# RLS on it would deny alembic its own version row.
_EXEMPT = "alembic_version"

_ROLE = "SELECT usesuper, usebypassrls FROM pg_user WHERE usename = current_user"

# pg_has_role covers membership: a role that may SET ROLE to the owner is the owner.
_SCHEMA_OWNER = (
    "SELECT pg_has_role(nspowner, 'USAGE') FROM pg_namespace WHERE nspname = %s"
)

_TABLES = """
    SELECT relname, relrowsecurity, relforcerowsecurity,
           pg_has_role(c.relowner, 'USAGE')
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = %s AND c.relkind = 'r' AND c.relname <> %s
"""


class MisconfiguredDatabase(RuntimeError):
    """Raised at startup. Crashing loudly beats serving cross-tenant reads."""


def verify(store: Store, schema: str = SCHEMA) -> None:
    """Raise unless row-level security actually constrains the connected role."""
    with store.raw() as conn:
        role = conn.execute(_ROLE).fetchone()
        if role is None:
            raise MisconfiguredDatabase(
                "okf cannot verify the connected role: it is absent from pg_user"
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
