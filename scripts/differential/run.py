"""Run the Python and Go servers side by side and compare them operation by operation.

Temporary: it exists to prove the Go port and goes when the Python server does.
Usage: `uv run python scripts/differential/run.py <seed>`. See README.md.
"""

from __future__ import annotations

import asyncio
import json
import math
import os
import random
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from collections import Counter
from datetime import UTC, datetime
from pathlib import Path
from typing import Any
from urllib.parse import quote, urlparse

import httpx2 as httpx
import psycopg
from mcp import Client
from mcp.client.streamable_http import streamable_http_client

from okf_core import parse

REPO = Path(__file__).resolve().parents[2]
TESTDATA = REPO / "okf" / "testdata"
PG = os.environ.get("DIFF_PG", "127.0.0.1:55435/postgres")
OWNER_DSN = f"postgresql://okf_owner:owner@{PG}"
APP_DSN = f"postgresql://okf_app:app@{PG}"
PASSWORD = "diff-admin-password"
OPS = 2000
SIDES = {"py": ("okf_py", 18001), "go": ("okf_go", 18002)}
MAX_VERSION = 2_147_483_647
# Versions are ints on both sides; divergence 4's float leniency MUST NOT reach them.
VERSION_KEYS = {"version", "current_version", "expected_version"}
LEGACY_VERSION = "2025-11-25"
MODERN_VERSION = "2026-07-28"
MODERN_META = {
    "io.modelcontextprotocol/protocolVersion": MODERN_VERSION,
    "io.modelcontextprotocol/clientCapabilities": {},
}

WORDS = [
    "dormant",
    "PKCE",
    "indextime",
    "rule",
    "token",
    "alert",
    "runbook",
    "decision",
    "tenant",
    "rotate",
    "secret",
    "latency",
    "café",
    "naïve",
    "日本語",
    "x_y",
    "a-b-c",
    "123",
    "SELECT",
    "the",
    "and",
    "config.key",
    "https://example.com/x",
]
SEGMENTS = ["detect", "respond", "runbooks", "decisions", "café", "日本", "a b"]
TYPES = ["Concept", "Concept", "Runbook", "Decision", "True", "['a', 'b']", "", "  "]
BAD_PATHS = [
    "",
    " ",
    "/abs/x",
    "a/../b",
    "..",
    "index",
    "log",
    "a//b",
    "a/",
    "tab\tx",
    "nul\x00x",
    "del\x7fx",
    "x" * 1025,
    "日" * 400,
]
PATTERNS = [
    "dormant",
    "PKCE|token",
    "^#",
    "café",
    "[0-9]+",
    "d.rmant",
    "(",
    "[",
    "a{2,1}",
    "it's (",
    "日本(",
    "\\",
    "*",
    "",
    "x\ny",
]

BAD_UUIDS = [
    "nope",
    "",
    "é",
    "{}",
    "urn:uuid:",
    "URN:UUID:cd613e30-d8f1-6adf-91b7-584a2265b1f5",
    "{cd613e30-d8f1-6adf-91b7-584a2265b1f5}",
    "cd613e30d8f16adf91b7584a2265b1f5",
    "cd613e30-d8f16-adf-91b7-584a2265b1f5",
    "-----",
    "----",
    "cd613e30-d8f1-6adf-91b7-584a2265b1f5 ",
]
LONE_SURROGATE = b'"\\ud800"'
INVALID_UTF8 = b"\xff"
LOGIN_BODIES = [
    ("application/json", b"{"),
    ("application/json", b"[1]"),
    ("application/json", b'{"password":1}'),
    ("application/json", b'{"password": "x", "password": 2}'),
    ("application/json", b"\xef\xbb\xbf{}"),
    ("application/json", b'{"password":"\xff"}'),
    ("application/json", b"[NaN]"),
    ("application/json", b"[1,]"),
    ("application/json", LONE_SURROGATE),
    ("text/plain", b'{"password":1}'),
    ("application/x+json", b"1 2"),
    ("", b'{"password":"x"}'),
    ("application/json", b"null"),
    ("application/json", b'{"password": 1e999}'),
    ("application/json", b"-1e400"),
    ("application/json", b'{"password": "\\ud800"}'),
    ("text/plain", INVALID_UTF8),
]


