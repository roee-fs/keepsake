"""Compare search rankers on quality, latency at scale, write cost and storage.

Quality: BEIR (SciFact, NFCorpus, FiQA) and LongMemEval_S session retrieval, one tenant
per LongMemEval question. Scale: a synthetic 100k-concept tenant among 195k rows.
Rankers are the SQL functions in bench/rankers.sql, plus any --external program that
reads {"id","query"} JSON lines for a bundle and writes {"id","results":[paths]}.

    python3 bench/rankers.py
    python3 bench/rankers.py --datasets scifact --skip-scale --external okf=/path/to/okfsearch
"""

from __future__ import annotations

import argparse
import json
import re
import statistics
import subprocess
import tempfile
import time
import uuid
from pathlib import Path

import beir
import grade
import longmemeval
import run

RANKERS = {
    "ts_rank_cd (0.2.0)": "okf.rank_cd({t}, {q}, {k})",
    "ts_rank norm=1": "okf.rank_ts1({t}, {q}, {k})",
    "bm25 scan": "okf.rank_bm25_scan({t}, {q}, {k})",
    "bm25 postings (shipped)": "okf.rank_shipped({q}, NULL, {k}, {t})",
}


def shipped_ranker() -> str:
    """store.Search's SQL, read from reads.go so the benchmark cannot drift from what ships."""
    src = (run.ROOT / "internal" / "store" / "reads.go").read_text()
    sql = re.search(r"const searchSQL = `(.*?)`", src, re.DOTALL).group(1)
    return (
        "CREATE FUNCTION okf.rank_shipped(text, text, int, uuid)\n"
        "RETURNS TABLE (path text, type text, title text, description text, score float8)\n"
        f"LANGUAGE sql STABLE SET search_path = okf, pg_catalog AS $shipped${sql}$shipped$;"
    )


WORD = re.compile(r"\w+")
SYNTHETIC_QUERIES = {
    "1 rare term": "w4900",
    "2 rare terms": "w2500 | w4800",
    "3 terms, 1 common": "w40 | w2500 | w4800",
    "1 common term": "w1",
    "5 terms": "w3 | w90 | w700 | w2500 | w4800",
}


def terms(text: str) -> str:
    return " | ".join(WORD.findall(text))


def lit(s: str) -> str:
    return "'" + s.replace("'", "''") + "'"


class DB:
    def __init__(self, pg: run.Postgres) -> None:
        self.pg = pg

    def psql(self, sql: str, role: str | None = None) -> str:
        prefix = f"SET ROLE {role};\n" if role else ""
        return subprocess.run(
            [
                "docker",
                "exec",
                "-i",
                self.pg.name,
                "psql",
                "-v",
                "ON_ERROR_STOP=1",
                "-U",
                "postgres",
                "-d",
                "bench",
                "-q",
                "-A",
                "-F",
                "\t",
                "-t",
            ],
            input=prefix + sql,
            capture_output=True,
            text=True,
            check=True,
        ).stdout


def bundle_beir(
    name: str, root: Path
) -> tuple[dict[str, str], list[tuple[str, str]], dict[str, dict[str, int]]]:
    """Returns concept path -> doc id, (qid, query) pairs, and qrels keyed by doc id."""
    directory = beir.fetch(name)
    ids = beir.write_bundle(beir.jsonl(directory / "corpus.jsonl"), root)
    queries = {q["_id"]: q["text"] for q in beir.jsonl(directory / "queries.jsonl")}
    judged = beir.qrels(directory)
    return ids, [(qid, queries[qid]) for qid in judged], judged


def score(
    ranked: dict[str, list[str]], judged: dict[str, dict[str, int]]
) -> dict[str, float]:
    out = {}
    for name, fn, k in (
        ("nDCG@10", beir.ndcg, 10),
        ("R@5", beir.recall, 5),
        ("R@10", beir.recall, 10),
        ("R@100", beir.recall, 100),
    ):
        out[name] = round(
            statistics.mean(fn(ranked.get(q, []), rel, k) for q, rel in judged.items()),
            3,
        )
    return out


