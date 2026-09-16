"""SQL and connection handling. Owns every statement keepsake sends to Postgres."""

import os
import re

_SCHEMA_NAME = re.compile(r"[a-z_][a-z0-9_]*")


def validated_schema(name: str) -> str:
    """Reject anything not a bare identifier: the name is formatted into DDL."""
    if not _SCHEMA_NAME.fullmatch(name):
        raise ValueError(f"not a usable schema name: {name!r}")
    return name


SCHEMA = validated_schema(os.environ.get("KEEPSAKE_SCHEMA", "okf"))

# The only GUCs a policy may key on. For each, the policy, the connection that sets
# it and the startup check that asserts the policy reads it must all name the same
# string, so they are named once here.
TENANT_GUC = "okf.current_tenant"
ADMIN_GUC = "okf.admin"

# The one policy the startup check exempts from reading TENANT_GUC. Shared for the
# same reason: the migration creating it and the check recognising it must agree.
ADMIN_POLICY = "admin_read"


def _pool_size() -> int:
    """Refuse a bad value with a sentence rather than a traceback out of int()."""
    value = os.environ.get("KEEPSAKE_POOL_SIZE") or "10"
    if not value.isdecimal() or int(value) < 1:
        raise ValueError(f"KEEPSAKE_POOL_SIZE must be a positive integer: {value!r}")
    return int(value)


# Connections per pod, and so the server's concurrency: the tool bodies block on
# psycopg, and the worker threads running them are bounded by this same number, so a
# thread never waits on a connection that cannot exist. Multiply by replicaCount
# before raising it — the budget being spent is the database's backends.
POOL_SIZE = _pool_size()