def corpus() -> tuple[list[tuple[str, Any]], list[str]]:
    """Frontmatter (key, value) pairs and titles from the Task 1 corpus."""
    pairs: list[tuple[str, Any]] = []
    titles: list[str] = []
    for file in sorted((TESTDATA / "corpus").glob("*.md")):
        try:
            c = parse(file.read_text(encoding="utf-8"), file.stem)
        except Exception:  # noqa: BLE001, S112 - the REFUSE_* files are meant to fail.
            continue
        titles.append(c.title)
        for key, value in c.frontmatter.items():
            try:
                pairs.append((key, json.loads(json.dumps(value, allow_nan=False))))
            except TypeError, ValueError:
                continue  # Dates, sets and NaN cannot travel as JSON arguments.
    return pairs, titles


FRONTMATTER, TITLES = corpus()
QUERIES = json.loads((TESTDATA / "queries.json").read_text())
LINK_BODIES = [
    b["body"] for b in json.loads((TESTDATA / "link_bodies.json").read_text())
]


class Gen:
    """Seeded operations. Paths and versions are tracked from the Python answers."""

    def __init__(self, rng: random.Random) -> None:
        self.r = rng
        self.versions: dict[str, int] = {}

    def new_path(self) -> str:
        depth = self.r.choice([0, 1, 1, 2])
        segs = [self.r.choice(SEGMENTS) for _ in range(depth)]
        return "/".join(
            [*segs, f"{self.r.choice(WORDS[:12]).lower()}-{self.r.randrange(40)}"]
        )

    def path(self) -> str:
        if self.versions and self.r.random() < 0.7:
            return self.r.choice(sorted(self.versions))
        return self.new_path()

    def body(self) -> str:
        r = self.r
        parts = [r.choice(WORDS) for _ in range(r.randrange(30))]
        for _ in range(r.randrange(4)):
            kind = r.randrange(4)
            if kind == 0 and self.versions:
                parts.append(f"[x](/{r.choice(sorted(self.versions))}.md)")
            elif kind == 1:
                parts.append(f"[y]({self.new_path().split('/')[-1]}.md#s)")
            elif kind == 2:
                parts.append(r.choice(LINK_BODIES))
            else:
                parts.append("![img](pic.md) [ext](https://example.com/a.md)")
        if r.random() < 0.02:
            parts.append("x\x00y")
        if r.random() < 0.005:
            parts.append("z" * (256 * 1024))
        return (" " if r.random() < 0.5 else "\n").join(parts)

    def fields(self) -> dict[str, Any]:
        r = self.r
        out: dict[str, Any] = {}
        if r.random() < 0.8:
            out["type"] = r.choice(TYPES)
        if r.random() < 0.6:
            out["title"] = (
                r.choice(TITLES) if r.random() < 0.5 else " ".join(r.sample(WORDS, 2))
            )
        if r.random() < 0.4:
            out["description"] = r.choice(QUERIES)
        if r.random() < 0.8:
            out["body"] = self.body()
        if r.random() < 0.6:
            out["frontmatter"] = dict(r.sample(FRONTMATTER, r.randrange(5)))
        return out

    def op(self) -> tuple[str, dict[str, Any], tuple[str, ...] | None]:
        """(tool, arguments, fields a schema error must name, or None if it is not one)."""
        r = self.r
        kind = r.choices(
            [
                "create",
                "update",
                "relate",
                "read",
                "list",
                "search",
                "grep",
                "bad",
                "missing",
            ],
            [25, 25, 10, 12, 5, 8, 6, 6, 3],
        )[0]
        if kind == "create":
            path = r.choice(BAD_PATHS) if r.random() < 0.05 else self.path()
            return "okf_create", {"path": path, "type": "Concept"} | self.fields(), None
        if kind == "update":
            path = self.path()
            args: dict[str, Any] = {"path": path} | self.fields()
            current = self.versions.get(path, 1)
            choice = r.randrange(4)
            if choice == 1:
                args["expected_version"] = current
            elif choice == 2:
                args["expected_version"] = current - 1 if current > 1 else current + 1
            elif choice == 3:
                args["expected_version"] = r.randint(1, MAX_VERSION)
            return "okf_update", args, None
        if kind == "relate":
            to = r.choice(BAD_PATHS) if r.random() < 0.05 else self.path()
            return "okf_relate", {"from_path": self.path(), "to_path": to}, None
        if kind == "read":
            return "okf_read", {"path": self.path()}, None
        if kind == "list":
            args = {}
            if r.random() < 0.7:
                args["prefix"] = r.choice(
                    ["", *SEGMENTS, "detect/", "a", "日本/", "%", "_"]
                )
            return "okf_list", args, None
        if kind == "search":
            args = {
                "query": r.choice(QUERIES + WORDS),
                "limit": r.choice([0, 1, 5, 20, 200]),
            }
            if r.random() < 0.3:
                args["prefix"] = r.choice(SEGMENTS + ["detect/", "%"])
            return "okf_search", args, None
        if kind == "grep":
            return (
                "okf_grep",
                {"pattern": r.choice(PATTERNS), "limit": r.choice([0, 1, 10, 200])},
                None,
            )
        if kind == "missing":
            name = f"missing/{uuid.UUID(int=r.getrandbits(128))}"
            return r.choice(
                [
                    ("okf_read", {"path": name}, None),
                    ("okf_update", {"path": name, "body": "x"}, None),
                    ("okf_relate", {"from_path": name, "to_path": self.path()}, None),
                ]
            )
        return r.choice(
            [
                ("okf_read", {"path": 5}, ("path",)),
                ("okf_read", {}, ("path",)),
                ("okf_list", {"prefix": None}, ("prefix",)),
                ("okf_search", {"query": "x", "limit": 201}, ("limit",)),
                ("okf_search", {"query": "x", "limit": -1}, ("limit",)),
                ("okf_search", {"query": "x", "limit": "3"}, ("limit",)),
                ("okf_search", {"query": "x"}, ("limit",)),
                ("okf_search", {"query": "x", "limit": 1.5}, ("limit",)),
                ("okf_search", {"query": ["x"], "limit": 1}, ("query",)),
                ("okf_grep", {"pattern": "x", "limit": 1000}, ("limit",)),
                (
                    "okf_create",
                    {"path": self.new_path(), "type": "C", "links": ["a"]},
                    ("links",),
                ),
                ("okf_create", {"path": self.new_path()}, ("type",)),
                (
                    "okf_create",
                    {"path": self.new_path(), "type": "C", "frontmatter": []},
                    ("frontmatter",),
                ),
                (
                    "okf_create",
                    {"path": self.new_path(), "type": "C", "body": None},
                    ("body",),
                ),
                (
                    "okf_update",
                    {"path": self.path(), "expected_version": 0},
                    ("expected_version",),
                ),
                (
                    "okf_update",
                    {"path": self.path(), "expected_version": MAX_VERSION + 1},
                    ("expected_version",),
                ),
                (
                    "okf_update",
                    {"path": self.path(), "frontmatter": None},
                    ("frontmatter",),
                ),
                ("okf_relate", {"from_path": self.path()}, ("to_path",)),
                # Not schema errors: 2.0 is an integer to JSON Schema, and the tool is unknown.
                ("okf_search", {"query": "dormant", "limit": 2.0}, None),
                ("okf_nope", {}, None),
            ]
        )

    def learn(self, result: Any) -> None:
        if isinstance(result, dict) and "version" in result and "path" in result:
            self.versions[result["path"]] = result["version"]


