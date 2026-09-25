"""The benchmarks' scoring, against hand-written inputs: no Claude, no Postgres."""

from __future__ import annotations

import json
import math
import subprocess
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "bench"))
import beir
import grade
import run


def test_ndcg_and_recall_match_hand_computed_values() -> None:
    relevant = {"a": 2, "b": 1, "z": 1}
    # DCG = 2/log2(2) + 1/log2(4); IDCG = 2/log2(2) + 1/log2(3) + 1/log2(4).
    assert math.isclose(
        beir.ndcg(["a", "x", "b"], relevant, 10), 2.5 / (2 + 1 / math.log2(3) + 0.5)
    )
    assert beir.ndcg(["x"], relevant, 10) == 0.0
    assert beir.recall(["a", "x", "b"], relevant, 2) == 1 / 3
    assert beir.recall(["a", "x", "b"], relevant, 10) == 2 / 3


def _line(kind: str, *blocks: dict) -> str:
    return json.dumps({"type": kind, "message": {"content": list(blocks)}})


TRANSCRIPT = [
    json.dumps({"type": "system", "subtype": "init"}),
    _line(
        "assistant",
        {
            "type": "tool_use",
            "id": "1",
            "name": "mcp__keepsake__okf_search",
            "input": {"query": "failover runbook", "limit": 5},
        },
    ),
    _line("user", {"type": "tool_result", "tool_use_id": "1", "is_error": False}),
    _line(
        "assistant",
        {
            "type": "tool_use",
            "id": "2",
            "name": "mcp__keepsake__okf_read",
            "input": {"path": "/runbooks/db-failover.md"},
        },
    ),
    _line("user", {"type": "tool_result", "tool_use_id": "2", "is_error": False}),
    _line(
        "assistant",
        {
            "type": "tool_use",
            "id": "3",
            "name": "mcp__keepsake__okf_create",
            "input": {"path": "incidents/x"},
        },
    ),
    _line("user", {"type": "tool_result", "tool_use_id": "3", "is_error": True}),
    "not json",
    json.dumps(
        {
            "type": "result",
            "result": "Run pg_ctl promote.",
            "num_turns": 4,
            "total_cost_usd": 0.05,
            "duration_ms": 9000,
            "usage": {
                "input_tokens": 3,
                "cache_read_input_tokens": 100,
                "output_tokens": 20,
            },
        }
    ),
]


def test_parse_keeps_keepsake_calls_in_order_and_marks_errors() -> None:
    calls, result = grade.parse(TRANSCRIPT)
    assert [c["tool"] for c in calls] == ["okf_search", "okf_read", "okf_create"]
    assert [c["error"] for c in calls] == [False, False, True]
    assert result["result"] == "Run pg_ctl promote."


def test_grade_normalizes_read_paths_and_diffs_the_store() -> None:
    calls, result = grade.parse(TRANSCRIPT)
    before = {"runbooks/db-failover": "page `#dba-oncall`"}
    after = {
        "runbooks/db-failover": "page `#data-platform-oncall`",
        "incidents/x": "see /services/search-indexer.md",
    }
    expect = {
        "reads": ["runbooks/db-failover"],
        "answer": ["pg_ctl promote", "never said"],
        "search_before_write": True,
        "after": {
            "added": 1,
            "new": [{"prefix": "incidents/", "body": ["/services/search-indexer"]}],
            "changed": {
                "runbooks/db-failover": {
                    "body": ["data-platform-oncall"],
                    "body_not": ["dba-oncall"],
                }
            },
        },
    }
    checks = grade.grade(expect, calls, result["result"], before, after)
    assert checks == {
        "used keepsake": True,
        "read runbooks/db-failover": True,
        "answer ~ /pg_ctl promote/": True,
        "answer ~ /never said/": False,
        "looked before writing": True,
        "nothing deleted": True,
        "added 1": True,
        "new incidents/*": True,
        "runbooks/db-failover updated": True,
    }


def test_load_bundle_skips_the_files_export_generates(tmp_path: Path) -> None:
    (tmp_path / "runbooks").mkdir()
    (tmp_path / "runbooks" / "db-failover.md").write_text("body")
    (tmp_path / "index.md").write_text("generated")
    (tmp_path / "log.md").write_text("generated")
    assert grade.load_bundle(tmp_path) == {"runbooks/db-failover": "body"}


def test_a_write_first_fails_the_look_before_writing_check() -> None:
    calls = [
        {"tool": "okf_create", "input": {}, "error": False},
        {"tool": "okf_search", "input": {}, "error": False},
    ]
    assert grade.grade({"search_before_write": True}, calls, "", {}, {}) == {
        "used keepsake": True,
        "looked before writing": False,
    }


def test_metrics_and_summary() -> None:
    calls, result = grade.parse(TRANSCRIPT)
    m = grade.metrics(calls, result)
    assert m["input_tokens"] == 103
    assert m["search_queries"] == ["failover runbook"]
    rows = [
        {
            "variant": "v",
            "task": "t",
            "trial": 1,
            "passed": False,
            "checks": {"judge": False},
            **m,
        }
    ]
    report = grade.summarize(rows)
    assert "| v | 0/1 | 1/1 |" in report
    assert "- v / t #1: judge" in report


