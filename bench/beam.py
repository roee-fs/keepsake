"""BEAM as agent tasks for bench/run.py, at 100K to 10M tokens of conversation.

BEAM (Tavakoli et al., ICLR 2026) is CC BY-SA 4.0. Each conversation is one tenant, one concept
per exchange, and carries 20 probing questions over ten memory abilities. Grading is BEAM's own
rubric judge: one call per rubric item, scored 0, 0.5 or 1, averaged per question. A trial passes
at 0.5; compare runs on the mean score, which is what BEAM reports. BEAM scores event ordering
by Kendall tau, which needs an alignment model; here it gets the rubric judge like the rest.

The questions only read, so run.py loads each conversation once and shares it across them.

    python3 bench/beam.py --split 1M --conversations 5
    python3 bench/run.py --tasks-file bench/data/beam/agent/1M.json --variants baseline --trials 1
    python3 bench/beam.py --report bench/results/<run>
"""

from __future__ import annotations

import argparse
import ast
import json
import shutil
import urllib.request
from collections import defaultdict
from pathlib import Path
from statistics import mean

import grade

HERE = Path(__file__).resolve().parent
DATA = HERE / "data" / "beam"
OUT = DATA / "agent"
# The dataset revisions these converters were written against.
DATASETS = {
    "Mohammadta/BEAM": (
        "3205395e897e7318c7b094ef4e6047b9b82dbb03",
        ["100K", "500K", "1M"],
    ),
    "Mohammadta/BEAM-10M": ("9b2096193fe74e2837e4713e483351e19817773c", ["10M"]),
}
ROWS = "https://datasets-server.huggingface.co/rows?dataset={}&config=default&split={}&offset={}&length=1"
# Below keepsake's 256 KiB body cap, with room for the frontmatter.
LIMIT = 200_000

# Verbatim from BEAM's src/prompts.py, unified_llm_judge_base_prompt (CC BY-SA 4.0).
JUDGE = """
You are an expert evaluator tasked with judging whether the LLM's response demonstrates compliance with the specified RUBRIC CRITERION.

## EVALUATION INPUTS
- RUBRIC CRITERION (what to check): <rubric_item>
- RESPONSE TO EVALUATE: <llm_response>

## EVALUATION RUBRIC:
The rubric defines a specific requirement, constraint, or expected behavior that the LLM response should demonstrate.

**IMPORTANT**: Pay careful attention to whether the rubric specifies:
- **Positive requirements** (things the response SHOULD include/do)
- **Negative constraints** (things the response SHOULD NOT include/do, often indicated by "no", "not", "avoid", "absent")

## RESPONSIVENESS REQUIREMENT
A compliant response must be **on-topic** and attempt to answer it.
- If the response does not address the QUESTION, score **0.0** and stop.
- For negative constraints, both must hold: (a) the response is responsive to the QUESTION, and (b) the prohibited element is absent.

## SEMANTIC TOLERANCE RULES:
Judge by meaning, not exact wording.
- Accept **paraphrases** and **synonyms** that preserve intent.
- **Case/punctuation/whitespace** differences must be ignored.
- **Numbers/currencies/dates** may appear in equivalent forms (e.g., “$68,000”, “68k”, “68,000 USD”, or “sixty-eight thousand dollars”). Treat them as equal when numerically equivalent.
- If the rubric expects a number or duration, prefer **normalized comparison** (extract and compare values) over string matching.

## STYLE NEUTRALITY (prevents style contamination):
Ignore tone, politeness, length, and flourish unless the rubric explicitly requires a format/structure (e.g., “itemized list”, “no citations”, “one sentence”).
- Do **not** penalize hedging, voice, or verbosity if content satisfies the rubric.
- Only evaluate format when the rubric **explicitly** mandates it.

## SCORING SCALE:
- **1.0 (Complete Compliance)**: Fully complies with the rubric criterion.
  - Positive: required element present, accurate, properly executed (allowing semantic equivalents).
  - Negative: prohibited element **absent** AND response is **responsive**.

- **0.5 (Partial Compliance)**: Partially complies.
  - Positive: element present but minor inaccuracies/incomplete execution.
  - Negative: generally responsive and mostly avoids the prohibited element but with minor/edge violations.

- **0.0 (No Compliance)**: Fails to comply.
  - Positive: required element missing or incorrect.
  - Negative: prohibited element present **or** response is non-responsive/evasive even if the element is absent.

## EVALUATION INSTRUCTIONS:
1. **Understand the Requirement**: Determine if the rubric is asking for something to be present (positive) or absent (negative/constraint).

2. **Parse Compound Statements**: If the rubric contains multiple elements connected by "and" or commas, evaluate whether:
   - **All elements** must be present for full compliance (1.0)
   - **Some elements** present indicates partial compliance (0.5)
   - **No elements** present indicates no compliance (0.0)

3. **Check Compliance**:
   - For positive requirements: Look for the presence and quality of the required element
   - For negative constraints: Look for the absence of the prohibited element

4. **Assign Score**: Based on compliance with the specific rubric criterion according to the scoring scale above.

5. **Provide Reasoning**: Explain whether the rubric criterion was satisfied and justify the score.

## OUTPUT FORMAT:
Return your evaluation in JSON format with two fields:

{
   "score": [your score: 1.0, 0.5, or 0.0],
   "reason": "[detailed explanation of whether the rubric criterion was satisfied and why this justified the assigned score]"
}

NOTE: ONLY output the json object, without any explanation before or after that
"""

HOST = (
    "You are an assistant continuing a long relationship with this user. Your memory holds your past "
    "conversations with them. Answer their message as you would in that relationship, honouring what they "
    "told you before."
)