def same(a: Any, b: Any, where: str, out: list[str]) -> None:
    """Append every difference between two parsed JSON values, key order included."""
    if isinstance(a, bool) or isinstance(b, bool):
        if a is not b:
            out.append(f"{where}: {a!r} != {b!r}")
    elif isinstance(a, (int, float)) and isinstance(b, (int, float)):
        if isinstance(a, int) and isinstance(b, int):
            if a != b:
                out.append(f"{where}: {a!r} != {b!r}")
        # Divergence 4: float JSON text may differ (1.0 vs 1, 1e+20 vs 100000000000000000000).
        elif not math.isclose(a, b, rel_tol=1e-6):
            out.append(f"{where}: {a!r} != {b!r}")
    elif isinstance(a, dict) and isinstance(b, dict):
        if list(a) != list(b):
            out.append(f"{where}: keys {list(a)} != {list(b)}")
        for k in a:
            if (
                k in VERSION_KEYS
                and k in b
                and (type(a[k]), a[k]) != (type(b[k]), b[k])
            ):
                out.append(f"{where}.{k}: {a[k]!r} != {b[k]!r}")
            elif k in b:
                same(a[k], b[k], f"{where}.{k}", out)
    elif isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            out.append(f"{where}: {len(a)} items != {len(b)}: {a!r:.300} vs {b!r:.300}")
            return
        for i, (x, y) in enumerate(zip(a, b, strict=True)):
            same(x, y, f"{where}[{i}]", out)
    elif a != b:
        out.append(f"{where}: {a!r:.300} != {b!r:.300}")


