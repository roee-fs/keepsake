"""LongMemEval_S as agent tasks for bench/run.py, split into a tuning set and a holdout set.

Each question becomes one task with its own bundle: the ~50 chat sessions of its history,
one concept per session. Grading uses LongMemEval's own judge prompts. Tune variants on
tune.json and report them on holdout.json, which a tuning loop MUST NOT read.

    python3 bench/longmemeval.py --per-bucket 8
    python3 bench/run.py --tasks-file bench/data/longmemeval/agent/tune.json --trials 1

To test curated memory, have an agent consolidate each memory, then answer against both:

    python3 bench/longmemeval.py --curate single-session-preference multi-session
    python3 bench/run.py --tasks-file bench/data/longmemeval/agent/curate-tune.json --timeout 1200 --budget 4
    python3 bench/longmemeval.py --curated bench/results/<that run>
    python3 bench/run.py --tasks-file bench/data/longmemeval/agent/tune-raw.json
    python3 bench/run.py --tasks-file bench/data/longmemeval/agent/tune-curated.json
"""

from __future__ import annotations

import argparse
import hashlib
import json
import random
import shutil
import subprocess
from collections import defaultdict
from pathlib import Path

import grade

HERE = Path(__file__).resolve().parent
DATA = HERE / "data" / "longmemeval"
SOURCE = DATA / "longmemeval_s_cleaned.json"
URL = "https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_s_cleaned.json"
OUT = DATA / "agent"
RESPONSE = grade.RESPONSE

# Verbatim from LongMemEval's src/evaluation/evaluate_qa.py (MIT), response slot marked.
_ANSWER = (
    "I will give you a question, a correct answer, and a response from a model. Please answer yes if the "
    "response contains the correct answer. Otherwise, answer no. If the response is equivalent to the correct "
    "answer or contains all the intermediate steps to get the correct answer, you should also answer yes. If "
    "the response only contains a subset of the information required by the answer, answer no. "
)
_TAIL = (
    "\n\nQuestion: {q}\n\nCorrect Answer: {a}\n\nModel Response: "
    + RESPONSE
    + "\n\nIs the model response correct? Answer yes or no only."
)
TEMPLATES = {
    "single-session-user": _ANSWER + _TAIL,
    "single-session-assistant": _ANSWER + _TAIL,
    "multi-session": _ANSWER + _TAIL,
    "temporal-reasoning": _ANSWER
    + "In addition, do not penalize off-by-one errors for the number of days. If the question asks for the "
    "number of days/weeks/months, etc., and the model makes off-by-one errors (e.g., predicting 19 days when "
    "the answer is 18), the model's response is still correct. " + _TAIL,
    "knowledge-update": "I will give you a question, a correct answer, and a response from a model. Please "
    "answer yes if the response contains the correct answer. Otherwise, answer no. If the response contains "
    "some previous information along with an updated answer, the response should be considered as correct as "
    "long as the updated answer is the required answer." + _TAIL,
    "single-session-preference": "I will give you a question, a rubric for desired personalized response, and "
    "a response from a model. Please answer yes if the response satisfies the desired response. Otherwise, "
    "answer no. The model does not need to reflect all the points in the rubric. The response is correct as "
    "long as it recalls and utilizes the user's personal information correctly.\n\nQuestion: {q}\n\nRubric: "
    "{a}\n\nModel Response: "
    + RESPONSE
    + "\n\nIs the model response correct? Answer yes or no only.",
    "abstention": "I will give you an unanswerable question, an explanation, and a response from a model. "
    "Please answer yes if the model correctly identifies the question as unanswerable. The model could say that "
    "the information is incomplete, or some other information is given but the asked information is not."
    "\n\nQuestion: {q}\n\nExplanation: {a}\n\nModel Response: "
    + RESPONSE
    + "\n\nDoes the model correctly "
    "identify the question as unanswerable? Answer yes or no only.",
}


def load() -> list[dict]:
    if not SOURCE.exists():
        DATA.mkdir(parents=True, exist_ok=True)
        subprocess.run(["curl", "-sfL", "-o", str(SOURCE), URL], check=True)
    return json.loads(SOURCE.read_text())


def kind(instance: dict) -> str:
    return (
        "abstention"
        if instance["question_id"].endswith("_abs")
        else instance["question_type"]
    )


def bundle_session(instance: dict, root: Path) -> dict[str, str]:
    """Writes one concept per session, titled with its date, and returns concept path -> session id."""
    (root / "session").mkdir(parents=True)
    ids = {}
    sessions = zip(
        instance["haystack_session_ids"],
        instance["haystack_dates"],
        instance["haystack_sessions"],
        strict=True,
    )
    for sid, date, turns in sessions:
        # Hashed, since LongMemEval's own ids start with answer_ exactly on the evidence sessions.
        path = "session/" + hashlib.sha256(sid.encode()).hexdigest()[:16]
        ids[path] = sid
        body = "\n\n".join(f"{t['role']}: {t['content']}" for t in turns)
        (root / f"{path}.md").write_text(
            f"---\ntype: Session\ntitle: {json.dumps('Session ' + date)}\n---\n{body}\n"
        )
    return ids


