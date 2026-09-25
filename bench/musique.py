"""MuSiQue as agent tasks for bench/run.py, in three bundles that differ only in their links.

MuSiQue (Trivedi et al., TACL 2022) is CC BY 4.0. Each question needs 2-4 hops and ships 20
paragraphs, whose distractors were retrieved by BM25 with the bridge entity masked. Every
question gets three bundles of the same 20 concepts: `none` has no links, `mention` links each
paragraph's mentions of other paragraphs' titles, and `chain` links only each hop to the next.

    python3 bench/musique.py --per-hop 10
    python3 bench/run.py --tasks-file bench/data/musique/agent/musique-mention.json --variants main cards
"""

from __future__ import annotations

import argparse
import json
import random
import re
import shutil
from collections import defaultdict
from pathlib import Path

import grade

HERE = Path(__file__).resolve().parent
DATA = HERE / "data" / "musique"
OUT = DATA / "agent"
URL = "https://huggingface.co/datasets/bdsaglam/musique/resolve/main/musique_ans_v1.0_dev.jsonl"
SHA256 = "15fa63794d18a94ce12411aca6e2327e65b6e83b0b1490efab3f1962e48abf3b"
KINDS = ("none", "mention", "chain")
# Shorter titles ("War", "Red") would link almost every paragraph.
MIN_TITLE = 4

HOST = (
    "Your memory holds encyclopedia paragraphs, and the answer to the user's question is in it. "
    "Reply with the answer as a short phrase first, then explain."
)


def slug(title: str) -> str:
    return re.sub(r"[^a-z0-9]+", "-", title.lower()).strip("-")[:40]


def concept_path(p: dict) -> str:
    return f"p/{p['idx']}-{slug(p['title'])}"


def _word(title: str) -> re.Pattern[str]:
    return re.compile(r"(?<!\w)" + re.escape(title) + r"(?!\w)")


def link(text: str, targets: list[tuple[str, str]], blockers: list[str] = ()) -> str:
    """Links each target title's first whole-word mention. Longer titles win overlaps, and every
    mention of a blocker (the paragraph's own title) is off limits to shorter titles inside it."""
    spans: list[tuple[int, int, str | None]] = []

    def free(s: int, e: int) -> bool:
        return all(e <= a or s >= b for a, b, _ in spans)

    # Every mention of each title is reserved, longest first; only a target's first free one links.
    for title, path in sorted([*targets, *((b, None) for b in blockers)], key=lambda t: -len(t[0])):
        linked = path is None
        for m in _word(title).finditer(text):
            if free(m.start(), m.end()):
                spans.append((m.start(), m.end(), None if linked else path))
                linked = True
    out, at = [], 0
    for s, e, path in sorted(spans):
        if path is not None:
            out += [text[at:s], f"[{text[s:e]}](/{path}.md)"]
            at = e
    return "".join(out) + text[at:]


def bodies(record: dict, kind: str) -> dict[str, str]:
    paras = record["paragraphs"]
    out = {concept_path(p): p["paragraph_text"] for p in paras}
    if kind == "mention":
        first: dict[str, dict] = {}
        for p in sorted(paras, key=lambda p: p["idx"]):
            first.setdefault(p["title"], p)
        for p in paras:
            targets = [(t, concept_path(q)) for t, q in first.items()
                       if len(t) >= MIN_TITLE and t != p["title"]]
            out[concept_path(p)] = link(p["paragraph_text"], targets, [p["title"]])
    elif kind == "chain":
        by_idx = {p["idx"]: p for p in paras}
        steps = [d["paragraph_support_idx"] for d in record["question_decomposition"]]
        for a, b in zip(steps, steps[1:]):
            if a == b:
                continue
            src, dst = by_idx[a], by_idx[b]
            before = out[concept_path(src)]
            linked = link(before, [(dst["title"], concept_path(dst))], [src["title"]])
            if linked == before:
                linked += f"\n\nRelated: [{dst['title']}](/{concept_path(dst)}.md)"
            out[concept_path(src)] = linked
    return out


def task(record: dict, kind: str) -> dict:
    by_idx = {p["idx"]: p for p in record["paragraphs"]}
    decomposition = record["question_decomposition"]
    reads = [concept_path(by_idx[d["paragraph_support_idx"]]) for d in decomposition]
    return {
        "id": record["id"],
        "kind": f"{len(decomposition)}hop",
        "bundle": f"{kind}/{record['id']}",
        "prompt": record["question"],
        "system_prompt": HOST,
        "expect": {
            "reads": list(dict.fromkeys(reads)),
            "answer_any": [record["answer"], *record.get("answer_aliases", [])],
        },
    }


def sample(records: list[dict], per_hop: int, seed: int) -> list[dict]:
    by_hops: dict[int, list[dict]] = defaultdict(list)
    for r in sorted(records, key=lambda r: r["id"]):
        by_hops[len(r["question_decomposition"])].append(r)
    rng = random.Random(seed)
    return [r for hops in (2, 3, 4) for r in rng.sample(by_hops[hops], per_hop)]


def write(record: dict, kind: str) -> None:
    titles = {concept_path(p): p["title"] for p in record["paragraphs"]}
    root = OUT / kind / record["id"]
    for path, body in bodies(record, kind).items():
        f = root / f"{path}.md"
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text(f"---\ntype: Paragraph\ntitle: {json.dumps(titles[path])}\n---\n{body}\n")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--per-hop", type=int, default=10, help="Questions per hop count. Default: 10.")
    p.add_argument("--seed", type=int, default=0, help="Sampling seed. Default: 0.")
    args = p.parse_args()
    source = grade.fetch(URL, DATA / "musique_ans_v1.0_dev.jsonl", SHA256)
    records = [json.loads(line) for line in source.read_text().splitlines() if line.strip()]
    chosen = sample(records, args.per_hop, args.seed)
    shutil.rmtree(OUT, ignore_errors=True)
    for kind in KINDS:
        for r in chosen:
            write(r, kind)
        tasks = [task(r, kind) for r in chosen]
        (OUT / f"musique-{kind}.json").write_text(json.dumps(tasks, indent=2))
    print(f"wrote {len(chosen)} questions x {len(KINDS)} bundles to {OUT}")


if __name__ == "__main__":
    main()
