"""The benchmarks' scoring, against hand-written inputs: no Claude, no Postgres."""

from __future__ import annotations

import json
import math
import os
import subprocess
import sys
import time
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
            "name": "mcp__keepsake__search",
            "input": {"query": "failover runbook", "limit": 5},
        },
    ),
    _line("user", {"type": "tool_result", "tool_use_id": "1", "is_error": False}),
    _line(
        "assistant",
        {
            "type": "tool_use",
            "id": "2",
            "name": "mcp__keepsake__read",
            "input": {"path": "/runbooks/db-failover.md"},
        },
    ),
    _line("user", {"type": "tool_result", "tool_use_id": "2", "is_error": False}),
    _line(
        "assistant",
        {
            "type": "tool_use",
            "id": "3",
            "name": "mcp__keepsake__create",
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
    assert [c["tool"] for c in calls] == ["search", "read", "create"]
    assert [c["error"] for c in calls] == [False, False, True]
    assert result["result"] == "Run pg_ctl promote."


def test_parse_names_a_pre_rename_servers_tools_as_the_current_ones() -> None:
    old = [line.replace("mcp__keepsake__", "mcp__keepsake__okf_") for line in TRANSCRIPT]
    assert [c["tool"] for c in grade.parse(old)[0]] == ["search", "read", "create"]


def test_only_a_quiet_agent_that_wrote_nothing_counts_as_stalled(tmp_path: Path) -> None:
    reads, writes = tmp_path / "reads.jsonl", tmp_path / "writes.jsonl"
    reads.write_text("\n".join(TRANSCRIPT[:5]))
    writes.write_text("\n".join(TRANSCRIPT))
    assert not run.stalled(reads)
    old = time.time() - run.STALL_S - 1
    for p in (reads, writes):
        os.utime(p, (old, old))
    assert run.stalled(reads)
    assert not run.stalled(writes)


def test_parse_counts_file_tools_over_the_exported_memory() -> None:
    calls, _ = grade.parse(
        [
            _line(
                "assistant",
                {
                    "type": "tool_use",
                    "id": "1",
                    "name": "Grep",
                    "input": {"pattern": "promote"},
                },
                {
                    "type": "tool_use",
                    "id": "2",
                    "name": "Read",
                    "input": {"file_path": "/tmp/x/memory/runbooks/db-failover.md"},
                },
                {"type": "tool_use", "id": "3", "name": "Bash", "input": {"command": "ls"}},
            ),
        ]
    )
    assert [c["tool"] for c in calls] == ["grep", "read"]
    checks = grade.grade({"reads": ["runbooks/db-failover"]}, calls, "", {}, {})
    assert checks == {"used keepsake": True, "read runbooks/db-failover": True}


def test_parse_skips_file_tools_outside_the_exported_memory(tmp_path: Path) -> None:
    root = tmp_path / "memory"
    calls, _ = grade.parse(
        [
            _line(
                "assistant",
                {"type": "tool_use", "id": "1", "name": "Grep", "input": {"pattern": "x"}},
                {"type": "tool_use", "id": "2", "name": "Read", "input": {"file_path": "/etc/hosts"}},
                {"type": "tool_use", "id": "3", "name": "Glob", "input": {"pattern": "*", "path": ".."}},
                {"type": "tool_use", "id": "4", "name": "Read", "input": {"file_path": f"{root}/a.md"}},
            ),
        ],
        root,
    )
    assert [c["tool"] for c in calls] == ["grep", "read"]
    assert calls[1]["input"]["path"] == "a.md"


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
        {"tool": "create", "input": {}, "error": False},
        {"tool": "search", "input": {}, "error": False},
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
    assert "| v | 0/1 | 0 | 1/1 |" in report
    assert "- v / t #1: judge" in report


def test_summary_shows_passes_per_trial() -> None:
    m = grade.metrics([], {})
    rows = [
        {"variant": "v", "task": t, "trial": n, "passed": ok, "checks": {}, **m}
        for t, n, ok in [("a", 1, True), ("b", 1, True), ("a", 2, True), ("b", 2, False)]
    ]
    assert "| v | 3/4 | 2 / 1 |" in grade.summarize(rows)


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


def test_stream_yields_each_instance_across_chunk_boundaries(tmp_path: Path) -> None:
    import longmemeval

    instances = [
        {"question_id": "a", "text": "brackets ] and [ commas, inside"},
        {"question_id": "b", "text": 'an escaped \\" quote and a } brace'},
        {"question_id": "c", "nested": [{"x": 1}, {"y": [2, 3]}]},
    ]
    path = tmp_path / "m.json"
    path.write_text(json.dumps(instances, indent=1))
    assert list(longmemeval.stream(path, chunk=7)) == instances
    # The opening bracket can arrive after a whole chunk of whitespace.
    path.write_text(" " * 20 + json.dumps(instances))
    assert list(longmemeval.stream(path, chunk=7)) == instances


def test_a_failed_download_leaves_no_dataset(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    import longmemeval

    parts = []

    def interrupted(cmd: list[str], check: bool) -> None:
        parts.append(cmd[cmd.index("-o") + 1])
        Path(parts[-1]).write_text("[{")
        raise subprocess.CalledProcessError(18, cmd)

    monkeypatch.setattr(longmemeval.subprocess, "run", interrupted)
    dest = tmp_path / "fresh" / "m.json"
    for _ in range(2):
        with pytest.raises(subprocess.CalledProcessError):
            longmemeval.fetch("https://example.com/m.json", dest)
    # Two runs that overlap MUST NOT share a temporary file, and neither leaves one behind.
    assert len(set(parts)) == 2
    assert list(dest.parent.iterdir()) == []


def test_stream_refuses_data_after_the_array(tmp_path: Path) -> None:
    import longmemeval

    path = tmp_path / "m.json"
    path.write_text('[{"a": 1}] {"b": 2}')
    with pytest.raises(ValueError, match="after its closing"):
        list(longmemeval.stream(path, chunk=4))


def test_m_sample_zero_is_refused_before_any_split_is_written(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    import longmemeval

    monkeypatch.setattr(sys, "argv", ["longmemeval.py", "--m-sample", "0"])
    monkeypatch.setattr(longmemeval, "write", lambda *a: pytest.fail("wrote a split"))
    with pytest.raises(SystemExit):
        longmemeval.main()


def test_a_split_outside_the_agent_directory_resolves_its_bundles(
    tmp_path: Path,
) -> None:
    import longmemeval

    instance = {
        "question_id": "q",
        "question_type": "single-session-user",
        "question": "Where?",
        "answer": "Oslo",
        "question_date": "2023/05/30 (Tue) 23:40",
        "haystack_session_ids": ["s1"],
        "haystack_dates": ["2023/05/01"],
        "haystack_sessions": [[{"role": "user", "content": "I moved to Oslo."}]],
    }
    full = tmp_path / "full"
    longmemeval.write(full, {"all": [instance]})
    [task] = json.loads((full / "all.json").read_text())
    # run.py resolves a bundle against its tasks file's directory.
    assert list((full / task["bundle"] / "session").glob("*.md"))


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


def test_evidence_counts_missed_links_but_is_not_a_pass_check() -> None:
    calls = [_read("a", ["b"], [])]
    assert grade.grade({"evidence": ["a", "b"]}, calls, "x", {}, {}) == {"used keepsake": True}
    assert grade.metrics(calls, {}, {"evidence": ["a", "b"]})["missed_links"] == 1
    assert grade.metrics(calls, {}, {"reads": ["a", "b"]})["missed_links"] == 1


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


def test_isolated_env_carries_the_token_but_not_the_operator(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("USER", "operator")
    monkeypatch.setenv("GIT_AUTHOR_EMAIL", "operator@example.com")
    monkeypatch.setenv("CLAUDE_CODE_OAUTH_TOKEN", "token")
    env = run.isolated_env(tmp_path)
    assert env.keys() == {"PATH", "HOME", "CLAUDE_CODE_OAUTH_TOKEN"}
    assert env["HOME"] == str(tmp_path)


def test_serve_retries_a_server_that_exits_while_starting(tmp_path: Path) -> None:
    (tmp_path / "www").mkdir()
    (tmp_path / "www" / "readyz").write_text("ok")
    # Exits on its first start, as on a port taken since it was probed; serves /readyz on its second.
    fake = tmp_path / "keepsake"
    fake.write_text(
        "#!/bin/sh\n"
        f'echo x >> "{tmp_path}/starts"\n'
        f'[ "$(wc -l < "{tmp_path}/starts")" -gt 1 ] || exit 1\n'
        'while [ "$1" != --port ]; do shift; done\n'
        f'exec "{sys.executable}" -m http.server "$2" --bind 127.0.0.1 --directory "{tmp_path}/www"\n'
    )
    fake.chmod(0o755)
    server, port = run.serve(fake, "dsn", "tenant", subprocess.DEVNULL)
    try:
        assert port > 0
        assert (tmp_path / "starts").read_text().count("x") == 2
    finally:
        server.terminate()
        server.wait()


def test_serve_gives_up_after_three_starts(tmp_path: Path) -> None:
    fake = tmp_path / "keepsake"
    fake.write_text(f'#!/bin/sh\necho x >> "{tmp_path}/starts"\nexit 1\n')
    fake.chmod(0o755)
    with pytest.raises(RuntimeError):
        run.serve(fake, "dsn", "tenant", subprocess.DEVNULL)
    assert (tmp_path / "starts").read_text().count("x") == 3


def _read(path: str, links: list, backlinks: list) -> dict:
    return {"tool": "read", "input": {"path": path}, "error": False,
            "output": json.dumps({"path": path, "links": links, "backlinks": backlinks})}


def test_link_metrics_count_link_only_reads_and_missed_links() -> None:
    calls = [
        {"tool": "search", "input": {"query": "q"}, "error": False,
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


def test_a_failed_read_is_neither_a_read_nor_a_follow() -> None:
    failed = {**_read("b", [], []), "error": True, "output": "no such concept"}
    m = grade.link_metrics([_read("a", ["b"], []), failed], required=["b"])
    assert m == {"reads": 1, "offered": 1, "link_only_reads": 0, "missed_links": 1}


def test_summary_tolerates_rows_without_link_metrics() -> None:
    row = {"variant": "v", "task": "t", "trial": 1, "passed": True, "checks": {},
           **grade.metrics([], {})}
    for key in ("reads", "offered", "followed", "link_only_reads", "missed_links"):
        row.pop(key, None)
    assert "| v | 1/1 | 1 |" in grade.summarize([row])


BEAM_ROW = {
    "conversation_id": "7",
    "chat": [
        [
            {"role": "user", "content": "Use tabs.", "time_anchor": "March-01-2024"},
            {"role": "assistant", "content": "Noted.", "time_anchor": "March-01-2024"},
            {"role": "user", "content": "x" * 300_000, "time_anchor": "March-01-2024"},
        ]
    ],
    "probing_questions": repr(
        {
            "instruction_following": [
                {"question": "Format this.", "rubric": ["Uses tabs"]}
            ]
        }
    ),
}


def test_a_beam_conversation_becomes_storable_concepts(tmp_path: Path) -> None:
    import beam

    beam.bundle(BEAM_ROW, tmp_path)
    files = sorted(tmp_path.rglob("*.md"))
    assert files[0].relative_to(tmp_path).as_posix() == "session/001/001.md"
    assert "user: Use tabs." in files[0].read_text()
    # The 300 KB turn is split, since keepsake caps a body at 256 KiB.
    assert len(files) >= 3
    assert all(len(f.read_bytes()) <= 256 * 1024 for f in files)


def test_a_beam_task_carries_the_official_rubric_judge() -> None:
    import beam

    [task] = beam.tasks(BEAM_ROW, "1M", beam.OUT / "bundles" / "1M-7")
    assert task["id"] == "1M-7-instruction_following-1"
    assert task["kind"] == "instruction_following"
    assert task["prompt"] == "Format this."
    assert task["read_only"] is True
    [prompt] = task["expect"]["judge_rubric"]
    assert "RUBRIC CRITERION (what to check): Uses tabs" in prompt
    assert grade.RESPONSE in prompt


def test_a_rubric_judge_reply_scores_from_its_json() -> None:
    assert run.rubric_score('```json\n{"score": 0.5, "reason": "partly"}\n```') == 0.5
    assert run.rubric_score('{"score": 1.0}') == 1.0
    assert run.rubric_score("no json") == 0.0