def rank_sql(
    db: DB, rows: list[tuple[str, str, str]], ids: dict[str, str], k: int = 100
) -> dict[str, tuple[dict[str, list[str]], float]]:
    """rows are (qid, tenant, OR-ed terms). Returns per ranker the ranking and mean ms per query."""
    db.psql(
        "DROP TABLE IF EXISTS q; CREATE TABLE q (qid text, tenant uuid, terms text);\n"
        + "".join(
            f"INSERT INTO q VALUES ({lit(a)}, {lit(b)}, {lit(c)});\n"
            for a, b, c in rows
            if c
        )
    )
    out = {}
    for label, call in RANKERS.items():
        # It ranks exactly as "bm25 postings" (SciFact 0.682 both) at up to 200x the cost; scale() times it.
        if label == "bm25 scan":
            continue
        started = time.monotonic()
        text = db.psql(
            f"SELECT q.qid, r.path FROM q, LATERAL {call.format(t='q.tenant', q='q.terms', k=k)} r;"
        )
        ms = 1000 * (time.monotonic() - started) / max(len(rows), 1)
        ranked: dict[str, list[str]] = {}
        for line in text.splitlines():
            qid, path = line.split("\t")
            ranked.setdefault(qid, []).append(ids[path])
        out[label] = (ranked, ms)
    return out


def rank_external(
    cmd: str, bundle: Path, queries: list[tuple[str, str]], ids: dict[str, str]
) -> tuple[dict[str, list[str]], float]:
    started = time.monotonic()
    stdin = "".join(json.dumps({"id": q, "query": text}) + "\n" for q, text in queries)
    proc = subprocess.run(
        [cmd, str(bundle)], input=stdin, capture_output=True, text=True, check=True
    )
    ms = 1000 * (time.monotonic() - started) / max(len(queries), 1)
    ranked, reported = {}, []
    for line in proc.stdout.splitlines():
        r = json.loads(line)
        ranked[r["id"]] = [
            ids.get(grade.concept_path(p), grade.concept_path(p)) for p in r["results"]
        ]
        if "ms" in r:
            reported.append(r["ms"])
    # A program that times its own searches excludes loading the bundle, as the SQL rankers do.
    return ranked, statistics.mean(reported) if reported else ms


def quality(args: argparse.Namespace, db: DB, binary: Path, app: str) -> list[dict]:
    results = []
    for name in args.datasets:
        rankings: dict[str, dict[str, list[str]]] = {}
        latency: dict[str, list[float]] = {}
        judged: dict[str, dict[str, int]] = {}
        rows: list[tuple[str, str, str]] = []
        ids: dict[str, str] = {}
        with tempfile.TemporaryDirectory() as tmp:
            if name in beir.DATASETS:
                tenant = str(uuid.uuid4())
                ids, queries, judged = bundle_beir(name, Path(tmp))
                run.sh(str(binary), "import", "--dsn", app, "--tenant", tenant, tmp)
                rows = [(q, tenant, terms(text)) for q, text in queries]
                for label, cmd in args.external.items():
                    r, ms = rank_external(cmd, Path(tmp), queries, ids)
                    rankings[label], latency[label] = r, [ms]
            else:
                data = [
                    d for d in longmemeval.load() if longmemeval.kind(d) != "abstention"
                ]
                for d in data[: args.longmemeval_limit]:
                    tenant, root = str(uuid.uuid4()), Path(tmp) / d["question_id"]
                    sids = longmemeval.bundle_session(d, root)
                    ids.update({f"{p}\x00{tenant}": s for p, s in sids.items()})
                    run.sh(
                        str(binary),
                        "import",
                        "--dsn",
                        app,
                        "--tenant",
                        tenant,
                        str(root),
                    )
                    rows.append((d["question_id"], tenant, terms(d["question"])))
                    judged[d["question_id"]] = dict.fromkeys(d["answer_session_ids"], 1)
                    for label, cmd in args.external.items():
                        r, ms = rank_external(
                            cmd, root, [(d["question_id"], d["question"])], sids
                        )
                        rankings.setdefault(label, {}).update(r)
                        latency.setdefault(label, []).append(ms)
                # Session ids are unique across the dataset, so paths map back without the tenant.
                ids = {k.split("\x00")[0]: v for k, v in ids.items()}
        for label, (ranked, ms) in rank_sql(db, rows, ids).items():
            rankings[label], latency[label] = ranked, [ms]
        for label, ranked in rankings.items():
            row = {
                "dataset": name,
                "ranker": label,
                **score(ranked, judged),
                "ms/query": round(statistics.mean(latency[label]), 1),
            }
            results.append(row)
            print(json.dumps(row), flush=True)
    return results


