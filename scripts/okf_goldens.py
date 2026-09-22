"""Write okf/testdata/goldens/*.json from the Python implementation.

Run once against PY_SPEC_SHA with a migrated database. The Go tests read these
files; nothing reads this script after Task 15 deletes it.
"""

import base64
import hashlib
import hmac
import json
import os
import sys
import uuid
from pathlib import Path

from keepsake.cli import export_bundle, import_bundle
from keepsake.server.auth import Auth
from keepsake.store.concepts import ConceptStore, _tsquery
from keepsake.store.pool import Store
from okf_core import parse
from okf_core.links import extract_links

ROOT = Path(__file__).resolve().parent.parent
CORPUS = ROOT / "okf" / "testdata" / "corpus"
OUT = ROOT / "okf" / "testdata" / "goldens"


def _dump(name: str, rows: list[dict]) -> None:
    (OUT / name).write_text(json.dumps(rows, indent=2, ensure_ascii=False) + "\n")


def parse_goldens() -> list[dict]:
    rows = []
    for file in sorted(CORPUS.glob("*.md")):
        raw = file.read_bytes()
        # Base64, not text: one case is deliberately not UTF-8, and JSON cannot carry it.
        row = {"name": file.stem, "input_b64": base64.b64encode(raw).decode()}
        try:
            c = parse(raw.decode("utf-8"), "corpus/" + file.stem)
        except Exception as exc:  # noqa: BLE001 - recording the refusal is the point
            row |= {"ok": False, "error_contains": type(exc).__name__}
        else:
            row |= {
                "ok": True,
                "type": c.type,
                "title": c.title,
                "description": c.description,
                "body": c.body,
                "frontmatter_json": json.dumps(c.frontmatter, default=str),
                "links": list(c.links),
            }
        rows.append(row)
    return rows


def export_goldens(dsn: str, tmp: Path) -> list[dict]:
    store = Store(dsn)
    concepts = ConceptStore(store)
    rows = []
    try:
        for file in sorted(CORPUS.glob("*.md")):
            tenant = uuid.uuid4()
            src = tmp / file.stem / "in"
            src.mkdir(parents=True)
            (src / "doc.md").write_bytes(file.read_bytes())
            try:
                import_bundle(concepts, tenant, src)
            except Exception:  # noqa: BLE001, S112 - refusals live in parse.json
                continue
            out = tmp / file.stem / "out"
            export_bundle(concepts, tenant, out)
            with store.scope(tenant) as conn:
                jsonb_text = conn.execute(
                    "SELECT frontmatter::text FROM concept WHERE path = 'doc'"
                ).fetchone()[0]
            c = concepts.read(tenant, "doc")
            rows.append(
                {
                    "name": file.stem,
                    "path": "doc",
                    "jsonb_text": jsonb_text,
                    "type": c.type,
                    "title": c.title,
                    "description": c.description,
                    "body": c.body,
                    "exported": (out / "doc.md").read_text(encoding="utf-8"),
                }
            )
    finally:
        store.close()
    return rows


def link_goldens() -> list[dict]:
    bodies = json.loads((CORPUS.parent / "link_bodies.json").read_text())
    return [
        {"body": b["body"], "path": b["path"], "links": list(extract_links(b["body"], b["path"]))}
        for b in bodies
    ]


def tsquery_goldens() -> list[dict]:
    queries = json.loads((CORPUS.parent / "queries.json").read_text())
    return [{"query": q, "terms": _tsquery(q)} for q in queries]


def session_goldens() -> list[dict]:
    rows = []
    for password in ["test-admin-password", "pässwörd", "🎉"]:
        auth = Auth(password)
        expiry = 4102444800  # 2100-01-01, so the golden never expires.
        # The key as PY_SPEC_SHA derives it (HMAC before #17, scrypt after), not a copy.
        digest = hmac.new(auth._key, str(expiry).encode(), hashlib.sha256).hexdigest()
        cookie = f"{expiry}.{digest}"
        assert auth.valid(cookie)
        rows.append({"password": password, "expiry": expiry, "cookie": cookie})
    return rows


if __name__ == "__main__":
    import tempfile

    OUT.mkdir(parents=True, exist_ok=True)
    _dump("parse.json", parse_goldens())
    _dump("links.json", link_goldens())
    _dump("tsquery.json", tsquery_goldens())
    _dump("session.json", session_goldens())
    with tempfile.TemporaryDirectory() as tmp:
        _dump("export.json", export_goldens(sys.argv[1] if len(sys.argv) > 1 else os.environ["KEEPSAKE_DSN"], Path(tmp)))