def test_a_longmemeval_task_carries_the_official_judge_prompt() -> None:
    import longmemeval

    base = {
        "question": "How many days?",
        "answer": 18,
        "question_date": "2023/05/30 (Tue) 23:40",
    }
    bundle = longmemeval.OUT / "bundles" / "q"
    temporal = longmemeval.task(
        {**base, "question_id": "q", "question_type": "temporal-reasoning"}, bundle
    )
    assert temporal["kind"] == "temporal-reasoning"
    assert temporal["prompt"] == "Today is 2023/05/30 (Tue) 23:40. How many days?"
    template = temporal["expect"]["judge_template"]
    assert (
        "off-by-one" in template
        and "Correct Answer: 18" in template
        and grade.RESPONSE in template
    )
    abstained = longmemeval.task(
        {**base, "question_id": "q_abs", "question_type": "multi-session"}, bundle
    )
    assert abstained["kind"] == "abstention"
    assert "unanswerable" in abstained["expect"]["judge_template"]


def test_curated_pairs_each_question_with_one_variants_first_trial(
    tmp_path: Path, monkeypatch
) -> None:
    import longmemeval

    monkeypatch.setattr(longmemeval, "OUT", tmp_path)
    monkeypatch.setattr(grade, "load_bundle", lambda _: {"user/profile": ""})
    (tmp_path / "tune.json").write_text(json.dumps([{"id": "q", "bundle": "raw"}]))
    run = tmp_path / "run"
    run.mkdir()
    rows = [
        {"variant": "baseline", "task": "q", "trial": 2, "export": "b2"},
        {"variant": "baseline", "task": "q", "trial": 1, "export": "b1"},
        {"variant": "terse-tools", "task": "q", "trial": 1, "export": "t1"},
    ]
    (run / "results.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows))
    longmemeval.curated("tune", run)
    paired = json.loads((tmp_path / "tune-curated.json").read_text())
    assert [t["bundle"] for t in paired] == ["b1"]


def test_normalize_drops_case_punctuation_and_articles() -> None:
    assert grade.normalize("  The   U.S. Army, an Army! ") == "us army army"


def test_answer_any_matches_any_alias_after_normalizing() -> None:
    expect = {"answer_any": ["Swiss Confederation", "Switzerland"]}
    assert grade.grade(expect, [], "It was in SWITZERLAND.", {}, {}, require_tools=False) == {
        "answer ~ any alias": True
    }
    assert grade.grade(expect, [], "In France.", {}, {}, require_tools=False) == {
        "answer ~ any alias": False
    }


def test_answer_any_matches_whole_words_only() -> None:
    for alias, answer in (("Hu", "It was the church."), ("US", "I trust it"), ("45", "In 1945.")):
        checks = grade.grade({"answer_any": [alias]}, [], answer, {}, {}, require_tools=False)
        assert checks == {"answer ~ any alias": False}, (alias, answer)
    checks = grade.grade({"answer_any": ["Hu"]}, [], "It was Hu Jintao.", {}, {}, require_tools=False)
    assert checks == {"answer ~ any alias": True}


def test_hyphens_and_slashes_separate_words() -> None:
    checks = grade.grade({"answer_any": ["Jean-Paul Sartre"]}, [], "Jean Paul Sartre.", {}, {}, require_tools=False)
    assert checks == {"answer ~ any alias": True}


def test_an_alias_that_normalizes_to_nothing_never_matches() -> None:
    checks = grade.grade({"answer_any": ["The", "..."]}, [], "anything", {}, {}, require_tools=False)
    assert checks == {"answer ~ any alias": False}


def test_fetch_refuses_a_file_with_the_wrong_hash(tmp_path: Path) -> None:
    src = tmp_path / "src.txt"
    src.write_text("data")
    dest = tmp_path / "dest.txt"
    with pytest.raises(ValueError):
        grade.fetch(src.as_uri(), dest, "0" * 64)
    assert not dest.exists()
    good = "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
    assert grade.fetch(src.as_uri(), dest, good).read_text() == "data"


def test_resolve_ref_returns_the_commit_sha() -> None:
    sha = run.resolve_ref("HEAD")
    assert len(sha) == 40 and all(ch in "0123456789abcdef" for ch in sha)


def test_resolve_ref_refuses_an_unknown_ref() -> None:
    with pytest.raises(subprocess.CalledProcessError):
        run.resolve_ref("no-such-ref-anywhere")


def _read(path: str, links: list, backlinks: list) -> dict:
    return {"tool": "okf_read", "input": {"path": path}, "error": False,
            "output": json.dumps({"path": path, "links": links, "backlinks": backlinks})}


def test_link_metrics_count_link_only_reads_and_missed_links() -> None:
    calls = [
        {"tool": "okf_search", "input": {"query": "q"}, "error": False,
         "output": json.dumps({"results": [{"path": "a"}, {"path": "b"}]})},
        _read("a", ["b", "c"], ["d"]),
        # b came from search too, so reading it is not link-only.
        _read("b", [], []),
        # c came only from a's links.
        _read("c", [{"path": "e", "title": "E"}], []),
    ]
    # d and e were offered and never read; only d was required.
    m = grade.link_metrics(calls, required=["c", "d", "z"])
    assert m == {"reads": 3, "offered": 4, "link_only_reads": 1, "missed_links": 1}


def test_summary_tolerates_rows_without_link_metrics() -> None:
    row = {"variant": "v", "task": "t", "trial": 1, "passed": True, "checks": {},
           **grade.metrics([], {})}
    for key in ("reads", "offered", "followed", "link_only_reads", "missed_links"):
        row.pop(key, None)
    assert "| v | 1/1 |" in grade.summarize([row])
