"""Score okf_search's ranking on BEIR datasets, with no agent in the loop.

Imports each corpus as one concept per document, sends every test query through the
okf_search MCP tool, and reports nDCG@10 and recall beside BEIR's published BM25.

    python3 bench/beir.py                  # scifact and nfcorpus
    python3 bench/beir.py --datasets scifact
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import re
import subprocess
import tempfile
import time
import urllib.request
import uuid
import zipfile
from pathlib import Path

import run

DATA = run.BENCH / "data"
URL = "https://public.ukp.informatik.tu-darmstadt.de/thakur/BEIR/datasets/{}.zip"

# MD5s from the BEIR README. BM25 is BEIR paper Table 2 (Anserini, k1=0.9, b=0.4).
DATASETS = {
    "scifact": {
        "md5": "5f7d1de60b170fc8027bb7898e2efca1",
        "bm25_ndcg10": 0.665,
        "bm25_recall100": 0.908,
    },
    "nfcorpus": {
        "md5": "a89dba18a62ef92f7d323ec890a0d38d",
        "bm25_ndcg10": 0.325,
        "bm25_recall100": 0.250,
    },
    "fiqa": {
        "md5": "17918ed23cd04fb15047f73e6c3bd9d9",
        "bm25_ndcg10": 0.236,
        "bm25_recall100": 0.539,
    },
}
K = 100


def fetch(name: str) -> Path:
    directory = DATA / name
    if directory.exists():
        return directory
    DATA.mkdir(parents=True, exist_ok=True)
    archive = DATA / f"{name}.zip"
    urllib.request.urlretrieve(URL.format(name), archive)
    digest = hashlib.md5(archive.read_bytes(), usedforsecurity=False).hexdigest()
    if digest != DATASETS[name]["md5"]:
        archive.unlink()
        raise SystemExit(f"{name}.zip has MD5 {digest}, want {DATASETS[name]['md5']}")
    with zipfile.ZipFile(archive) as z:
        z.extractall(DATA)
    archive.unlink()
    return directory


def jsonl(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def to_path(doc_id: str) -> str:
    return "doc/" + re.sub(r"[^A-Za-z0-9_-]", "_", doc_id)


def write_bundle(corpus: list[dict], root: Path) -> dict[str, str]:
    """Writes one concept per document and returns concept path -> BEIR doc id."""
    (root / "doc").mkdir(parents=True)
    ids = {}
    for doc in corpus:
        path = to_path(doc["_id"])
        ids[path] = doc["_id"]
        # A JSON string is a valid YAML double-quoted scalar.
        front = f"---\ntype: Document\ntitle: {json.dumps(doc.get('title') or '', ensure_ascii=False)}\n---\n"
        (root / f"{path}.md").write_text(front + (doc.get("text") or "") + "\n")
    return ids


def qrels(directory: Path) -> dict[str, dict[str, int]]:
    out: dict[str, dict[str, int]] = {}
    for line in (directory / "qrels" / "test.tsv").read_text().splitlines()[1:]:
        qid, doc_id, score = line.split("\t")
        out.setdefault(qid, {})[doc_id] = int(score)
    return out


def ndcg(ranked: list[str], relevant: dict[str, int], k: int) -> float:
    # Linear gain, as trec_eval's ndcg_cut and so BEIR's pytrec_eval score it.
    dcg = sum(relevant.get(d, 0) / math.log2(i + 2) for i, d in enumerate(ranked[:k]))
    ideal = sorted(relevant.values(), reverse=True)[:k]
    idcg = sum(g / math.log2(i + 2) for i, g in enumerate(ideal))
    return dcg / idcg if idcg else 0.0


def recall(ranked: list[str], relevant: dict[str, int], k: int) -> float:
    hits = {d for d, g in relevant.items() if g > 0}
    return len(hits & set(ranked[:k])) / len(hits) if hits else 0.0


def search(url: str, query: str) -> list[str]:
    body = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": "okf_search", "arguments": {"query": query, "limit": K}},
    }
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Accept": "application/json"},
    )
    with urllib.request.urlopen(req) as resp:
        result = json.load(resp)["result"]
    if result.get("isError"):
        raise RuntimeError(result["content"][0]["text"])
    return [hit["path"] for hit in result["structuredContent"]["results"]]


def evaluate(name: str, binary: Path, pg: run.Postgres) -> dict:
    directory = fetch(name)
    corpus = jsonl(directory / "corpus.jsonl")
    queries = {q["_id"]: q["text"] for q in jsonl(directory / "queries.jsonl")}
    judged = qrels(directory)
    app, tenant = pg.dsn("okf_app", "app"), str(uuid.uuid4())
    with tempfile.TemporaryDirectory() as tmp:
        ids = write_bundle(corpus, Path(tmp))
        started = time.monotonic()
        run.sh(str(binary), "import", "--dsn", app, "--tenant", tenant, tmp)
        import_s = time.monotonic() - started

    port = run.free_port()
    server = subprocess.Popen(
        [
            str(binary),
            "serve",
            "--dsn",
            app,
            "--tenant",
            tenant,
            "--host",
            "127.0.0.1",
            "--port",
            str(port),
        ],
        stderr=subprocess.DEVNULL,
        env={**os.environ, "KEEPSAKE_UI": "false"},
    )
    try:
        run.wait_ready(f"http://127.0.0.1:{port}/readyz", server)
        scores = {"ndcg@10": [], "recall@10": [], f"recall@{K}": []}
        latencies = []
        for qid, relevant in judged.items():
            started = time.monotonic()
            ranked = [
                ids.get(p, p)
                for p in search(f"http://127.0.0.1:{port}/mcp", queries[qid])
            ]
            latencies.append(time.monotonic() - started)
            scores["ndcg@10"].append(ndcg(ranked, relevant, 10))
            scores["recall@10"].append(recall(ranked, relevant, 10))
            scores[f"recall@{K}"].append(recall(ranked, relevant, K))
    finally:
        server.terminate()
        server.wait()

    latencies.sort()
    return {
        "dataset": name,
        "docs": len(corpus),
        "queries": len(judged),
        "import_s": round(import_s, 1),
        **{m: round(sum(v) / len(v), 3) for m, v in scores.items()},
        "p50_ms": round(1000 * latencies[len(latencies) // 2], 1),
        "p95_ms": round(1000 * latencies[int(len(latencies) * 0.95)], 1),
        "bm25_ndcg@10": DATASETS[name]["bm25_ndcg10"],
        f"bm25_recall@{K}": DATASETS[name]["bm25_recall100"],
    }


def main() -> None:
    p = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    p.add_argument(
        "--datasets", nargs="*", default=list(DATASETS), choices=list(DATASETS)
    )
    args = p.parse_args()

    out = run.BENCH / "results" / f"beir-{time.strftime('%Y%m%d-%H%M%S')}"
    out.mkdir(parents=True)
    binary = out / "keepsake"
    run.sh("go", "build", "-o", str(binary), "./cmd/keepsake", cwd=run.ROOT)
    pg = run.Postgres()
    try:
        run.sh(str(binary), "migrate", "--dsn", pg.dsn("okf_owner", "owner"))
        rows = [evaluate(name, binary, pg) for name in args.datasets]
    finally:
        pg.close()

    (out / "results.json").write_text(json.dumps(rows, indent=2) + "\n")
    cols = [
        "dataset",
        "docs",
        "queries",
        "ndcg@10",
        "bm25_ndcg@10",
        "recall@10",
        f"recall@{K}",
        f"bm25_recall@{K}",
        "p50_ms",
        "p95_ms",
        "import_s",
    ]
    print("| " + " | ".join(cols) + " |\n|" + "---|" * len(cols))
    for r in rows:
        print("| " + " | ".join(str(r[c]) for c in cols) + " |")
    print(f"\n{out}")


if __name__ == "__main__":
    main()