def task(instance: dict, bundle: Path) -> dict:
    k = kind(instance)
    template = (
        TEMPLATES[k]
        .replace("{q}", instance["question"])
        .replace("{a}", str(instance["answer"]))
    )
    return {
        "id": instance["question_id"],
        "kind": k,
        "bundle": str(bundle.relative_to(OUT)),
        "prompt": f"Today is {instance['question_date']}. {instance['question']}",
        "system_prompt": HOST,
        "expect": {"judge_template": template},
    }


# Replaces Claude Code's coding-assistant prompt. Claude Code still injects the machine's date and
# the account's email, so the user's own date MUST win, and account details MUST NOT stand in for them.
HOST = (
    "You are a personal assistant with a memory of this user. The user's message states today's date: "
    "use it, and ignore any other date, account or environment details in your context, which are not "
    "about this user."
)


# The same for every tenant, and blind to the question the memory will later answer.
CURATE = (
    "Your memory holds raw transcripts of the user's past conversations: one concept per session "
    "under session/, titled with its date. Consolidate them into memory worth keeping. Read the "
    "sessions, then write a small set of concepts about the user: who they are, their preferences, "
    "plans, possessions, relationships and routines, and anything that changed over time, with its "
    "date. Every concept you write MUST link to each session it draws on, as "
    "[Session <date>](/session/<id>.md). Prefer one concept per topic, updated as you read, over one "
    "per session. Leave the sessions as they are."
)


def curate(split: str, kinds: list[str]) -> None:
    """Writes curate-<split>.json: one consolidation task per question of those kinds."""
    tasks = [
        t for t in json.loads((OUT / f"{split}.json").read_text()) if t["kind"] in kinds
    ]
    curation = [
        {
            "id": t["id"],
            "kind": t["kind"],
            "bundle": t["bundle"],
            "prompt": CURATE,
            "system_prompt": HOST,
            "expect": {},
        }
        for t in tasks
    ]
    path = OUT / f"curate-{split}.json"
    path.write_text(json.dumps(curation, indent=2) + "\n")
    print(f"{len(curation)} curation tasks -> {path}")


def curated(split: str, run: Path) -> None:
    """Pairs each curated memory from a run with its question: <split>-raw.json and <split>-curated.json."""
    exports = {}
    for line in (run / "results.jsonl").read_text().splitlines():
        r = json.loads(line)
        if not r.get("export") or r.get("error"):
            continue
        # A curation that wrote nothing leaves the raw memory, which would dilute the comparison.
        written = [
            p
            for p in grade.load_bundle(Path(r["export"]))
            if not p.startswith("session/")
        ]
        if written:
            exports[r["task"]] = r["export"]
        else:
            print(f"skipped {r['task']}: the curation wrote no concepts")
    tasks = [
        t for t in json.loads((OUT / f"{split}.json").read_text()) if t["id"] in exports
    ]
    (OUT / f"{split}-raw.json").write_text(json.dumps(tasks, indent=2) + "\n")
    (OUT / f"{split}-curated.json").write_text(
        json.dumps([{**t, "bundle": exports[t["id"]]} for t in tasks], indent=2) + "\n"
    )
    print(
        f"{len(tasks)} questions -> {OUT / (split + '-raw.json')} and {OUT / (split + '-curated.json')}"
    )


def main() -> None:
    p = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    p.add_argument("--split", default="tune", choices=["tune", "holdout"])
    p.add_argument(
        "--curate",
        nargs="+",
        metavar="KIND",
        help="Write curation tasks for these kinds, and exit.",
    )
    p.add_argument(
        "--curated",
        type=Path,
        metavar="RUN",
        help="Pair a curation run's memories with questions, and exit.",
    )
    p.add_argument(
        "--per-bucket",
        type=int,
        default=8,
        help="Questions per kind in each split. Default: 8.",
    )
    p.add_argument("--seed", type=int, default=0)
    args = p.parse_args()
    if args.curate:
        curate(args.split, args.curate)
        return
    if args.curated:
        curated(args.split, args.curated)
        return

    buckets: dict[str, list[dict]] = defaultdict(list)
    for instance in load():
        buckets[kind(instance)].append(instance)
    rng = random.Random(args.seed)
    splits: dict[str, list[dict]] = {"tune": [], "holdout": []}
    for k in sorted(buckets):
        chosen = rng.sample(buckets[k], min(2 * args.per_bucket, len(buckets[k])))
        half = len(chosen) // 2
        splits["tune"] += chosen[:half]
        splits["holdout"] += chosen[half:]

    shutil.rmtree(OUT, ignore_errors=True)
    for name, instances in splits.items():
        tasks = []
        for instance in instances:
            bundle = OUT / "bundles" / instance["question_id"]
            bundle_session(instance, bundle)
            tasks.append(task(instance, bundle))
        (OUT / f"{name}.json").write_text(json.dumps(tasks, indent=2) + "\n")
        counts = defaultdict(int)
        for t in tasks:
            counts[t["kind"]] += 1
        print(
            f"{name}: {len(tasks)} tasks {dict(sorted(counts.items()))} -> {OUT / (name + '.json')}"
        )


if __name__ == "__main__":
    main()
