"""Grade one `claude -p` stream-json transcript against a task, and summarize runs.

Pure functions over parsed data, so the tests need neither Claude nor Postgres. `fetch` is the
one exception: the dataset converters share it.
"""

from __future__ import annotations

import hashlib
import json
import re
import string
import urllib.request
from collections import Counter
from collections.abc import Iterable
from pathlib import Path
from statistics import mean
from typing import Any

TOOL_PREFIX = "mcp__keepsake__"
# Where a task's judge_template takes the agent's answer.
RESPONSE = "<<RESPONSE>>"
WRITES = {"okf_create", "okf_update", "okf_relate"}
# Export generates these at the bundle root; they are not concepts.
GENERATED = {"index", "log"}


def normalize(text: str) -> str:
    """SQuAD's answer normalization: lowercase, no punctuation, no articles, single spaces."""
    text = "".join(ch for ch in text.lower() if ch not in string.punctuation)
    return " ".join(w for w in text.split() if w not in {"a", "an", "the"})


def fetch(url: str, dest: Path, sha256: str) -> Path:
    """Downloads url to dest once, refusing a file whose SHA-256 is not the pinned one."""
    if dest.exists() and hashlib.sha256(dest.read_bytes()).hexdigest() == sha256:
        return dest
    dest.parent.mkdir(parents=True, exist_ok=True)
    part = dest.with_name(dest.name + ".part")
    with urllib.request.urlopen(url) as r, part.open("wb") as f:
        while chunk := r.read(1 << 20):
            f.write(chunk)
    got = hashlib.sha256(part.read_bytes()).hexdigest()
    if got != sha256:
        part.unlink()
        raise ValueError(f"{url}: sha256 {got}, want {sha256}")
    return part.replace(dest)


def concept_path(path: str) -> str:
    return path.strip().lstrip("/").removesuffix(".md")


def load_bundle(directory: Path) -> dict[str, str]:
    concepts = {
        concept_path(str(p.relative_to(directory))): p.read_text()
        for p in directory.rglob("*.md")
    }
    return {path: text for path, text in concepts.items() if path not in GENERATED}


def parse(lines: Iterable[str]) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    """Returns the keepsake tool calls in order, and the final `result` message."""
    calls: list[dict[str, Any]] = []
    by_id: dict[str, dict[str, Any]] = {}
    result: dict[str, Any] = {}
    for line in lines:
        try:
            m = json.loads(line)
        except ValueError:
            continue
        content = (m.get("message") or {}).get("content")
        if m.get("type") == "assistant" and isinstance(content, list):
            for block in content:
                if block.get("type") == "tool_use" and block["name"].startswith(
                    TOOL_PREFIX
                ):
                    call = {
                        "tool": block["name"].removeprefix(TOOL_PREFIX),
                        "input": block.get("input") or {},
                        "error": False,
                        "output": "",
                    }
                    calls.append(call)
                    by_id[block["id"]] = call
        elif m.get("type") == "user" and isinstance(content, list):
            for block in content:
                if (
                    block.get("type") == "tool_result"
                    and block.get("tool_use_id") in by_id
                ):
                    call = by_id[block["tool_use_id"]]
                    call["error"] = bool(block.get("is_error"))
                    body = block.get("content")
                    call["output"] = (
                        body
                        if isinstance(body, str)
                        else "".join(
                            b.get("text", "") for b in body or [] if isinstance(b, dict)
                        )
                    )
        elif m.get("type") == "result":
            result = m
    return calls, result


def grade(
    expect: dict[str, Any],
    calls: list[dict[str, Any]],
    answer: str,
    before: dict[str, str],
    after: dict[str, str],
    judged: bool | None = None,
    require_tools: bool = True,
) -> dict[str, bool]:
    # Every task needs the knowledge base, so an answer from general knowledge never passes.
    checks: dict[str, bool] = {"used keepsake": bool(calls)} if require_tools else {}
    read = {
        concept_path(c["input"].get("path", ""))
        for c in calls
        if c["tool"] == "okf_read"
    }
    for path in expect.get("reads", []) if require_tools else []:
        checks[f"read {path}"] = path in read
    for rx in expect.get("answer", []):
        checks[f"answer ~ /{rx}/"] = re.search(rx, answer, re.IGNORECASE) is not None
    if "answer_any" in expect:
        aliases = [a for a in map(normalize, expect["answer_any"]) if a]
        checks["answer ~ any alias"] = any(a in normalize(answer) for a in aliases)
    if "judge" in expect or "judge_template" in expect:
        checks["judge"] = bool(judged)
    if expect.get("search_before_write"):
        first = next((i for i, c in enumerate(calls) if c["tool"] in WRITES), None)
        checks["looked before writing"] = first is None or any(
            c["tool"] not in WRITES for c in calls[:first]
        )

    spec = expect.get("after")
    if spec:
        added = sorted(set(after) - set(before))
        checks["nothing deleted"] = set(before) <= set(after)
        if "added" in spec:
            checks[f"added {spec['added']}"] = len(added) == spec["added"]
        for new in spec.get("new", []):
            checks[f"new {new['prefix']}*"] = any(
                p.startswith(new["prefix"])
                and all(
                    re.search(rx, after[p], re.IGNORECASE) for rx in new.get("body", [])
                )
                for p in added
            )
        for path, want in spec.get("changed", {}).items():
            text = after.get(path, "")
            checks[f"{path} updated"] = all(
                re.search(rx, text, re.IGNORECASE) for rx in want.get("body", [])
            ) and not any(
                re.search(rx, text, re.IGNORECASE) for rx in want.get("body_not", [])
            )
    return checks


