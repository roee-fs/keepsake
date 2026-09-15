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

# The one GUC a policy may key on. The policy, the connection that sets it and the
# startup check that asserts the policy reads it must all name the same one.
TENANT_GUC = "okf.current_tenant"

# Connections per pod, and so the server's concurrency: the tool bodies block on
# psycopg, and the worker threads running them are bounded by this same number, so a
# thread never waits on a connection that cannot exist. Multiply by replicaCount
# before raising it — the budget being spent is the database's backends.
POOL_SIZE = int(os.environ.get("KEEPSAKE_POOL_SIZE") or 10)