async def call(client: Client, tool: str, args: dict[str, Any]) -> dict[str, Any]:
    try:
        res = await client.call_tool(tool, args)
    except Exception as exc:  # noqa: BLE001 - a protocol error is itself compared.
        return {"raised": f"{type(exc).__name__}: {exc}"}
    return {
        "isError": bool(res.is_error),
        "structured": res.structured_content,
        "content": [(c.type, getattr(c, "text", None)) for c in res.content],
    }


def compare_call(
    tool: str, schema_fields: tuple[str, ...] | None, py: dict, go: dict
) -> list[str]:
    out: list[str] = []
    if "raised" in py or "raised" in go:
        same(py.get("raised"), go.get("raised"), "raised", out)
        return out
    same(py["isError"], go["isError"], "isError", out)
    if len(py["content"]) != 1 or len(go["content"]) != 1 or out:
        same(py["content"], go["content"], "content", out)
        return out
    (pt, ptext), (gt, gtext) = py["content"][0], go["content"][0]
    same(pt, gt, "content.type", out)
    if py["isError"]:
        if schema_fields is not None:
            # Divergence 2: schema-error wording is the validator's; both MUST name the tool and field.
            for text in (ptext, gtext):
                if not text.startswith(f"{tool}: ") or not all(
                    f in text for f in schema_fields
                ):
                    out.append(f"schema error does not name {schema_fields}: {text!r}")
        else:
            same(ptext, gtext, "error", out)
    else:
        same(py["structured"], go["structured"], "structuredContent", out)
        same(json.loads(ptext), json.loads(gtext), "text", out)
    return out


def sh(*argv: str, env: dict[str, str] | None = None) -> str:
    done = subprocess.run(
        argv, env=os.environ | (env or {}), capture_output=True, text=True, check=False
    )
    if done.returncode != 0:
        sys.exit(f"{' '.join(argv)} failed ({done.returncode}): {done.stderr}")
    return done.stdout


def cli(side: str, go_bin: str) -> list[str]:
    return [go_bin] if side == "go" else [str(Path(sys.executable).parent / "keepsake")]


def reset(go_bin: str) -> None:
    with psycopg.connect(OWNER_DSN, autocommit=True) as conn:
        for schema, _ in SIDES.values():
            conn.execute(f"DROP SCHEMA IF EXISTS {schema} CASCADE")
    for side, (schema, _) in SIDES.items():
        sh(
            *cli(side, go_bin),
            "migrate",
            "--dsn",
            OWNER_DSN,
            env={"KEEPSAKE_SCHEMA": schema},
        )


def serve(
    side: str, go_bin: str, tenant: uuid.UUID, logs: Path, procs: list[subprocess.Popen]
) -> None:
    """Start one server, registered in `procs` for cleanup before it is waited on."""
    schema, port = SIDES[side]
    with socket.socket() as probe:
        if probe.connect_ex(("127.0.0.1", port)) == 0:
            sys.exit(f"port {port} is already taken")
    env = os.environ | {
        "KEEPSAKE_DSN": APP_DSN,
        "KEEPSAKE_TENANT_ID": str(tenant),
        "KEEPSAKE_SCHEMA": schema,
        "KEEPSAKE_ADMIN_PASSWORD": PASSWORD,
        "KEEPSAKE_UI": "true",
        "KEEPSAKE_STATIC_DIR": str(logs / "no-console"),
    }
    log = (logs / f"{side}.log").open("w")
    proc = subprocess.Popen(
        [*cli(side, go_bin), "serve", "--host", "127.0.0.1", "--port", str(port)],
        env=env,
        stdout=log,
        stderr=subprocess.STDOUT,
    )
    procs.append(proc)
    for _ in range(100):
        try:
            if httpx.get(f"http://127.0.0.1:{port}/readyz").status_code == 200:
                return
        except httpx.TransportError:
            pass
        if proc.poll() is not None:
            break
        time.sleep(0.1)
    sys.exit(f"{side} server did not come up; see {logs / f'{side}.log'}")


