"""The four read paths an agent uses to find knowledge.

`search` returns cards, never bodies: that is what keeps agent context small.
"""

import uuid

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
    _seed(concepts, tenant)
    hit = concepts.search(tenant, "dormant", limit=1, prefix=None)[0]
    assert not hasattr(hit, "body")
    assert (hit.path, hit.type, hit.title) == (
        "detect/dormant",
        "Concept",
        "Dormant Rule Identification",
    )


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