def _paths(items: Any) -> list[str]:
    """Paths from a list of path strings or of cards, as okf_read and search return them."""
    return [
        concept_path(i if isinstance(i, str) else i.get("path", ""))
        for i in items or []
        if isinstance(i, (str, dict))
    ]


def link_metrics(
    calls: list[dict[str, Any]], required: Iterable[str] = ()
) -> dict[str, int]:
    """How agents use okf_read's links. A link-only read opens a path no search, grep or list
    had shown; a missed link is a required path the agent was linked to and never read."""
    searched: set[str] = set()
    read: set[str] = set()
    offered: set[str] = set()
    link_only = reads = 0
    for c in calls:
        try:
            out = json.loads(c.get("output") or "null")
        except ValueError:
            out = None
        if c["tool"] == "okf_read":
            path = concept_path(c["input"].get("path", ""))
            reads += 1
            if path in offered and path not in read:
                link_only += path not in searched
            read.add(path)
            if isinstance(out, dict):
                offered |= {
                    p
                    for p in _paths(out.get("links")) + _paths(out.get("backlinks"))
                    if p not in read
                }
        elif isinstance(out, dict):
            searched |= set(_paths(out.get("results")) + _paths(out.get("paths")))
    return {
        "reads": reads,
        "offered": len(offered),
        "link_only_reads": link_only,
        "missed_links": len({concept_path(p) for p in required} & (offered - read)),
    }


def metrics(
    calls: list[dict[str, Any]], result: dict[str, Any], required: Iterable[str] = ()
) -> dict[str, Any]:
    usage = result.get("usage") or {}
    return {
        "calls": len(calls),
        "by_tool": dict(Counter(c["tool"] for c in calls)),
        "tool_errors": sum(c["error"] for c in calls),
        "first_tool": calls[0]["tool"] if calls else None,
        "search_queries": [
            c["input"].get("query", "") for c in calls if c["tool"] == "okf_search"
        ],
        "turns": result.get("num_turns") or 0,
        "cost_usd": result.get("total_cost_usd") or 0.0,
        "duration_s": (result.get("duration_ms") or 0) / 1000,
        "input_tokens": sum(
            usage.get(k) or 0
            for k in (
                "input_tokens",
                "cache_creation_input_tokens",
                "cache_read_input_tokens",
            )
        ),
        "output_tokens": usage.get("output_tokens") or 0,
        **link_metrics(calls, required),
    }


def _avg(values: list[float]) -> str:
    return f"{mean(values):.2f}" if values else "-"


def summarize(rows: list[dict[str, Any]]) -> str:
    """A markdown report: one line per variant, a task-by-variant pass matrix, then every failed check."""
    variants = sorted({r["variant"] for r in rows})
    tasks = list(dict.fromkeys(r["task"] for r in rows))
    out = [
        "| variant | pass | used tools | calls | searches | words/query | cost $ | turns | seconds "
        "| input tokens | reads | link-only reads | missed links |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for v in variants:
        rs = [r for r in rows if r["variant"] == v]
        queries = [q for r in rs for q in r["search_queries"]]
        out.append(
            f"| {v} | {sum(r['passed'] for r in rs)}/{len(rs)} "
            f"| {sum(r['calls'] > 0 for r in rs)}/{len(rs)} "
            f"| {_avg([r['calls'] for r in rs])} "
            f"| {_avg([len(r['search_queries']) for r in rs])} "
            f"| {_avg([len(q.split()) for q in queries])} "
            f"| {_avg([r['cost_usd'] for r in rs])} "
            f"| {_avg([r['turns'] for r in rs])} "
            f"| {_avg([r['duration_s'] for r in rs])} "
            f"| {_avg([r['input_tokens'] for r in rs])} "
            f"| {_avg([r['reads'] for r in rs if 'reads' in r])} "
            f"| {sum(r.get('link_only_reads', 0) for r in rs)}/{sum(r.get('reads', 0) for r in rs)} "
            f"| {sum(r.get('missed_links', 0) for r in rs)} |"
        )
    # Past a screenful of tasks, a row per task hides the pattern; a row per kind shows it.
    key = "task" if len(tasks) <= 20 else "kind"
    out += [
        "",
        f"| {key} | " + " | ".join(variants) + " |",
        "|---|" + "---|" * len(variants),
    ]
    for t in dict.fromkeys(r[key] for r in rows):
        cells = []
        for v in variants:
            rs = [r for r in rows if r["variant"] == v and r[key] == t]
            cells.append(f"{sum(r['passed'] for r in rs)}/{len(rs)}" if rs else "-")
        out.append(f"| {t} | " + " | ".join(cells) + " |")
    failures = [
        f"- {r['variant']} / {r['task']} #{r['trial']}: "
        + (
            "; ".join(
                filter(
                    None,
                    [
                        r.get("error"),
                        ", ".join(k for k, ok in r["checks"].items() if not ok),
                    ],
                )
            )
            or "failed"
        )
        for r in rows
        if not r["passed"]
    ]
    if failures:
        out += ["", "Failures:", *failures]
    return "\n".join(out)