def api_cases(
    rng: random.Random, tenant: uuid.UUID, paths: list[str]
) -> list[tuple[str, str]]:
    """(method, url) for every /api route, with good and bad parameters."""
    t = f"tenant={tenant}"
    other = f"tenant={uuid.UUID(int=rng.getrandbits(128))}"
    cases: list[tuple[str, str]] = [
        ("GET", "/api/openapi.json"),
        ("GET", "/api/docs"),
        ("GET", "/api/tenants"),
        ("GET", "/api/graph"),
        ("GET", "/api/graph?tenant=nope"),
        ("GET", f"/api/graph?{t}"),
        ("GET", f"/api/graph?{other}"),
    ]
    for q in ["", t, other, *(f"tenant={quote(u)}" for u in BAD_UUIDS)]:
        cases += [("GET", f"/api/stats?{q}"), ("GET", f"/api/concepts?{q}")]
    cases.append(("GET", "/api/stats/timeseries"))
    for days in ["0", "1", "30", "365", "366", "x", "1.5"]:
        cases.append(("GET", f"/api/stats/timeseries?{t}&days={days}"))
    for limit in ["0", "1", "50", "200", "201", "x"]:
        cases += [
            ("GET", f"/api/concepts?limit={limit}"),
            ("GET", f"/api/activity?limit={limit}"),
        ]
    for offset in ["-1", "0", "3", "100000"]:
        cases.append(("GET", f"/api/concepts?{t}&offset={offset}&limit=7"))
    for prefix in ["detect/", "café", "日本/", "%", "_", "a b"]:
        cases.append(("GET", f"/api/concepts?{t}&prefix={quote(prefix)}"))
    cases.append(("GET", f"/api/activity?{t}"))
    for path in [*rng.sample(paths, min(10, len(paths))), "missing/x", "日本/nope"]:
        cases += [
            ("GET", f"/api/concepts/{quote(path)}?{t}"),
            ("GET", f"/api/concepts/{quote(path)}"),
            ("GET", f"/api/concepts/{quote(path)}?{other}"),
        ]
    for q in rng.sample(QUERIES, 8) + WORDS[:5]:
        cases.append(
            (
                "GET",
                f"/api/search?{t}&q={quote(q)}&limit={rng.choice([1, 20, 100])}",
            )
        )
    for bad in ["limit=0", "limit=101", "limit=x"]:
        cases += [
            ("GET", f"/api/search?{t}&q=x&{bad}"),
            ("GET", f"/api/grep?{t}&pattern=x&{bad}"),
        ]
    cases += [("GET", f"/api/search?{t}"), ("GET", "/api/search?q=x")]
    for p in PATTERNS:
        cases.append(("GET", f"/api/grep?{t}&pattern={quote(p)}"))
    cases += [("GET", f"/api/grep?{t}"), ("GET", "/api/grep?pattern=x")]
    return cases


def normalize(value: Any, start: datetime, now: datetime) -> Any:
    """Replace row timestamps, which are each server's own now(), after checking them."""
    if isinstance(value, dict):
        out = {}
        for k, v in value.items():
            if k in ("created_at", "updated_at") and isinstance(v, str):
                # Ruling (Task 11): Go renders UTC; both MUST be instants from this run.
                ts = datetime.fromisoformat(v)
                out[k] = "<now>" if ts.tzinfo and start <= ts <= now else f"<bad {v}>"
            else:
                out[k] = normalize(v, start, now)
        return out
    if isinstance(value, list):
        return [normalize(v, start, now) for v in value]
    return value