SYNTHETIC = """
INSERT INTO okf.concept (tenant_id, path, type, title, description, body)
SELECT {tenant}::uuid, 'c/' || i, 'note',
  'w' || floor(power(random(), 3) * 5000)::int || ' w' || floor(power(random(), 3) * 5000)::int,
  (SELECT string_agg('w' || floor(power(random(), 3) * 5000)::int, ' ') FROM generate_series(1, 12) x WHERE x > 0 * i),
  (SELECT string_agg('w' || floor(power(random(), 3) * 5000)::int, ' ') FROM generate_series(1, 150) x WHERE x > 0 * i)
FROM generate_series(1, {n}) i;
"""


def timed(db: DB, sql: str) -> float:
    started = time.monotonic()
    db.psql(sql)
    return time.monotonic() - started


def scale(db: DB) -> dict:
    """Write cost with and without the posting trigger, then latency in a 100k tenant."""
    n = 20000
    untriggered = lit(str(uuid.uuid4()))
    db.psql("ALTER TABLE okf.concept DISABLE TRIGGER concept_posting;")
    without = timed(db, SYNTHETIC.format(tenant=untriggered, n=n))
    db.psql("ALTER TABLE okf.concept ENABLE TRIGGER concept_posting;")
    # Postings for the concepts written without the trigger, so the sizes below cover every concept.
    db.psql(
        "INSERT INTO okf.posting (tenant_id, lexeme, path, tf, dl) "
        "SELECT c.tenant_id, u.lexeme, c.path, coalesce(array_length(u.positions, 1), 1), length(c.search) "
        f"FROM okf.concept c, unnest(c.search) u WHERE c.tenant_id = {untriggered}::uuid;"
    )
    with_trigger = timed(db, SYNTHETIC.format(tenant=lit(str(uuid.uuid4())), n=n))
    big, small = str(uuid.uuid4()), str(uuid.uuid4())
    db.psql(SYNTHETIC.format(tenant=lit(big), n=100000))
    for tenant in [small] + [str(uuid.uuid4()) for _ in range(10)]:
        db.psql(SYNTHETIC.format(tenant=lit(tenant), n=5000))
    db.psql("VACUUM ANALYZE okf.concept; VACUUM ANALYZE okf.posting;")
    sizes = db.psql(
        "SELECT pg_total_relation_size('okf.concept'), pg_total_relation_size('okf.posting'), "
        "(SELECT count(*) FROM okf.concept), (SELECT count(*) FROM okf.posting);"
    ).split("\t")

    latency: dict[str, dict[str, float]] = {}
    for label, call in RANKERS.items():
        latency[label] = {}
        for tenant_label, tenant in (("100k", big), ("5k", small)):
            for qlabel, q in SYNTHETIC_QUERIES.items():
                if tenant_label == "5k" and qlabel != "3 terms, 1 common":
                    continue
                sql = f"EXPLAIN (ANALYZE, TIMING OFF, SUMMARY ON) SELECT * FROM {call.format(t=lit(tenant) + '::uuid', q=lit(q), k=10)};"
                runs = []
                for _ in range(4):
                    out = db.psql(sql)
                    runs.append(
                        float(re.search(r"Execution Time: ([\d.]+) ms", out).group(1))
                    )
                latency[label][f"{tenant_label}: {qlabel}"] = round(
                    statistics.median(runs[1:]), 1
                )
        print(json.dumps({"ranker": label, **latency[label]}), flush=True)

    # The superuser above skips RLS, so this times the shipped ranker as the app role, as production runs it.
    rls = db.psql(
        f"SELECT set_config('okf.current_tenant', {lit(big)}, false);\n"
        f"EXPLAIN (ANALYZE, TIMING OFF, SUMMARY ON) SELECT * FROM okf.rank_shipped('w40 | w2500 | w4800', NULL, 10, {lit(big)}::uuid);",
        role="okf_app",
    )
    return {
        "write_s_per_1k_without_trigger": round(1000 * without / n, 3),
        "write_s_per_1k_with_trigger": round(1000 * with_trigger / n, 3),
        "concept_bytes": int(sizes[0]),
        "posting_bytes": int(sizes[1]),
        "concepts": int(sizes[2]),
        "postings": int(sizes[3]),
        "bm25_postings_as_app_role_ms": float(
            re.search(r"Execution Time: ([\d.]+) ms", rls).group(1)
        ),
        "latency_ms": latency,
    }


