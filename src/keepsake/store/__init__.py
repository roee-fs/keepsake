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
