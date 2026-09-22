"""The four read paths an agent uses to find knowledge.

`search` returns cards, never bodies: that is what keeps agent context small.
"""

import uuid
from dataclasses import astuple
from datetime import UTC, datetime

import pytest

from keepsake.store import concepts as concepts_module
from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store
from okf_core import Concept, extract_links

_SEED = [
    (
        "detect/dormant",
        "Dormant Rule Identification",
        "Detection rules that have not fired in 90 days.",
    ),
    (
        "auth/flow",
        "OAuth2 Authorization Flow",
        "Standardised on PKCE for client authentication.",
    ),
    (
        "splunk/cursor",
        "Splunk Position Cursor",
        "The poller advances an index-time cursor. See [dormant](../detect/dormant.md).",
    ),
]


def _seed(concepts: ConceptStore, tenant: uuid.UUID) -> None:
    for path, title, body in _SEED:
        concepts.create(
            tenant,
            Concept(
                path=path,
                type="Concept",
                title=title,
                body=body,
                links=extract_links(body, path),
            ),
            "seed",
        )


def test_read_round_trips_a_created_concept(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    c = Concept(
        path="a/b",
        type="Concept",
        title="T",
        description="D",
        body="B [x](../detect/dormant.md)",
        frontmatter={"owner": "sec"},
        links=("detect/dormant",),
    )
    concepts.create(tenant, c, "seed")
    assert concepts.read(tenant, "a/b") == c


def test_read_returns_none_for_an_unknown_path(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    assert concepts.read(tenant, "detect/nothing") is None


def test_search_ors_terms_rather_than_anding(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """AND semantics returned nothing for realistic queries. OR plus rank recovers them."""
    _seed(concepts, tenant)
    hits = concepts.search(tenant, "cursors stalling", limit=10, prefix=None)
    assert [h.path for h in hits][:1] == ["splunk/cursor"]


def test_search_ranks_more_matching_terms_higher(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    hits = concepts.search(tenant, "dormant detection rules", limit=10, prefix=None)
    assert hits[0].path == "detect/dormant"


def test_search_never_returns_a_body(concepts: ConceptStore, tenant: uuid.UUID) -> None:
    """The values, not `hasattr`. `Hit` is slots=True, so an attribute check is
    guaranteed by the dataclass rather than by the query — it would pass on a `Hit`
    whose `description` held the whole body."""
    secret = "zqxjkbody"
    concepts.create(
        tenant,
        Concept(path="detect/secret", type="Concept", title="Dormant", body=secret),
        "seed",
    )
    hit = concepts.search(tenant, "dormant", limit=1, prefix=None)[0]
    assert secret not in "".join(str(v) for v in astuple(hit))
    assert (hit.path, hit.type, hit.title) == ("detect/secret", "Concept", "Dormant")


def test_search_respects_limit(concepts: ConceptStore, tenant: uuid.UUID) -> None:
    _seed(concepts, tenant)
    q = "cursor detection authentication"
    assert len(concepts.search(tenant, q, limit=10, prefix=None)) == len(_SEED)
    assert len(concepts.search(tenant, q, limit=2, prefix=None)) == 2
    # Negative, not zero: Postgres answers LIMIT 0 with no rows by itself, so only a
    # negative limit reaches the guard.
    assert concepts.search(tenant, q, limit=-1, prefix=None) == []


def test_search_confines_hits_to_the_prefix(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    hits = concepts.search(
        tenant, "cursor detection authentication", limit=10, prefix="detect/"
    )
    assert [h.path for h in hits] == ["detect/dormant"]


# Both concepts mention "dormant"; the one carrying it in its title outranks the other.
_DORMANT_HITS = ["detect/dormant", "splunk/cursor"]


def test_search_is_case_insensitive(concepts: ConceptStore, tenant: uuid.UUID) -> None:
    """A tokeniser restricted to lowercase would drop the query entirely."""
    _seed(concepts, tenant)
    hits = concepts.search(tenant, "DORMANT", limit=10, prefix=None)
    assert [h.path for h in hits] == _DORMANT_HITS


def test_search_treats_tsquery_syntax_as_text(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """Untokenised caller text reaches to_tsquery as syntax and raises."""
    assert concepts.search(tenant, "dormant | ) & !(", limit=10, prefix=None) == []
    _seed(concepts, tenant)
    hits = concepts.search(tenant, "dormant | ) & !(", limit=10, prefix=None)
    assert [h.path for h in hits] == _DORMANT_HITS


def test_search_of_a_termless_query_is_empty_not_invalid(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    assert concepts.search(tenant, "  !!!  ", limit=10, prefix=None) == []


def test_grep_matches_a_regex_and_is_limited(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    assert [p for p, _ in concepts.grep(tenant, "index-time", limit=10)] == [
        "splunk/cursor"
    ]
    assert [p for p, _ in concepts.grep(tenant, "inde.-tim[ez]", limit=10)] == [
        "splunk/cursor"
    ]
    assert len(concepts.grep(tenant, "[a-z]", limit=10)) == len(_SEED)
    assert len(concepts.grep(tenant, "[a-z]", limit=2)) == 2
    assert concepts.grep(tenant, "index-time", limit=-1) == []


def test_grep_matches_titles_as_well_as_bodies(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    assert [p for p, _ in concepts.grep(tenant, "OAuth2", limit=10)] == ["auth/flow"]
    assert [p for p, _ in concepts.grep(tenant, "oauth2", limit=10)] == ["auth/flow"]


def test_grep_snippet_carries_surrounding_context(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """The match alone tells an agent nothing about relevance."""
    _seed(concepts, tenant)
    [(_, snippet)] = concepts.grep(tenant, "index-time", limit=10)
    assert "poller advances an index-time cursor" in snippet


def test_grep_snippet_is_bounded_and_single_line(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """A snippet is a card, not a body. The match is early, so the tail must be cut."""
    body = "needle here\n" + "\n".join(f"filler line {i}" for i in range(400))
    concepts.create(
        tenant, Concept(path="big/one", type="Concept", title="Big", body=body), "seed"
    )
    [(_, snippet)] = concepts.grep(tenant, "needle", limit=10)
    assert "needle" in snippet
    assert len(snippet) <= 200
    assert "\n" not in snippet


@pytest.mark.parametrize(
    "pattern",
    [
        "index-time(",
        # Postgres refuses to compile this one rather than scanning with it.
        "((((((((((a{1,10}){1,10}){1,10}){1,10}){1,10}){1,10}){1,10}){1,10}){1,10}){1,10}",
    ],
)
def test_grep_rejects_an_uncompilable_pattern(
    concepts: ConceptStore, tenant: uuid.UUID, pattern: str
) -> None:
    _seed(concepts, tenant)
    with pytest.raises(ValueError, match="regular expression"):
        concepts.grep(tenant, pattern, limit=10)


def test_grep_is_cancelled_rather_than_holding_the_pod(
    store: Store,
    concepts: ConceptStore,
    tenant: uuid.UUID,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """Every store call blocks the pod's event loop, so an expensive pattern must be
    the caller's problem and not every sibling agent's."""
    with store.scope(tenant) as conn:
        conn.execute(
            "INSERT INTO concept (tenant_id, path, type, body) "
            "SELECT %s, 'p/' || g, 'Concept', repeat('lorem ipsum dolor ', 100) "
            "FROM generate_series(1, 2000) g",
            (tenant,),
        )
    # The scan costs ~10ms, so 1ms cancels with an order of magnitude to spare.
    monkeypatch.setattr(concepts_module, "_GREP_TIMEOUT_MS", 1)
    with pytest.raises(ValueError, match="took longer than 1ms"):
        concepts.grep(tenant, "(lorem|ipsum|dolor)+ z", limit=10)


def test_backlinks_are_computed_not_stored(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    assert concepts.backlinks(tenant, "detect/dormant") == ["splunk/cursor"]
    assert concepts.backlinks(tenant, "auth/flow") == []


def test_list_returns_children_of_prefix(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    assert concepts.list_(tenant, "detect/") == [("detect/dormant", "Concept")]
    assert len(concepts.list_(tenant, "")) == len(_SEED)


def test_list_treats_the_prefix_literally(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """LIKE would read the underscore as a wildcard."""
    for path in ("a_b/one", "axb/two"):
        concepts.create(tenant, Concept(path=path, type="Concept"), "seed")
    assert [p for p, _ in concepts.list_(tenant, "a_b/")] == ["a_b/one"]


# The admin console's read paths. `tenant_id=None` means every tenant and must take
# `admin_scope()`; a concrete id must never fall through to it.


def test_page_returns_summaries_under_prefix(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    page = concepts.page(tenant, "detect/", limit=10, offset=0)
    assert [s.path for s in page] == ["detect/dormant"]
    assert (page[0].type, page[0].title) == ("Concept", "Dormant Rule Identification")


def test_page_respects_limit_and_offset(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    first = concepts.page(tenant, "", limit=2, offset=0)
    second = concepts.page(tenant, "", limit=2, offset=2)
    assert [s.path for s in first] == ["auth/flow", "detect/dormant"]
    assert [s.path for s in second] == ["splunk/cursor"]


def test_page_of_a_negative_limit_is_empty_not_invalid(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """Negative, not zero: Postgres answers LIMIT 0 with no rows by itself, so only a
    negative limit reaches the guard."""
    _seed(concepts, tenant)
    assert concepts.page(tenant, "", limit=-1, offset=0) == []


def test_count_matches_what_page_covers(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    assert concepts.count(tenant, "") == len(_SEED)
    assert concepts.count(tenant, "detect/") == 1


def test_page_of_one_tenant_never_returns_anothers_rows(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    other = uuid.uuid4()
    _seed(concepts, tenant)
    concepts.create(other, Concept(path="other/one", type="Concept"), "seed")
    page = concepts.page(tenant, "", limit=10, offset=0)
    assert "other/one" not in [s.path for s in page]
    assert {s.tenant_id for s in page} == {tenant}


def test_page_of_every_tenant_returns_both(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    other = uuid.uuid4()
    _seed(concepts, tenant)
    concepts.create(other, Concept(path="other/one", type="Concept"), "seed")
    page = concepts.page(None, "", limit=1000, offset=0)
    tenants_seen = {s.tenant_id for s in page}
    assert tenant in tenants_seen
    assert other in tenants_seen


def test_page_of_every_tenant_breaks_a_tied_path_by_tenant_id(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """Two tenants can hold the same path, so under admin_scope() `path` alone is
    not a total order. A test seeding distinct paths across tenants wouldn't catch
    a missing tie-breaker: this one seeds the same path twice and pages one row at
    a time, so a non-total order would return the same row on both pages, or skip
    one of them, instead of covering both exactly once."""
    other = uuid.uuid4()
    path = "decisions/retry-policy"
    concepts.create(tenant, Concept(path=path, type="Concept"), "seed")
    concepts.create(other, Concept(path=path, type="Concept"), "seed")

    first = concepts.page(None, path, limit=1, offset=0)
    second = concepts.page(None, path, limit=1, offset=1)

    assert [s.path for s in first] == [path]
    assert [s.path for s in second] == [path]
    assert first[0].tenant_id != second[0].tenant_id
    assert {first[0].tenant_id, second[0].tenant_id} == {tenant, other}


def test_totals_counts_concepts_types_revisions_links_and_orphans(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    totals = concepts.totals(tenant)
    assert totals.concepts == len(_SEED)
    assert totals.by_type == {"Concept": len(_SEED)}
    assert totals.revisions == len(_SEED)
    # Only splunk/cursor carries a link, to detect/dormant.
    assert totals.links == 1
    # detect/dormant has an inbound link; the other two don't.
    assert totals.orphans == 2


def test_activity_returns_recent_revisions_newest_first(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    revs = concepts.activity(tenant, limit=10)
    # _seed writes dormant, then flow, then cursor: newest-first reverses that.
    assert [r.path for r in revs] == ["splunk/cursor", "auth/flow", "detect/dormant"]


def test_activity_of_every_tenant_tags_each_row_with_its_own_tenant(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """A path is only unique within a tenant, so two tenants can both write
    `decisions/policy`. Without `tenant_id` on each row, an all-tenants feed
    would render both writes as the same concept; a test using distinct paths
    across tenants wouldn't catch that."""
    other = uuid.uuid4()
    concepts.create(tenant, Concept(path="decisions/policy", type="Concept"), "seed")
    concepts.create(other, Concept(path="decisions/policy", type="Concept"), "seed")

    revs = concepts.activity(None, limit=10)

    same_path = [r for r in revs if r.path == "decisions/policy"]
    assert len(same_path) == 2
    assert {r.tenant_id for r in same_path} == {tenant, other}


def test_revisions_for_is_immune_to_other_paths_crowding_the_feed(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """`activity()`'s cap is tenant-wide, so a quiet concept can fall off it even
    with a perfectly good history of its own. `revisions_for` filters in SQL by
    path, so noise on other paths can never push a concept's own history out."""
    concepts.create(
        tenant, Concept(path="detect/dormant", type="Concept", title="v1"), "seed"
    )
    concepts.update(
        tenant, Concept(path="detect/dormant", type="Concept", title="v2"), "seed", 1
    )
    # More noisy revisions on other paths than the limit passed below: with a
    # tenant-wide scan, these alone would crowd "detect/dormant" out entirely.
    for i in range(5):
        concepts.create(
            tenant, Concept(path=f"noise/{i}", type="Concept", title="noise"), "seed"
        )

    revs = concepts.revisions_for(tenant, "detect/dormant", limit=3)

    assert [r.version for r in revs] == [2, 1]
    assert all(r.tenant_id == tenant for r in revs)


def test_daily_writes_groups_by_day_and_zero_fills_gaps(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    _seed(concepts, tenant)
    writes = concepts.daily_writes(tenant, days=7)
    assert len(writes) == 7
    assert writes[-1] == (datetime.now(UTC).date(), len(_SEED))
    assert all(count == 0 for _, count in writes[:-1])


def test_tenants_lists_every_tenant_with_its_concept_count(
    concepts: ConceptStore,
) -> None:
    a, b = uuid.uuid4(), uuid.uuid4()
    concepts.create(a, Concept(path="a/one", type="Concept"), "seed")
    concepts.create(a, Concept(path="a/two", type="Concept"), "seed")
    concepts.create(b, Concept(path="b/one", type="Concept"), "seed")
    counts = dict(concepts.tenants())
    assert counts[a] == 2
    assert counts[b] == 1


def test_graph_marks_a_missing_link_target_as_not_existing(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    concepts.create(
        tenant,
        Concept(path="a/one", type="Concept", links=("missing/target",)),
        "seed",
    )
    graph = concepts.graph(tenant, "", limit=10)
    target = next(n for n in graph.nodes if n.path == "missing/target")
    assert target.exists is False
    assert ("a/one", "missing/target") in graph.edges


def test_graph_reports_a_target_outside_a_capped_page_as_existing(
    concepts: ConceptStore, tenant: uuid.UUID
) -> None:
    """The bug this design exists to prevent: a link target with a real row, just not
    on the page a `limit` cap returned, must not be reported as missing."""
    # "b/target" sorts after "a/one", so limit=1 pages in only "a/one".
    concepts.create(tenant, Concept(path="b/target", type="Concept"), "seed")
    concepts.create(
        tenant,
        Concept(path="a/one", type="Concept", links=("b/target",)),
        "seed",
    )
    graph = concepts.graph(tenant, "", limit=1)
    assert graph.truncated is True
    assert [n.path for n in graph.nodes if n.path == "a/one"] == ["a/one"]
    target = next(n for n in graph.nodes if n.path == "b/target")
    assert target.exists is True
