"""A non-default KEEPSAKE_SCHEMA, end to end.

Every other test hardcodes `okf.`, so the configurable path was exercised nowhere and
failed with a misleading "relation does not exist". The name is read from the
environment at import, so this drives the real command in a subprocess rather than
reloading modules in-process.
"""

from __future__ import annotations

import os
import subprocess
import uuid

import psycopg
import pytest

from keepsake.store import validated_schema
from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store
from okf_core import Concept

SCHEMA = "okf_elsewhere"


def test_a_non_default_schema_migrates_and_serves(
    owner_dsn: str, pg_dsn: str, admin_dsn: str
) -> None:
    """The name reaches the DDL, the policies and the app role's grants — and the store
    then reads and writes through it with no qualified name anywhere in the SQL."""
    migrated = subprocess.run(
        ["keepsake", "migrate"],
        capture_output=True,
        text=True,
        check=False,
        env=os.environ | {"KEEPSAKE_SCHEMA": SCHEMA, "KEEPSAKE_DSN": owner_dsn},
    )
    assert migrated.returncode == 0, migrated.stderr

    with psycopg.connect(owner_dsn) as conn:
        guarded = [
            row[0]
            for row in conn.execute(
                "SELECT relname FROM pg_class "
                "WHERE relnamespace = %s::regnamespace AND relkind = 'r' "
                "AND relrowsecurity AND relforcerowsecurity ORDER BY relname",
                (SCHEMA,),
            ).fetchall()
        ]
    assert guarded == ["concept", "concept_revision"]

    store = Store(pg_dsn, schema=SCHEMA)
    try:
        tenant = uuid.uuid4()
        concepts = ConceptStore(store)
        concepts.create(tenant, Concept(path="a/b", type="Concept", body="here"), "t")
        stored = concepts.read(tenant, "a/b")
        assert stored is not None and stored.body == "here"

        # And it landed in that schema rather than in okf, which the search_path would
        # have reached just as silently. As the superuser, because FORCE ROW LEVEL
        # SECURITY subjects the owner to the policy too, and the policy's
        # `current_setting` raises on a connection that never scoped itself.
        with psycopg.connect(admin_dsn) as conn:
            row = conn.execute(
                f"SELECT count(*) FROM {SCHEMA}.concept WHERE tenant_id = %s", (tenant,)
            ).fetchone()
        assert row is not None and row[0] == 1
    finally:
        store.close()


@pytest.mark.parametrize(
    "bad", ["okf; DROP TABLE concept", "Okf", "1okf", "okf-other", "", "okf okf"]
)
def test_an_unusable_schema_name_is_refused_before_it_reaches_ddl(bad: str) -> None:
    """The name is formatted into DDL rather than bound, so this check is the only
    thing between a values file and a statement of the operator's choosing."""
    with pytest.raises(ValueError, match="not a usable schema name"):
        validated_schema(bad)