def fetch(split: str, offset: int) -> dict | None:
    """One conversation, cached with only the fields used here, or None past the last."""
    dest = DATA / split / f"{offset}.json"
    if dest.exists():
        return json.loads(dest.read_text())
    dataset, (revision, _) = next((d, v) for d, v in DATASETS.items() if split in v[1])
    with urllib.request.urlopen(f"https://huggingface.co/api/datasets/{dataset}") as r:
        if (sha := json.load(r)["sha"]) != revision:
            raise SystemExit(
                f"{dataset} is at {sha}, not {revision}; check the format, then update DATASETS"
            )
    with urllib.request.urlopen(ROWS.format(dataset, split, offset)) as r:
        rows = json.load(r)["rows"]
    if not rows:
        return None
    full = rows[0]["row"]
    row = {k: full[k] for k in ("conversation_id", "chat", "probing_questions")}
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_text(json.dumps(row))
    return row


def sessions(row: dict) -> list[list[list[dict]]]:
    """Sessions of exchanges of turns. 10M nests batches under plans and groups exchanges itself;
    the shorter splits list each session's turns, so an exchange starts at each user turn."""
    out = []
    for item in row["chat"]:
        if isinstance(item, dict):
            for batches in filter(None, item.values()):
                out += [batch["turns"] for batch in batches]
            continue
        exchanges: list[list[dict]] = []
        for turn in item:
            if turn["role"] == "user" or not exchanges:
                exchanges.append([])
            exchanges[-1].append(turn)
        out.append(exchanges)
    return out


def bundle(row: dict, root: Path) -> None:
    """Writes one concept per exchange, in parts where one would pass keepsake's body cap."""
    for s, exchanges in enumerate(sessions(row), 1):
        (root / "session" / f"{s:03d}").mkdir(parents=True, exist_ok=True)
        n = 0
        for exchange in exchanges:
            date = next(
                (t["time_anchor"] for t in exchange if t.get("time_anchor")), ""
            )
            body = "\n\n".join(f"{t['role']}: {t['content']}" for t in exchange)
            # Four bytes is the most a character takes in UTF-8.
            step = LIMIT // 4
            for part in range(0, max(len(body), 1), step):
                n += 1
                title = json.dumps(f"Session {s}, exchange {n}, {date}".strip(", "))
                (root / "session" / f"{s:03d}" / f"{n:03d}.md").write_text(
                    f"---\ntype: Exchange\ntitle: {title}\n---\n{body[part : part + step]}\n"
                )


def tasks(row: dict, split: str, root: Path) -> list[dict]:
    """The conversation's probing questions, each graded by BEAM's rubric judge."""
    probes = ast.literal_eval(row["probing_questions"])
    out = []
    for kind, questions in sorted(probes.items()):
        for i, q in enumerate(questions, 1):
            out.append(
                {
                    "id": f"{split}-{row['conversation_id']}-{kind}-{i}",
                    "kind": kind,
                    "bundle": str(root.relative_to(root.parents[1])),
                    "prompt": q["question"],
                    "system_prompt": HOST,
                    "read_only": True,
                    "expect": {
                        "judge_rubric": [
                            JUDGE.replace("<rubric_item>", item).replace(
                                "<llm_response>", grade.RESPONSE
                            )
                            for item in q["rubric"]
                        ]
                    },
                }
            )
    return out


def report(run: Path) -> str:
    """Mean rubric score per variant and ability, as BEAM reports it."""
    rows = [
        json.loads(line) for line in (run / "results.jsonl").read_text().splitlines()
    ]
    scores: dict[str, dict[str, list[float]]] = defaultdict(lambda: defaultdict(list))
    for r in rows:
        scores[r["variant"]][r["kind"]].append(r.get("score", 0.0))
    variants = sorted(scores)
    kinds = sorted({k for v in scores.values() for k in v})
    out = [
        "| ability | " + " | ".join(variants) + " |",
        "|---|" + "---|" * len(variants),
    ]
    for k in kinds:
        out.append(
            f"| {k} | "
            + " | ".join(
                f"{mean(scores[v][k]):.3f}" if scores[v][k] else "-" for v in variants
            )
            + " |"
        )
    out.append(
        "| **overall** | "
        + " | ".join(
            f"**{mean(s for ss in scores[v].values() for s in ss):.3f}**"
            for v in variants
        )
        + " |"
    )
    return "\n".join(out)


def main() -> None:
    p = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    p.add_argument(
        "--split", default="1M", choices=[s for _, ss in DATASETS.values() for s in ss]
    )
    p.add_argument(
        "--conversations",
        type=int,
        help="Use only the first N conversations. Default: all.",
    )
    p.add_argument(
        "--report",
        type=Path,
        metavar="RUN",
        help="Print a run's mean scores, and exit.",
    )
    args = p.parse_args()
    if args.report:
        print(report(args.report))
        return
    out: list[dict] = []
    offset = 0
    while args.conversations is None or offset < args.conversations:
        row = fetch(args.split, offset)
        if row is None:
            break
        root = OUT / "bundles" / f"{args.split}-{row['conversation_id']}"
        shutil.rmtree(root, ignore_errors=True)
        bundle(row, root)
        out += tasks(row, args.split, root)
        offset += 1
    (OUT / f"{args.split}.json").write_text(json.dumps(out, indent=2) + "\n")
    print(
        f"{args.split}: {len(out)} tasks over {offset} conversations -> {OUT / (args.split + '.json')}"
    )


if __name__ == "__main__":
    main()
