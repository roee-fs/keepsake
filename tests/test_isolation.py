"""Tenant isolation as seen through `Store`, the only place the GUC is set.

`test_migration.py` proves the policies; these prove that the connection handling
above them scopes every statement and leaves no scope behind on a pooled connection.
"""

import uuid

import psycopg
import pytest

from keepsake.store.pool import Store

A, B = uuid.uuid4(), uuid.uuid4()

# Enough acquisitions to cycle any plausible pool. Exceeding it fails the probe.
_PROBE_ATTEMPTS = 64


def _insert(conn: psycopg.Connection, tenant: uuid.UUID, path: str) -> None:
    conn.execute(
        "INSERT INTO okf.concept (tenant_id, path, type) VALUES (%s, %s, 'Concept')",
        (tenant, path),
    )


def _assert_no_leftover_scope(conn: psycopg.Connection) -> None:
    """missing_ok: an unset GUC reads NULL, and one reset by SET LOCAL reads ''."""
    left = conn.execute("SELECT current_setting('okf.current_tenant', true)").fetchone()
    assert left in {(None,), ("",)}, f"scope outlived its transaction: {left}"


def _probe_until_a_scoped_connection_returns(store: Store, scoped: set[int]) -> None:
    """Take raw connections until one of `scoped` is re-handed, checking each.

    Self-verifying rather than pool-geometry-dependent: a pool too large to cycle
    fails here instead of silently ceasing to test the leak path.
    """
    for _ in range(_PROBE_ATTEMPTS):
        with store.raw() as conn:
            _assert_no_leftover_scope(conn)
            if id(conn) in scoped:
                return
    raise AssertionError("no scoped connection came back; the leak path went untested")


def test_read_isolation(store: Store) -> None:
    with store.scope(A) as c:
        _insert(c, A, "a/one")
        # The positive control: a policy matching nothing also returns 0 below.
        assert c.execute(
            "SELECT count(*) FROM okf.concept WHERE path = 'a/one'"
        ).fetchone() == (1,)
    with store.scope(B) as c:
        assert c.execute("SELECT count(*) FROM okf.concept").fetchone() == (0,)


def test_cannot_insert_for_another_tenant(store: Store) -> None:
    """Catches a WITH CHECK that does not constrain tenant_id.

    Omitting WITH CHECK entirely is not that defect: Postgres then applies the
    USING expression to new rows, which is the same constraint.
    """
    scoped: set[int] = set()
    with pytest.raises(psycopg.errors.InsufficientPrivilege), store.scope(A) as c:
        # Recorded before the raise, which skips the rest of the body.
        scoped.add(id(c))
        _insert(c, B, "a/two")
    # The transaction aborted, so the scope must be gone from that connection too.
    _probe_until_a_scoped_connection_returns(store, scoped)


def test_scope_does_not_leak_across_pooled_connections(store: Store) -> None:
    """Catches SET instead of SET LOCAL."""
    scoped: set[int] = set()
    with store.scope(A) as c:
        _insert(c, A, "a/three")
        scoped.add(id(c))
    for _ in range(5):
        with store.scope(B) as c:
            assert c.execute("SELECT count(*) FROM okf.concept").fetchone() == (0,)
            scoped.add(id(c))
    _probe_until_a_scoped_connection_returns(store, scoped)


def test_unset_scope_raises_rather_than_returning_everything(store: Store) -> None:
    """Either SQLSTATE is correct: a never-assigned GUC is undefined, a reset one ''."""
    with (
        pytest.raises((psycopg.errors.UndefinedObject, psycopg.errors.DataError)),
        store.raw() as c,
    ):
        c.execute("SELECT count(*) FROM okf.concept").fetchone()