def compare_api(
    rng: random.Random, tenant: uuid.UUID, paths: list[str], start: datetime
) -> tuple[int, list[str]]:
    diffs: list[str] = []
    # One connection per request: uvicorn resets a kept-alive connection after a 500.
    clients = {
        s: httpx.Client(
            base_url=f"http://127.0.0.1:{p}", headers={"connection": "close"}
        )
        for s, (_, p) in SIDES.items()
    }

    sent = 0

    def both(method: str, url: str, **kw: Any) -> dict[str, httpx.Response]:
        nonlocal sent
        sent += 1
        return {s: c.request(method, url, **kw) for s, c in clients.items()}

    def check(
        label: str, res: dict[str, httpx.Response], parse_body: bool = True
    ) -> None:
        stats = urlparse(label.split(" ")[1]).path == "/api/stats"
        py, go = res["py"], res["go"]
        out: list[str] = []
        same(py.status_code, go.status_code, "status", out)
        if parse_body and not out:
            now = datetime.now(UTC)
            try:
                a, b = (
                    normalize(py.json(), start, now),
                    normalize(go.json(), start, now),
                )
            except ValueError:
                a, b = py.text, go.text
            for body in (a, b) if stats else ():
                if isinstance(body, dict) and isinstance(body.get("by_type"), dict):
                    # Python's GROUP BY has no ORDER BY, so its own key order varies call to call.
                    body["by_type"] = dict(sorted(body["by_type"].items()))
            same(a, b, "body", out)
        diffs.extend(f"api {label}: {d}" for d in out)

    for label, body in [
        ("bad password", {"password": "no"}),
        ("no body", None),
        ("wrong shape", {"pw": 1}),
    ]:
        check(f"POST /api/session {label}", both("POST", "/api/session", json=body))
    for ct, raw in LOGIN_BODIES:
        res = both("POST", "/api/session", content=raw, headers={"content-type": ct})
        # Divergence 10: echoing a lone surrogate or invalid UTF-8 crashes Python (500); Go answers 422.
        if raw in (LONE_SURROGATE, INVALID_UTF8):
            if (res["py"].status_code, res["go"].status_code) != (500, 422):
                diffs.append(
                    f"api login {raw!r}: {res['py'].status_code} vs {res['go'].status_code}"
                )
            continue
        check(f"POST /api/session {ct} {raw!r}", res)
    cases = api_cases(rng, tenant, paths)
    for method, url in cases:
        check(f"{method} {url} unauthenticated", both(method, url))
    login = both("POST", "/api/session", json={"password": PASSWORD})
    check("POST /api/session good", login)
    for side, res in login.items():
        clients[side].cookies = res.cookies
    for method, url in cases:
        # The Swagger page is HTML; its status and text are compared below as a whole.
        check(f"{method} {url}", both(method, url), parse_body=url != "/api/docs")
    docs = both("GET", "/api/docs")
    same(docs["py"].text, docs["go"].text, "api /api/docs body", diffs)
    # Ruling (Task 11): unmatched routes answer plain text in Go, so only the status is compared.
    for url in ["/api/nope", "/api/concepts/"]:
        check(f"GET {url}", both("GET", url), parse_body=False)
    # Ruling (Task 11): 405 on a wrong method is plain text in Go.
    check("PUT /api/tenants", both("PUT", "/api/tenants"), parse_body=False)
    check("GET /readyz", both("GET", "/readyz"))
    check("DELETE /api/session", both("DELETE", "/api/session"))
    for c in clients.values():
        c.close()
    return sent, diffs


def mcp_request(
    era: str, method: str, params: dict[str, Any] | None = None, **headers: str
) -> tuple[dict[str, str], dict[str, Any]]:
    """Headers and JSON-RPC body for `method` as a client of `era` sends it."""
    params = dict(params or {})
    h = {
        "content-type": "application/json",
        "accept": "application/json, text/event-stream",
    }
    if era == "modern":
        params["_meta"] = MODERN_META
        h |= {"mcp-protocol-version": MODERN_VERSION, "mcp-method": method}
        if method == "tools/call":
            h["mcp-name"] = params["name"]
    else:
        h["mcp-protocol-version"] = LEGACY_VERSION
    body: dict[str, Any] = {"jsonrpc": "2.0", "id": 1, "method": method}
    if params:
        body["params"] = params
    return h | {k.replace("_", "-"): v for k, v in headers.items()}, body


def compare_wire(
    py: httpx.Response,
    go: httpx.Response,
    tool: str = "",
    schema_fields: tuple[str, ...] | None = None,
) -> list[str]:
    """Status, content type and the parsed body, key order included."""
    out: list[str] = []
    same(py.status_code, go.status_code, "status", out)
    same(
        py.headers.get("content-type"),
        go.headers.get("content-type"),
        "content-type",
        out,
    )
    try:
        a, b = py.json(), go.json()
    except ValueError:
        same(py.text, go.text, "body", out)
        return out
    if schema_fields is not None:
        for body in (a, b):
            content = body.get("result", {}).get("content") or [{}]
            text = content[0].get("text", "")
            # Divergence 2: schema-error wording is the validator's; both MUST name the tool and field.
            if not text.startswith(f"{tool}: ") or not all(
                f in text for f in schema_fields
            ):
                out.append(f"schema error does not name {schema_fields}: {text!r}")
            content[0]["text"] = "<schema error>"
    for body in (a, b):
        result = body.get("result") if isinstance(body, dict) else None
        if isinstance(result, dict) and not result.get("isError"):
            for item in result.get("content", []):
                # The text is json.dumps output: parsed, so divergence 4 applies to its floats.
                item["text"] = json.loads(item["text"])
    same(a, b, "body", out)
    return out