def report(quality_rows: list[dict], scale_row: dict | None) -> str:
    out = [
        "## Quality",
        "",
        "| dataset | ranker | nDCG@10 | R@5 | R@10 | R@100 | ms/query |",
        "|---|---|---|---|---|---|---|",
    ]
    for r in quality_rows:
        out.append(
            f"| {r['dataset']} | {r['ranker']} | {r['nDCG@10']} | {r['R@5']} | {r['R@10']} | {r['R@100']} | {r['ms/query']} |"
        )
    if scale_row:
        cols = list(next(iter(scale_row["latency_ms"].values())))
        out += [
            "",
            "## Latency (ms, median of 3 warm runs)",
            "",
            "| ranker | " + " | ".join(cols) + " |",
            "|---|" + "---|" * len(cols),
        ]
        for label, row in scale_row["latency_ms"].items():
            out.append(f"| {label} | " + " | ".join(str(row[c]) for c in cols) + " |")
        out += [
            "",
            (
                f"Writes: {scale_row['write_s_per_1k_without_trigger']}s per 1k concepts without the posting trigger, "
                f"{scale_row['write_s_per_1k_with_trigger']}s with it."
            ),
            (
                f"Storage: concept {scale_row['concept_bytes'] / 1e6:.0f} MB, posting {scale_row['posting_bytes'] / 1e6:.0f} MB "
                f"({scale_row['postings']:,} postings for {scale_row['concepts']:,} concepts)."
            ),
            f"bm25 postings as okf_app under FORCE RLS, 100k tenant, 3 terms: {scale_row['bm25_postings_as_app_role_ms']} ms.",
        ]
    return "\n".join(out)


def main() -> None:
    p = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    p.add_argument(
        "--datasets",
        nargs="*",
        default=[*beir.DATASETS, "longmemeval"],
        choices=[*beir.DATASETS, "longmemeval"],
    )
    p.add_argument(
        "--longmemeval-limit",
        type=int,
        default=None,
        help="Evaluate only the first N LongMemEval questions.",
    )
    p.add_argument(
        "--external",
        nargs="*",
        default=[],
        metavar="LABEL=CMD",
        help="Rankers outside Postgres.",
    )
    p.add_argument(
        "--skip-scale",
        action="store_true",
        help="Skip the synthetic latency and write-cost run.",
    )
    args = p.parse_args()
    args.external = dict(e.split("=", 1) for e in args.external)

    out = run.BENCH / "results" / f"rankers-{time.strftime('%Y%m%d-%H%M%S')}"
    out.mkdir(parents=True)
    binary = out / "keepsake"
    run.sh("go", "build", "-o", str(binary), "./cmd/keepsake", cwd=run.ROOT)
    pg = run.Postgres()
    try:
        db = DB(pg)
        run.sh(str(binary), "migrate", "--dsn", pg.dsn("okf_owner", "owner"))
        db.psql((run.BENCH / "rankers.sql").read_text() + shipped_ranker())
        # scale() first, so its sizes count only the concepts it writes.
        scale_row = None if args.skip_scale else scale(db)
        rows = quality(args, db, binary, pg.dsn("okf_app", "app"))
    finally:
        pg.close()
    (out / "results.json").write_text(
        json.dumps({"quality": rows, "scale": scale_row}, indent=2) + "\n"
    )
    text = report(rows, scale_row)
    (out / "report.md").write_text(text + "\n")
    print(f"\n{text}\n\n{out}")


if __name__ == "__main__":
    main()
