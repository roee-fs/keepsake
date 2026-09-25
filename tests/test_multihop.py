"""The multi-hop dataset converters, against hand-written records: no downloads."""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "bench"))
import iirc
import musique


def _para(idx: int, title: str, text: str, supporting: bool = False) -> dict:
    return {"idx": idx, "title": title, "paragraph_text": text, "is_supporting": supporting}


RECORD = {
    "id": "2hop__1_2",
    "question": "Which label released the debut album of Grant Green?",
    "answer": "Blue Note",
    "answer_aliases": ["Blue Note Records"],
    "paragraphs": [
        _para(0, "Grant Green", "Grant Green was a jazz guitarist from St. Louis (Missouri).", True),
        _para(1, "Grant's First Stand", "Grant's First Stand is the debut album by Grant Green on Blue Note.", True),
        _para(2, "Green", "Green is a colour. Grant Green liked green."),
        _para(3, "St. Louis (Missouri)", "A city."),
        _para(4, "Green", "A second paragraph titled Green."),
    ],
    "question_decomposition": [
        {"id": 1, "question": "debut album of Grant Green", "answer": "Grant's First Stand", "paragraph_support_idx": 0},
        {"id": 2, "question": "label of #1", "answer": "Blue Note", "paragraph_support_idx": 1},
    ],
}


def test_slug_is_lowercase_dashed_and_capped() -> None:
    assert musique.slug("Grant's First Stand!") == "grant-s-first-stand"
    assert len(musique.slug("x" * 99)) == 40


def test_link_prefers_the_longer_overlapping_title_and_matches_literally() -> None:
    text = "Grant Green was from St. Louis (Missouri)."
    got = musique.link(text, [("Green", "p/2-green"), ("Grant Green", "p/0-grant-green"),
                              ("St. Louis (Missouri)", "p/3-st-louis-missouri")])
    assert got == ("[Grant Green](/p/0-grant-green.md) was from "
                   "[St. Louis (Missouri)](/p/3-st-louis-missouri.md).")


def test_a_shorter_title_never_links_inside_any_mention_of_a_longer_one() -> None:
    got = musique.link("New York City is big. New York City again.",
                       [("New York City", "p/1-nyc"), ("New York", "p/2-ny")])
    assert got == "[New York City](/p/1-nyc.md) is big. New York City again."


def test_link_needs_a_whole_word_and_the_same_case() -> None:
    assert musique.link("Greenland and green.", [("Green", "p/2-green")]) == "Greenland and green."


def test_mention_bundle_links_other_titles_but_never_itself() -> None:
    bodies = musique.bodies(RECORD, "mention")
    assert bodies["p/0-grant-green"] == (
        "Grant Green was a jazz guitarist from [St. Louis (Missouri)](/p/3-st-louis-missouri.md).")
    # "Green" names two paragraphs; the mention links the lower idx.
    assert "[Grant Green](/p/0-grant-green.md) liked green" in bodies["p/2-green"]
    assert "[Green]" not in bodies["p/4-green"]


def test_chain_bundle_links_each_step_to_the_next_or_appends_one() -> None:
    bodies = musique.bodies(RECORD, "chain")
    assert bodies["p/0-grant-green"].endswith(
        "\n\nRelated: [Grant's First Stand](/p/1-grant-s-first-stand.md)")
    assert "](" not in bodies["p/1-grant-s-first-stand"]
    assert "](" not in bodies["p/2-green"]


def test_none_bundle_has_no_links() -> None:
    assert all("](" not in b for b in musique.bodies(RECORD, "none").values())


def test_task_reads_the_supporting_paragraphs_in_step_order() -> None:
    t = musique.task(RECORD, "mention")
    assert t["id"] == "2hop__1_2" and t["kind"] == "2hop" and t["bundle"] == "mention/2hop__1_2"
    assert t["expect"] == {"evidence": ["p/0-grant-green", "p/1-grant-s-first-stand"],
                           "answer_any": ["Blue Note", "Blue Note Records"]}
    assert t["prompt"] == RECORD["question"] and t["system_prompt"] == musique.HOST


