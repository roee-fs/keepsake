"""LoCoMo as agent tasks for bench/run.py, graded by the judge behind Mem0's published J score.

LoCoMo (Maharana et al., ACL 2024) is CC BY-NC 4.0. It holds 10 conversations between two
people, each up to 35 sessions. Each conversation is one tenant, one concept per session titled
with its date. Like Mem0, this leaves out category 5 (adversarial) and scores the other 1540
questions with Mem0's CORRECT/WRONG judge. Mem0 judged with gpt-4o-mini and run.py judges with
Claude, so compare variants with each other, not with Mem0's numbers.

The questions only read, so run.py loads each conversation once and shares it across them.

    python3 bench/datasets/locomo.py --conversations 2
    python3 bench/run.py --tasks-file bench/data/locomo/agent/locomo.json --variants baseline --trials 1
"""

from __future__ import annotations

import argparse
import json
import re
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import grade

HERE = Path(__file__).resolve().parents[1]
DATA = HERE / "data" / "locomo"
OUT = DATA / "agent"
URL = "https://raw.githubusercontent.com/snap-research/locomo/3eb6f2c585f5e1699204e3c3bdf7adc5c28cb376/data/locomo10.json"
SHA256 = "79fa87e90f04081343b8c8debecb80a9a6842b76a7aa537dc9fdf651ea698ff4"
# Mem0's names for LoCoMo's numbered categories (mem0ai/memory-benchmarks, benchmarks/locomo/prompts.py).
KINDS = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop"}

# Verbatim from Mem0's evaluation/metrics/llm_judge.py at b3ede5b (Apache-2.0), slots marked.
JUDGE = """
Your task is to label an answer to a question as ’CORRECT’ or ’WRONG’. You will be given the following data:
    (1) a question (posed by one user to another user),
    (2) a ’gold’ (ground truth) answer,
    (3) a generated answer
which you will score as CORRECT/WRONG.

The point of the question is to ask about something one user should know about the other user based on their prior conversations.
The gold answer will usually be a concise and short answer that includes the referenced topic, for example:
Question: Do you remember what I got the last time I went to Hawaii?
Gold answer: A shell necklace
The generated answer might be much longer, but you should be generous with your grading - as long as it touches on the same topic as the gold answer, it should be counted as CORRECT.

For time related questions, the gold answer will be a specific date, month, year, etc. The generated answer might be much longer or use relative time references (like "last Tuesday" or "next month"), but you should be generous with your grading - as long as it refers to the same date or time period as the gold answer, it should be counted as CORRECT. Even if the format differs (e.g., "May 7th" vs "7 May"), consider it CORRECT if it's the same date.

Now it's time for the real question:
Question: {question}
Gold answer: {gold_answer}
Generated answer: {generated_answer}

First, provide a short (one sentence) explanation of your reasoning, then finish with CORRECT or WRONG.
Do NOT include both CORRECT and WRONG in your response, or it will break the evaluation script.

Just return the label CORRECT or WRONG in a json format with the key as "label".
"""
# Mem0 passes a reply whose JSON label is CORRECT.
PASS = r'"label"\s*:\s*"CORRECT"'

HOST = (
    "Your memory holds the conversations between {a} and {b}, one session per concept, each titled "
    "with when it took place. Answer the question from it. Date events by their sessions, not by today's date."
)


def sessions(conversation: dict) -> list[tuple[int, str, list[dict]]]:
    """Each session's number, date and turns, in order."""
    return sorted(
        (int(m[1]), conversation[f"session_{m[1]}_date_time"], turns)
        for key, turns in conversation.items()
        if (m := re.fullmatch(r"session_(\d+)", key)) and turns
    )


def bundle(sample: dict, root: Path) -> None:
    """Writes one concept per session. A shared photo becomes its caption, as LoCoMo's own baselines read it."""
    (root / "session").mkdir(parents=True, exist_ok=True)
    for n, date, turns in sessions(sample["conversation"]):
        body = "\n\n".join(
            f"{t['speaker']}: {t['text']}"
            + (f" [shares {t['blip_caption']}]" if t.get("blip_caption") else "")
            for t in turns
        )
        title = json.dumps(f"Session {n}, {date}")
        (root / "session" / f"{n:02d}.md").write_text(
            f"---\ntype: Session\ntitle: {title}\n---\n{body}\n", encoding="utf-8"
        )


def tasks(sample: dict, root: Path) -> list[dict]:
    """The conversation's questions outside category 5, each graded by Mem0's judge."""
    c = sample["conversation"]
    host = HOST.format(a=c["speaker_a"], b=c["speaker_b"])
    out = []
    for i, qa in enumerate(sample["qa"], 1):
        if qa["category"] not in KINDS:
            continue
        evidence = sorted(
            {f"session/{int(m[1]):02d}" for e in qa["evidence"] if (m := re.match(r"D(\d+):", e))}
        )
        out.append(
            {
                "id": f"{sample['sample_id']}-{i}",
                "kind": KINDS[qa["category"]],
                "bundle": str(root.relative_to(root.parents[1])),
                "prompt": qa["question"],
                "system_prompt": host,
                "read_only": True,
                "expect": {
                    "judge_template": JUDGE.replace("{question}", qa["question"])
                    .replace("{gold_answer}", str(qa["answer"]))
                    .replace("{generated_answer}", grade.RESPONSE),
                    "judge_pass": PASS,
                    "evidence": evidence,
                },
            }
        )
    return out


def main() -> None:
    p = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    p.add_argument(
        "--conversations", type=int, help="Use only the first N conversations. Default: all 10."
    )
    args = p.parse_args()
    samples = json.loads(
        grade.fetch(URL, DATA / "locomo10.json", SHA256).read_text(encoding="utf-8")
    )[: args.conversations]
    out: list[dict] = []
    for sample in samples:
        root = OUT / "bundles" / sample["sample_id"]
        shutil.rmtree(root, ignore_errors=True)
        bundle(sample, root)
        out += tasks(sample, root)
    (OUT / "locomo.json").write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
    print(f"locomo: {len(out)} tasks over {len(samples)} conversations -> {OUT / 'locomo.json'}")


if __name__ == "__main__":
    main()
