"""Tenant isolation as seen through `Store`, the only place the GUC is set.

`test_migration.py` proves the policies; these prove that the connection handling
above them scopes every statement and leaves no scope behind on a pooled connection.
"""

import uuid

import psycopg
import pytest

from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store
from okf_core import Concept

A, B = uuid.uuid4(), uuid.uuid4()

# Enough acquisitions to cycle any plausible pool. Exceeding it fails the probe.
_PROBE_ATTEMPTS = 64


def _insert(conn: psycopg.Connection, tenant: uuid.UUID, path: str) -> None:
    conn.execute(
        "INSERT INTO okf.concept (tenant_id, path, type) VALUES (%s, %s, 'Concept')",
        (tenant, path),
    )


def _owners(conn: psycopg.Connection, paths: list[str]) -> set[uuid.UUID]:
    """The tenants owning `paths` that this connection can actually see."""
    rows = conn.execute(
        "SELECT tenant_id FROM okf.concept WHERE path = ANY(%s)", (paths,)
    ).fetchall()
    return {row[0] for row in rows}


def _assert_no_leftover_scope(conn: psycopg.Connection) -> None:
    """missing_ok: an unset GUC reads NULL, and one reset by SET LOCAL reads ''.

    Both GUCs: okf.admin riding a recycled connection to the next caller is a
    cross-tenant read exactly as okf.current_tenant leaking is.
    """
    left = conn.execute(
        "SELECT current_setting('okf.current_tenant', true),"
        "       current_setting('okf.admin', true)"
    ).fetchone()
    assert left is not None, "the probe read no row"
    assert set(left) <= {None, ""}, f"scope outlived its transaction: {left}"


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


def test_isolation_holds_for_every_read_shape(concepts: ConceptStore) -> None:
    """A policy can be right for one query shape and wrong for another."""
    concepts.create(
        A,
        Concept(
            path="x/y",
            type="Concept",
            title="secret",
            body="tenant a only",
            links=("x/z",),
        ),
        "seed",
    )
    # The positive control: every B assertion below also holds if nothing was written.
    assert concepts.read(A, "x/y") is not None
    assert concepts.list_(A, "x/") == [("x/y", "Concept")]
    assert [h.path for h in concepts.search(A, "secret", limit=10, prefix=None)] == [
        "x/y"
    ]
    assert [p for p, _ in concepts.grep(A, "tenant a only", limit=10)] == ["x/y"]
    assert concepts.backlinks(A, "x/z") == ["x/y"]

    assert concepts.read(B, "x/y") is None
    assert concepts.list_(B, "x/") == []
    assert concepts.search(B, "secret", limit=10, prefix=None) == []
    assert concepts.grep(B, "tenant a only", limit=10) == []
    assert concepts.backlinks(B, "x/z") == []


def test_unset_scope_raises_rather_than_returning_everything(store: Store) -> None:
    """Either SQLSTATE is correct: a never-assigned GUC is undefined, a reset one ''."""
    with (
        pytest.raises((psycopg.errors.UndefinedObject, psycopg.errors.DataError)),
        store.raw() as c,
    ):
        c.execute("SELECT count(*) FROM okf.concept").fetchone()


def test_raw_is_read_only_and_not_merely_documented(store: Store) -> None:
    """`raw()` is the one unscoped connection in the codebase, so a write through it
    would reach every tenant's rows at once.

    `SET TRANSACTION READ ONLY` outside a transaction block is a Postgres *warning*,
    not an error — so a refactor that moved it could silently downgrade enforcement to
    advisory with every other test still green. The statement is DDL, because a write
    to `concept` is refused by the policy first and would fail either way.
    """
    with pytest.raises(psycopg.errors.ReadOnlySqlTransaction), store.raw() as c:
        c.execute("CREATE TABLE okf.should_never_exist (x int)")


def test_admin_scope_reads_every_tenant(store: Store) -> None:
    one, two = uuid.uuid4(), uuid.uuid4()
    paths = [f"admin/{one}", f"admin/{two}"]
    for tenant, path in zip((one, two), paths, strict=True):
        with store.scope(tenant) as c:
            _insert(c, tenant, path)

    # The positive control: the same query under a tenant scope sees one of the two.
    with store.scope(one) as c:
        assert _owners(c, paths) == {one}

    scoped: set[int] = set()
    with store.admin_scope() as c:
        scoped.add(id(c))
        assert _owners(c, paths) == {one, two}
    # Both GUCs are SET LOCAL, so neither may ride this connection back out.
    _probe_until_a_scoped_connection_returns(store, scoped)


def test_admin_scope_cannot_write(store: Store) -> None:
    """The admin policy is FOR SELECT, so it is never consulted for an UPDATE and
    an admin's write stays scoped to the nil tenant.

    That alone would match no rows silently, which reads to a caller as a write that
    succeeded and changed nothing. The read-only transaction is what raises here, so
    the class is pinned: a bare Error also catches the UndefinedColumn a renamed
    `title` would raise, and would stay green with READ ONLY gone.
    """
    with (
        store.admin_scope() as c,
        pytest.raises(psycopg.errors.ReadOnlySqlTransaction),
    ):
        c.execute("UPDATE okf.concept SET title = 'x'")


def test_the_admin_guc_does_not_widen_a_write(store: Store) -> None:
    """Pins the FOR SELECT half, which the test above cannot reach.

    That one raises because the transaction is read-only, so it would still pass if
    admin_read were widened to FOR ALL. This sets the admin GUC on an ordinary
    writable connection, where only the policy's command scope stops the UPDATE.
    """
    one, two = uuid.uuid4(), uuid.uuid4()
    paths = [f"write/{one}", f"write/{two}"]
    for tenant, path in zip((one, two), paths, strict=True):
        with store.scope(tenant) as c:
            _insert(c, tenant, path)

    with store.scope(one) as c:
        c.execute("SELECT set_config('okf.admin', 'on', true)")
        # No WHERE, so the count is exactly the rows the policies let it reach.
        updated = c.execute("UPDATE okf.concept SET title = 'claimed'").rowcount

    assert updated == 1, f"the admin GUC widened an UPDATE to {updated} rows"
    with store.admin_scope() as c:
        titles = c.execute(
            "SELECT path, title FROM okf.concept WHERE path = ANY(%s)", (paths,)
        ).fetchall()
    assert dict(titles) == {paths[0]: "claimed", paths[1]: ""}