PASSAGE = {
    "pid": "p1",
    "title": "Thomas Bain",
    "text": "Bain was born in London and studied in Geneva.",
    "links": [
        {"indices": [17, 23], "target": "London"},
        {"indices": [39, 45], "target": "University of Geneva"},
        {"indices": [0, 4], "target": "No Such Article"},
        {"indices": [40, 99], "target": "London"},  # out of range: skipped
    ],
    "questions": [],
}
ARTICLES = {
    "london": 'London is on the <a href="River%20thames">Thames</a> and near <a href="university%20of%20Geneva">Geneva</a>? <b>No</b> &amp; yes.',
    "university of geneva": 'A university in <a href="Switzerland">Switzerland</a>, far from <a href="London">London</a>.',
}


def _question(qid: str, passages: list[str], kind: str = "span") -> dict:
    return {
        "qid": qid,
        "question": "Where?",
        "answer": {"type": kind, "answer_spans": [{"text": "Switzerland"}, {"text": "Swiss"}]},
        "question_links": passages,
        "context": [{"passage": "main", "text": "x"}] + [{"passage": p, "text": "y"} for p in passages],
    }


def test_iirc_main_passage_links_its_spans_and_skips_bad_ones() -> None:
    concepts, _ = iirc.bundle(PASSAGE, ARTICLES, "linked")
    title, type_, body = concepts["main/thomas-bain"]
    assert (title, type_) == ("Thomas Bain", "Passage")
    assert body == ("Bain was born in [London](/a/london.md) and studied in "
                    "[Geneva](/a/university-of-geneva.md).")
    assert set(concepts) == {"main/thomas-bain", "a/london", "a/university-of-geneva"}


def test_iirc_articles_link_each_other_and_drop_other_html() -> None:
    concepts, _ = iirc.bundle(PASSAGE, ARTICLES, "linked")
    assert concepts["a/london"] == ("London", "Article",
        "London is on the Thames and near [Geneva](/a/university-of-geneva.md)? No & yes.")
    assert concepts["a/university-of-geneva"][2] == (
        "A university in Switzerland, far from [London](/a/london.md).")


def test_iirc_none_bundle_keeps_the_text_without_links() -> None:
    concepts, _ = iirc.bundle(PASSAGE, ARTICLES, "none")
    assert all("](" not in body for _, _, body in concepts.values())
    assert concepts["main/thomas-bain"][2] == PASSAGE["text"]


def test_iirc_slug_collisions_get_a_suffix() -> None:
    passage = {**PASSAGE, "links": [{"indices": [17, 23], "target": "London"},
                                    {"indices": [39, 45], "target": "London!"}]}
    _, paths = iirc.bundle(passage, {"london": "a", "london!": "b"}, "linked")
    assert paths == {"london": "a/london", "london!": "a/london-2"}


def test_iirc_task_reads_main_then_gold_passages_in_context_order() -> None:
    _, paths = iirc.bundle(PASSAGE, ARTICLES, "linked")
    t = iirc.task(PASSAGE, _question("q1", ["University of Geneva", "London"]), "linked", paths)
    assert t["kind"] == "2link+" and t["bundle"] == "linked/q1"
    assert t["expect"] == {"evidence": ["main/thomas-bain", "a/university-of-geneva", "a/london"],
                           "answer_any": ["Switzerland", "Swiss"]}
    assert iirc.task(PASSAGE, _question("q2", ["Nowhere"]), "linked", paths) is None


def test_iirc_eligible_needs_a_span_answer_and_a_linked_passage() -> None:
    assert iirc.eligible(_question("q", ["London"]))
    assert not iirc.eligible(_question("q", ["London"], kind="value"))
    assert not iirc.eligible(_question("q", []))


def test_sample_takes_per_hop_questions_of_each_depth() -> None:
    records = [{"id": f"{h}hop__{i}", "question_decomposition": [{}] * h} for h in (2, 3, 4) for i in range(5)]
    got = musique.sample(records, per_hop=2, seed=0)
    assert sorted(len(r["question_decomposition"]) for r in got) == [2, 2, 3, 3, 4, 4]