def protocol_cases() -> list[tuple[str, dict[str, str], Any]]:
    """(label, headers, body) for the protocol surface the tool calls never reach."""
    cases: list[tuple[str, dict[str, str], Any]] = []
    for era in ("legacy", "modern"):
        cases += [
            (f"{era} tools/list", *mcp_request(era, "tools/list")),
            (f"{era} ping", *mcp_request(era, "ping")),
            (f"{era} unknown method", *mcp_request(era, "foo/bar")),
            (f"{era} bad accept", *mcp_request(era, "tools/list", accept="text/html")),
            (
                f"{era} json accept only",
                *mcp_request(era, "tools/list", accept="application/json"),
            ),
            (f"{era} parse error", mcp_request(era, "tools/list")[0], b"{"),
            (
                f"{era} batch",
                mcp_request(era, "tools/list")[0],
                [mcp_request(era, "tools/list")[1]],
            ),
        ]
    notification = {"jsonrpc": "2.0", "method": "notifications/initialized"}
    init = {
        "protocolVersion": LEGACY_VERSION,
        "capabilities": {},
        "clientInfo": {"name": "d", "version": "1"},
    }
    h, _ = mcp_request("legacy", "initialize")
    cases += [
        (
            "legacy initialize",
            {k: v for k, v in h.items() if k != "mcp-protocol-version"},
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": init},
        ),
        ("legacy notification", h, notification),
        (
            "modern notification",
            mcp_request("modern", "notifications/initialized")[0],
            notification | {"params": {"_meta": MODERN_META}},
        ),
        ("modern server/discover", *mcp_request("modern", "server/discover")),
        (
            "modern mcp-method mismatch",
            *mcp_request("modern", "tools/list", mcp_method="tools/call"),
        ),
        (
            "modern mcp-name mismatch",
            *mcp_request(
                "modern",
                "tools/call",
                {"name": "okf_list", "arguments": {}},
                mcp_name="okf_read",
            ),
        ),
        (
            "modern missing _meta",
            mcp_request("modern", "tools/list")[0],
            {"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
        ),
        (
            "unknown protocol version",
            *mcp_request("legacy", "tools/list", mcp_protocol_version="1999-01-01"),
        ),
    ]
    return cases


def compare_protocol() -> list[str]:
    out: list[str] = []
    for label, headers, body in protocol_cases():
        kw = {"content": body} if isinstance(body, bytes) else {"json": body}
        py, go = (
            httpx.post(f"http://127.0.0.1:{port}/mcp", headers=headers, **kw)
            for _, port in SIDES.values()
        )
        out += [f"mcp {label}: {d}" for d in compare_wire(py, go)]
    for method in ("GET", "DELETE"):
        # Python answers GET with an event stream that never ends, so only its head is read.
        heads = []
        for _, port in SIDES.values():
            with httpx.stream(method, f"http://127.0.0.1:{port}/mcp", timeout=5) as r:
                heads.append((r.status_code, r.headers.get("content-type")))
        same(heads[0], heads[1], f"mcp {method}", out)
    return out


class Wire:
    """An httpx client that keeps the last tools/call response the MCP client read."""

    def __init__(self) -> None:
        self.last: httpx.Response | None = None
        self.client = httpx.AsyncClient(
            event_hooks={"response": [self.keep]}, timeout=60
        )

    async def keep(self, response: httpx.Response) -> None:
        if b'"tools/call"' in response.request.content:
            await response.aread()
            self.last = response


async def drive(
    rng: random.Random, mode: str, outcomes: Counter[str]
) -> tuple[list[str], Gen]:
    """Send the seeded operations to both servers and compare each answer."""
    diffs: list[str] = []
    gen = Gen(rng)
    wires = [Wire() for _ in SIDES]
    urls = [f"http://127.0.0.1:{port}/mcp" for _, port in SIDES.values()]
    transports = [
        streamable_http_client(u, http_client=w.client)
        for u, w in zip(urls, wires, strict=True)
    ]
    async with (
        Client(transports[0], mode=mode, cache=None) as py,
        Client(transports[1], mode=mode, cache=None) as go,
    ):
        for i in range(OPS):
            tool, args, schema_fields = gen.op()
            label = f"{mode} op {i} {tool} {json.dumps(args)[:400]}"
            pr, gr = await asyncio.gather(call(py, tool, args), call(go, tool, args))
            diffs += [
                f"{label}: {d}" for d in compare_call(tool, schema_fields, pr, gr)
            ]
            # The client's models hide wire key order and envelope fields.
            if wires[0].last and wires[1].last:
                wire = compare_wire(wires[0].last, wires[1].last, tool, schema_fields)
                diffs += [f"{label} wire: {d}" for d in wire]
            outcomes[f"{tool}:{'error' if pr.get('isError', True) else 'ok'}"] += 1
            if pr.get("isError") is False:
                gen.learn(pr["structured"])
                structured = pr["structured"]
                if isinstance(structured, dict) and structured.get("conflict"):
                    outcomes["okf_update:conflict"] += 1
    return diffs, gen


def export_diffs(go_bin: str, tenant: uuid.UUID, work: Path) -> list[str]:
    """Export both schemas with both CLIs; all four bundles MUST be identical."""
    bundles = []
    for exporter in SIDES:
        for side, (schema, _) in SIDES.items():
            out = work / f"export-{exporter}-cli-{side}-db"
            argv = ["export", "--dsn", APP_DSN, "--tenant", str(tenant), str(out)]
            sh(*cli(exporter, go_bin), *argv, env={"KEEPSAKE_SCHEMA": schema})
            bundles.append(out)
    diffs = []
    for other in bundles[1:]:
        argv = ["diff", "-r", str(bundles[0]), str(other)]
        done = subprocess.run(argv, capture_output=True, text=True, check=False)
        if done.returncode != 0:
            diffs.append(
                f"export {bundles[0].name} vs {other.name}:\n{done.stdout[:2000]}"
            )
    return diffs


def run(seed: int, go_bin: str, work: Path) -> int:
    """One pass per protocol era, each from an empty database and the same seed."""
    diffs: list[str] = []
    for mode in ("legacy", MODERN_VERSION):
        rng = random.Random(seed)
        tenant = uuid.UUID(int=rng.getrandbits(128))
        reset(go_bin)
        start = datetime.now(UTC)
        procs: list[subprocess.Popen] = []
        outcomes: Counter[str] = Counter()
        try:
            for side in SIDES:
                serve(side, go_bin, tenant, work, procs)
            era, gen = asyncio.run(drive(rng, mode, outcomes))
            era += compare_protocol()
            routes, api_diffs = compare_api(rng, tenant, sorted(gen.versions), start)
            era += api_diffs
        finally:
            for p in procs:
                p.terminate()
                p.wait()
        era += export_diffs(go_bin, tenant, work / mode)
        for d in era[:300]:
            print(d)
        if len(era) > 300:
            print(f"... {len(era) - 300} more")
        print(
            f"seed {seed} {mode}: {OPS} ops, {routes} api requests, 4 exports "
            f"({len(gen.versions)} concepts), {len(era)} differences"
        )
        kinds = Counter(
            "wire"
            if " wire: " in d
            else d.split(" ")[0]
            if not d.startswith(mode)
            else "client"
            for d in era
        )
        print("  by kind:", " ".join(f"{k}={v}" for k, v in sorted(kinds.items())))
        print("  outcomes:", " ".join(f"{k}={v}" for k, v in sorted(outcomes.items())))
        diffs += era
    return 1 if diffs else 0


def main() -> int:
    seed = int(sys.argv[1])
    with tempfile.TemporaryDirectory(prefix="okf-diff-") as tmp:
        work = Path(tmp)
        go_bin = str(work / "keepsake-go")
        sh("go", "build", "-C", str(REPO), "-o", go_bin, "./cmd/keepsake")
        return run(seed, go_bin, work)


if __name__ == "__main__":
    sys.exit(main())
