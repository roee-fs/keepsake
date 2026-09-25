"""IIRC as agent tasks for bench/run.py, with the links people wrote and without them.

IIRC (Ferguson et al., EMNLP 2020) is CC BY 4.0. Each question was written from one Wikipedia
passage by annotators who could not see the articles it links to, so the question shares few
words with the article holding the answer. A question's bundle is its passage plus every article
that passage links to; articles link each other wherever both are bundled. `linked` keeps those
links and `none` keeps only their text.

    python3 bench/iirc.py --per-kind 15
    python3 bench/run.py --tasks-file bench/data/iirc/agent/iirc-linked.json --variants main cards
"""

from __future__ import annotations

import argparse
import html
import json
import random
import re
import shutil
import tarfile
import urllib.parse
from collections import defaultdict
from pathlib import Path

import grade
from musique import HOST, slug

HERE = Path(__file__).resolve().parent
DATA = HERE / "data" / "iirc"
OUT = DATA / "agent"
QUESTIONS = ("https://iirc-dataset.s3.us-west-2.amazonaws.com/iirc_train_dev.tgz",
             "adfc34c8180337467105b2f534410c34e4fe43a82e6da03922440387802ca441")
ARTICLES = ("https://jamesf-incomplete-qa.s3.amazonaws.com/context_articles.tar.gz",
            "d239390a27ed3f4aa868afe126187e6d108990a7bcc2a3efb6cae5de917264c7")
KINDS = ("none", "linked")
ANCHOR = re.compile(r'<a href="([^"]*)">(.*?)</a>', re.DOTALL)
TAG = re.compile(r"<[^>]+>")


def _linked(passage: dict) -> list[dict]:
    return [q for q in passage["questions"] if eligible(q)]


def eligible(q: dict) -> bool:
    return q["answer"]["type"] == "span" and any(c["passage"] != "main" for c in q["context"])


def _paths(passage: dict, articles: dict[str, str]) -> dict[str, str]:
    """Lowercased link target -> concept path, for every target the archive holds."""
    paths: dict[str, str] = {}
    taken: set[str] = set()
    for link in passage["links"]:
        key = link["target"].lower()
        if key in paths or key not in articles:
            continue
        base = path = f"a/{slug(link['target'])}"
        n = 1
        while path in taken:
            n += 1
            path = f"{base}-{n}"
        taken.add(path)
        paths[key] = path
    return paths


def _article(text: str, paths: dict[str, str], kind: str) -> str:
    def anchor(m: re.Match[str]) -> str:
        label = TAG.sub("", m.group(2))
        path = paths.get(urllib.parse.unquote(m.group(1)).lower())
        return f"[{label}](/{path}.md)" if kind == "linked" and path else label

    return html.unescape(TAG.sub("", ANCHOR.sub(anchor, text)))


def bundle(passage: dict, articles: dict[str, str], kind: str) -> tuple[dict[str, tuple[str, str, str]], dict[str, str]]:
    """Returns concept path -> (title, type, body), and the target -> path map it linked by."""
    paths = _paths(passage, articles)
    text = passage["text"]
    if kind == "linked":
        for link in sorted(passage["links"], key=lambda l: -l["indices"][0]):
            s, e = link["indices"]
            path = paths.get(link["target"].lower())
            if path and 0 <= s < e <= len(text):
                text = f"{text[:s]}[{text[s:e]}](/{path}.md){text[e:]}"
    titles = {l["target"].lower(): l["target"] for l in reversed(passage["links"])}
    concepts = {f"main/{slug(passage['title'])}": (passage["title"], "Passage", text)}
    for key, path in paths.items():
        concepts[path] = (titles[key], "Article", _article(articles[key], paths, kind))
    return concepts, paths


def task(passage: dict, q: dict, kind: str, paths: dict[str, str]) -> dict | None:
    """The task for q, or None when a gold passage is not in the bundle."""
    gold = list(dict.fromkeys(c["passage"].lower() for c in q["context"] if c["passage"] != "main"))
    if any(g not in paths for g in gold):
        return None
    return {
        "id": q["qid"],
        "kind": "1link" if len(gold) == 1 else "2link+",
        "bundle": f"{kind}/{q['qid']}",
        "prompt": q["question"],
        "system_prompt": HOST,
        "expect": {
            "evidence": [f"main/{slug(passage['title'])}", *(paths[g] for g in gold)],
            "answer_any": [s["text"] for s in q["answer"]["answer_spans"]],
        },
    }


def _extract(archive: Path, member: str, dest: Path) -> Path:
    if not dest.exists():
        with tarfile.open(archive) as tar:
            f = tar.extractfile(member)
            assert f is not None, member
            dest.write_bytes(f.read())
    return dest


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--per-kind", type=int, default=15, help="Questions each for 1link and 2link+. Default: 15.")
    p.add_argument("--seed", type=int, default=0, help="Sampling seed. Default: 0.")
    args = p.parse_args()
    questions = grade.fetch(QUESTIONS[0], DATA / "iirc_train_dev.tgz", QUESTIONS[1])
    archive = grade.fetch(ARTICLES[0], DATA / "context_articles.tar.gz", ARTICLES[1])
    dev = json.loads(_extract(questions, "iirc_train_dev/dev.json", DATA / "dev.json").read_text())
    articles = json.loads(_extract(archive, "context_articles.json", DATA / "context_articles.json").read_text())

    pool: dict[str, list[tuple[dict, dict]]] = defaultdict(list)
    for passage in dev:
        _, paths = bundle(passage, articles, "none")
        for q in _linked(passage):
            t = task(passage, q, "none", paths)
            if t:
                pool[t["kind"]].append((passage, q))
    rng = random.Random(args.seed)
    chosen = [pq for kind in ("1link", "2link+")
              for pq in rng.sample(sorted(pool[kind], key=lambda pq: pq[1]["qid"]), args.per_kind)]

    shutil.rmtree(OUT, ignore_errors=True)
    for kind in KINDS:
        tasks = []
        for passage, q in chosen:
            concepts, paths = bundle(passage, articles, kind)
            for path, (title, type_, body) in concepts.items():
                f = OUT / kind / q["qid"] / f"{path}.md"
                f.parent.mkdir(parents=True, exist_ok=True)
                f.write_text(f"---\ntype: {type_}\ntitle: {json.dumps(title)}\n---\n{body}\n")
            tasks.append(task(passage, q, kind, paths))
        (OUT / f"iirc-{kind}.json").write_text(json.dumps(tasks, indent=2))
    print(f"wrote {len(chosen)} questions x {len(KINDS)} bundles to {OUT}")


if __name__ == "__main__":
    main()
