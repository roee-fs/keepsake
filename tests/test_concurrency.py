"""Writes and the optional compare-and-swap.

Agents hold a read-then-write open across an LLM call, so the version predicate is
the only thing standing between two of them and a silently clobbered edit.
"""

import datetime
import threading
import uuid
from concurrent.futures import ThreadPoolExecutor

import pytest

from keepsake.store.concepts import ConceptStore, Conflict
from keepsake.store.pool import Store
from okf_core import Concept, parse

T = uuid.uuid4()


def _c(path: str, body: str = "") -> Concept:
    return Concept(path=path, type="Concept", body=body)


def test_create_returns_none_when_path_taken(concepts: ConceptStore) -> None:
    assert concepts.create(T, _c("a/b", "one"), "agent") == 1
    assert concepts.create(T, _c("a/b", "one"), "agent") is None


def test_update_with_stale_version_returns_current_content(
    concepts: ConceptStore,
) -> None:
    """Also the sequential compare-and-swap case: two writes off the same base."""
    concepts.create(T, _c("a/c", "v1"), "agent")
    assert concepts.update(T, _c("a/c", "v2"), "agent", 1) == 2
    result = concepts.update(T, _c("a/c", "v3"), "agent", 1)
    assert isinstance(result, Conflict)
    assert result.current_version == 2
    assert result.current_body == "v2"


def test_update_without_expected_version_is_last_write_wins(
    concepts: ConceptStore, store: Store
) -> None:
    concepts.create(T, _c("a/d", "v1"), "agent")
    assert concepts.update(T, _c("a/d", "v2"), "agent", None) == 2
    with store.scope(T) as conn:
        assert conn.execute(
            "SELECT body FROM okf.concept WHERE path = 'a/d'"
        ).fetchone() == ("v2",)


def test_update_of_an_absent_path_raises_without_disclosing_a_body(
    concepts: ConceptStore,
) -> None:
    with pytest.raises(KeyError) as excinfo:
        concepts.update(T, _c("a/missing", "secret"), "agent", None)
    assert "a/missing" in str(excinfo.value)
    assert "secret" not in str(excinfo.value)


def test_exactly_one_of_two_concurrent_updates_wins(concepts: ConceptStore) -> None:
    """Two transactions open at once on two connections.

    The loser blocks on the winner's row lock and, under READ COMMITTED, re-checks
    the version predicate against the committed row — so it updates nothing.
    """
    concepts.create(T, _c("a/e", "v1"), "agent")
    ready = threading.Barrier(2)

    def race(actor: str, body: str) -> int | Conflict:
        ready.wait(timeout=10)
        return concepts.update(T, _c("a/e", body), actor, 1)

    with ThreadPoolExecutor(max_workers=2) as pool:
        futures = [pool.submit(race, "a1", "x"), pool.submit(race, "a2", "y")]
        results = [f.result(timeout=30) for f in futures]

    assert [r for r in results if not isinstance(r, Conflict)] == [2]
    assert [r.current_version for r in results if isinstance(r, Conflict)] == [2]


def test_every_write_appends_a_revision(concepts: ConceptStore, store: Store) -> None:
    concepts.create(T, _c("a/f", "v1"), "agent")
    concepts.update(T, _c("a/f", "v2"), "agent", 1)
    with store.scope(T) as conn:
        rows = conn.execute(
            "SELECT version, op, snapshot->>'body' FROM okf.concept_revision "
            "WHERE path = 'a/f' ORDER BY version"
        ).fetchall()
    assert rows == [(1, "create", "v1"), (2, "update", "v2")]


def test_a_conflicting_update_appends_no_revision(
    concepts: ConceptStore, store: Store
) -> None:
    concepts.create(T, _c("a/g", "v1"), "agent")
    assert isinstance(concepts.update(T, _c("a/g", "v2"), "agent", 99), Conflict)
    with store.scope(T) as conn:
        assert conn.execute(
            "SELECT count(*) FROM okf.concept_revision WHERE path = 'a/g'"
        ).fetchone() == (1,)


def test_frontmatter_carrying_a_date_survives_a_write(
    concepts: ConceptStore, store: Store
) -> None:
    """YAML loads an unquoted date as `datetime.date`, which json.dumps rejects.

    It survives as a string, so such a document no longer round-trips byte-identically.
    """
    c = parse("---\ntype: Concept\nreviewed: 2026-01-01\n---\nbody\n", "a/h")
    assert c.frontmatter == {"reviewed": datetime.date(2026, 1, 1)}
    assert concepts.create(T, c, "agent") == 1
    with store.scope(T) as conn:
        row = conn.execute(
            "SELECT frontmatter, snapshot->'frontmatter' FROM okf.concept "
            "JOIN okf.concept_revision USING (tenant_id, path, version) "
            "WHERE path = 'a/h'"
        ).fetchone()
    assert row == ({"reviewed": "2026-01-01"}, {"reviewed": "2026-01-01"})
