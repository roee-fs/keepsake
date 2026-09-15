"""Tenant isolation as seen through `Store`, the only place the GUC is set.

`test_migration.py` proves the policies; these prove that the connection handling
above them scopes every statement and leaves no scope behind on a pooled connection.
"""

import uuid

import psycopg
import pytest

from keepsake.store.pool import Store

A, B = uuid.uuid4(), uuid.uuid4()


def _insert(conn: psycopg.Connection, tenant: uuid.UUID, path: str) -> None:
    conn.execute(
        "INSERT INTO okf.concept (tenant_id, path, type) VALUES (%s, %s, 'Concept')",
        (tenant, path),
    )


def test_read_isolation(store: Store) -> None:
    with store.scope(A) as c:
        _insert(c, A, "a/one")
    with store.scope(B) as c:
        assert c.execute("SELECT count(*) FROM okf.concept").fetchone() == (0,)


def test_cannot_insert_for_another_tenant(store: Store) -> None:
    """Catches a WITH CHECK that does not constrain tenant_id.

    Omitting WITH CHECK entirely is not that defect: Postgres then applies the
    USING expression to new rows, which is the same constraint.
    """
    with pytest.raises(psycopg.errors.InsufficientPrivilege), store.scope(A) as c:
        _insert(c, B, "a/two")


def test_scope_does_not_leak_across_pooled_connections(store: Store) -> None:
    """Catches SET instead of SET LOCAL."""
    with store.scope(A) as c:
        _insert(c, A, "a/three")
    # More acquisitions than the pool holds, so every pooled connection is reused.
    for _ in range(5):
        with store.scope(B) as c:
            assert c.execute("SELECT count(*) FROM okf.concept").fetchone() == (0,)
        with store.raw() as c:
            # missing_ok, because a scope that outlived its transaction is the defect
            # under test: an unset GUC is NULL, and one reset by SET LOCAL is ''.
            left = c.execute(
                "SELECT current_setting('okf.current_tenant', true)"
            ).fetchone()
            assert left in {(None,), ("",)}, f"scope outlived its transaction: {left}"


def test_unset_scope_raises_rather_than_returning_everything(store: Store) -> None:
    with pytest.raises(psycopg.errors.UndefinedObject), store.raw() as c:
        c.execute("SELECT count(*) FROM okf.concept").fetchone()
