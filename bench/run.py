"""Benchmark how Claude uses keepsake's MCP tools, across prompt and tool-description variants.

Every trial gets a fresh tenant loaded from bench/bundle, its own server, and a proxy
that rewrites `tools/list` to the variant's descriptions. `claude -p` runs with no
user settings, no built-in tools and no other MCP servers.

    python3 bench/run.py --trials 3 --jobs 4
    python3 bench/run.py --variants main baseline --trials 5   # main's server against this tree's
    python3 bench/run.py --report bench/results/<run>
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import uuid
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.error import HTTPError

sys.path.insert(0, str(Path(__file__).resolve().parent))
import grade

ROOT = Path(__file__).resolve().parent.parent
BENCH = ROOT / "bench"
# tasks.json's answers hold for this copy of demo/bundle, which the demo is free to change.
BUNDLE = BENCH / "bundle"

# The same isolation for the agent and the judge: nothing from this machine's Claude setup leaks in.
ISOLATED = [
    "--setting-sources",
    "",
    "--strict-mcp-config",
    "--tools",
    "",
    "--disable-slash-commands",
    "--no-session-persistence",
]


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def sh(*cmd: str, **kw: Any) -> str:
    kw.setdefault("stderr", subprocess.PIPE)
    return subprocess.run(
        cmd, check=True, stdout=subprocess.PIPE, text=True, **kw
    ).stdout


def resolve_ref(ref: str) -> str:
    return sh("git", "rev-parse", "--verify", f"{ref}^{{commit}}", cwd=ROOT).strip()


def build(sha: str | None, dest: Path) -> Path:
    """Builds keepsake at sha, or the working tree for None, without touching the working tree."""
    if sha is None:
        sh("go", "build", "-o", str(dest), "./cmd/keepsake", cwd=ROOT)
        return dest
    with tempfile.TemporaryDirectory() as src:
        archive = subprocess.Popen(
            ["git", "archive", sha], cwd=ROOT, stdout=subprocess.PIPE
        )
        subprocess.run(["tar", "-x", "-C", src], stdin=archive.stdout, check=True)
        if archive.wait() != 0:
            raise RuntimeError(f"git archive {sha} failed")
        sh("go", "build", "-o", str(dest), "./cmd/keepsake", cwd=src)
    return dest


class Postgres:
    """A throwaway postgres:17 with the owner and app roles the chart creates."""

    def __init__(self) -> None:
        self.port = free_port()
        self.name = f"keepsake-bench-{self.port}"
        sh(
            "docker",
            "run",
            "--rm",
            "-d",
            "--name",
            self.name,
            "-e",
            "POSTGRES_PASSWORD=pg",
            "-p",
            f"127.0.0.1:{self.port}:5432",
            "postgres:17",
            "-c",
            "max_connections=1000",
        )
        try:
            # The image restarts once after init, so the second "ready" is the real one. It logs to stderr.
            deadline = time.monotonic() + 60
            while (
                sh("docker", "logs", self.name, stderr=subprocess.STDOUT).count(
                    "ready to accept connections"
                )
                < 2
            ):
                if time.monotonic() > deadline:
                    raise TimeoutError("postgres did not start")
                time.sleep(0.5)
            for sql in (
                "CREATE ROLE okf_owner LOGIN PASSWORD 'owner'",
                "CREATE ROLE okf_app LOGIN PASSWORD 'app'",
                "CREATE DATABASE bench OWNER okf_owner",
            ):
                sh("docker", "exec", self.name, "psql", "-U", "postgres", "-c", sql)
        except BaseException:
            self.close()
            raise

    def dsn(self, user: str, password: str, db: str = "bench") -> str:
        return f"postgresql://{user}:{password}@127.0.0.1:{self.port}/{db}?sslmode=disable"

    def database(self, name: str) -> None:
        sh(
            "docker", "exec", self.name, "psql", "-U", "postgres", "-c",
            f"CREATE DATABASE {name} OWNER okf_owner",
        )

    def close(self) -> None:
        subprocess.run(["docker", "stop", self.name], capture_output=True, check=False)


class Proxy(ThreadingHTTPServer):
    """Forwards /mcp, rewriting it to a variant's. In `tools` and in `instructions`, a string
    replaces the server's text and null drops it."""

    def __init__(self, upstream: str, variant: dict[str, Any]) -> None:
        self.upstream, self.overrides = upstream, variant.get("tools", {})
        self.instructions = variant.get("instructions", ...)
        super().__init__(("127.0.0.1", 0), _Forward)
        threading.Thread(target=self.serve_forever, daemon=True).start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.server_address[1]}/mcp"

    def rewrite(self, request: bytes, body: bytes) -> bytes:
        try:
            request_json, reply = json.loads(request), json.loads(body)
        except ValueError:
            return body
        method = request_json.get("method") if isinstance(request_json, dict) else None
        if "result" not in reply:
            return body
        if method in ("initialize", "server/discover"):
            if self.instructions is ...:
                return body
            reply["result"].pop("instructions", None)
            if self.instructions is not None:
                reply["result"]["instructions"] = self.instructions
            return json.dumps(reply).encode()
        if method != "tools/list":
            return body
        tools = []
        for tool in reply["result"]["tools"]:
            if tool["name"] not in self.overrides:
                tools.append(tool)
            elif self.overrides[tool["name"]] is not None:
                tools.append({**tool, "description": self.overrides[tool["name"]]})
        reply["result"]["tools"] = tools
        return json.dumps(reply).encode()


class _Forward(BaseHTTPRequestHandler):
    server: Proxy

    def _forward(self) -> None:
        request = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        headers = {
            k: v
            for k, v in self.headers.items()
            if k.lower() not in ("host", "content-length", "connection")
        }
        req = urllib.request.Request(
            self.server.upstream,
            data=request or None,
            method=self.command,
            headers=headers,
        )
        try:
            with urllib.request.urlopen(req) as resp:
                status, reply_headers, body = resp.status, resp.headers, resp.read()
        except HTTPError as e:
            status, reply_headers, body = e.code, e.headers, e.read()
        if status == 200 and "json" in (reply_headers.get("Content-Type") or ""):
            body = self.server.rewrite(request, body)
        self.send_response(status)
        for k, v in reply_headers.items():
            if k.lower() not in ("content-length", "transfer-encoding", "connection"):
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_POST = do_GET = do_DELETE = _forward

    def log_message(self, *args: Any) -> None:
        pass


def wait_ready(url: str, server: subprocess.Popen[bytes]) -> None:
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if server.poll() is not None:
            raise RuntimeError(f"keepsake serve exited with {server.returncode}")
        try:
            with urllib.request.urlopen(url) as resp:
                if resp.status == 200:
                    return
        except OSError:
            pass  # Not listening yet: poll again.
        time.sleep(0.2)
    raise TimeoutError(f"{url} never became ready")


def judged(model: str, task: dict[str, Any], answer: str, cwd: Path) -> bool | None:
    """A judge model's verdict on the answer, or None when the task names no judge."""
    expect = task["expect"]
    if "judge_template" in expect:
        # LongMemEval's own prompt and parsing: the verdict is any "yes" in the reply.
        reply = judge(
            model, expect["judge_template"].replace(grade.RESPONSE, answer), cwd
        )
        return "yes" in reply.lower()
    if "judge" in expect:
        prompt = (
            "Grade an answer against a rubric. Reply with exactly PASS or FAIL.\n\n"
            f"Question: {task['prompt']}\n\nRubric: {expect['judge']}\n\n<answer>\n{answer}\n</answer>"
        )
        return judge(model, prompt, cwd).strip().upper().startswith("PASS")
    return None


def judge(model: str, prompt: str, cwd: Path) -> str:
    out = sh(
        "claude",
        "-p",
        prompt,
        "--model",
        model,
        "--output-format",
        "json",
        *ISOLATED,
        cwd=cwd,
        timeout=120,
    )
    return json.loads(out).get("result", "")


def trial(
    args: argparse.Namespace,
    pg: Postgres,
    build_: tuple[Path, str],
    task: dict[str, Any],
    variant: dict[str, Any],
    n: int,
    out: Path,
) -> dict[str, Any]:
    binary, db = build_
    app = pg.dsn("okf_app", "app", db)
    tenant = str(uuid.uuid4())
    row: dict[str, Any] = {
        "variant": variant["name"],
        "task": task["id"],
        "kind": task["kind"],
        "trial": n,
        "tenant": tenant,
    }
    with tempfile.TemporaryDirectory() as tmp:
        work = Path(tmp)
        bundle = Path(task.get("bundle_path", BUNDLE))
        sh(str(binary), "import", "--dsn", app, "--tenant", tenant, str(bundle))
        port = free_port()
        log = (out / f"{variant['name']}.{task['id']}.{n}.server.log").open("wb")
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
            stderr=log,
            env={**os.environ, "KEEPSAKE_UI": "false"},
        )
        proxy = None
        try:
            wait_ready(f"http://127.0.0.1:{port}/readyz", server)
            proxy = Proxy(f"http://127.0.0.1:{port}/mcp", variant)
            config = work / "mcp.json"
            config.write_text(
                json.dumps(
                    {"mcpServers": {"keepsake": {"type": "http", "url": proxy.url}}}
                )
            )
            cmd = [
                "claude",
                "-p",
                task["prompt"],
                "--model",
                args.model,
                *ISOLATED,
                "--mcp-config",
                str(config),
                "--allowedTools",
                "mcp__keepsake",
                "--output-format",
                "stream-json",
                "--verbose",
                "--max-budget-usd",
                str(args.budget),
            ]
            if task.get("system_prompt"):
                # Replaces Claude Code's own prompt, and its coding-assistant role; MCP instructions survive it.
                cmd += ["--system-prompt", task["system_prompt"]]
            prompt = variant.get("system_prompt") or ""
            if variant.get("bundle_in_prompt"):
                prompt += "\n\nThe team's knowledge base, in full:\n\n" + "\n\n".join(
                    f"# {path}.md\n{text}"
                    for path, text in sorted(grade.load_bundle(bundle).items())
                )
            if prompt:
                # A file, since Linux caps one argument at 128 KiB and a LongMemEval bundle is 0.5 MB.
                (work / "prompt.md").write_text(prompt.strip())
                cmd += ["--append-system-prompt-file", str(work / "prompt.md")]
            transcript = out / f"{variant['name']}.{task['id']}.{n}.jsonl"
            with transcript.open("w") as f:
                agent = subprocess.Popen(
                    cmd, stdout=f, stderr=subprocess.STDOUT, cwd=work
                )
                try:
                    agent.wait(timeout=args.timeout)
                except subprocess.TimeoutExpired:
                    agent.kill()
                    agent.wait()
                    row["error"] = f"timed out after {args.timeout}s"
            exported = work / "export"
            sh(str(binary), "export", "--dsn", app, "--tenant", tenant, str(exported))
            after = grade.load_bundle(exported)
            # Kept, so a memory an agent wrote can seed a later task (bench/longmemeval.py --curated).
            kept = out / "exports" / f"{variant['name']}.{task['id']}.{n}"
            shutil.copytree(exported, kept)
            row["export"] = str(kept)
        finally:
            if proxy:
                proxy.shutdown()
            server.terminate()
            server.wait()
            log.close()

        calls, result = grade.parse(transcript.read_text().splitlines())
        answer = result.get("result") or ""
        if not result:
            row.setdefault("error", "no result message; see the transcript")
        elif result.get("is_error"):
            row.setdefault("error", f"claude ended with {result.get('subtype')}")
        try:
            verdict = judged(args.judge_model, task, answer, work) if answer else None
        except (subprocess.SubprocessError, ValueError) as e:
            # The judge's fault fails the trial but keeps the agent's answer and metrics.
            verdict = None
            row.setdefault("error", f"judge: {type(e).__name__}")
        row["checks"] = grade.grade(
            task["expect"],
            calls,
            answer,
            grade.load_bundle(bundle),
            after,
            verdict,
            require_tools=not variant.get("bundle_in_prompt"),
        )
        row["passed"] = "error" not in row and all(row["checks"].values())
        row.update(grade.metrics(calls, result, task["expect"].get("reads", [])))
        row["answer"] = answer
    return row


def load(directory: Path, names: list[str] | None) -> list[dict[str, Any]]:
    found = {
        p.stem: {"name": p.stem, **json.loads(p.read_text())}
        for p in sorted(directory.glob("*.json"))
    }
    unknown = set(names or []) - set(found)
    if unknown:
        sys.exit(f"unknown: {', '.join(sorted(unknown))}; have {', '.join(found)}")
    return [found[n] for n in names] if names else list(found.values())


def main() -> None:
    p = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    p.add_argument(
        "--variants", nargs="*", help="Names in bench/variants/. Default: all."
    )
    p.add_argument(
        "--tasks-file",
        type=Path,
        default=BENCH / "tasks.json",
        help="Default: bench/tasks.json. bench/longmemeval.py writes others.",
    )
    p.add_argument(
        "--tasks", nargs="*", help="Task ids in the tasks file. Default: all."
    )
    p.add_argument(
        "--trials", type=int, default=3, help="Runs per task and variant. Default: 3."
    )
    p.add_argument(
        "--jobs", type=int, default=4, help="Trials in parallel. Default: 4."
    )
    p.add_argument(
        "--model",
        default="claude-sonnet-5",
        help="The agent's model. Default: claude-sonnet-5.",
    )
    p.add_argument(
        "--judge-model",
        default="claude-haiku-4-5-20251001",
        help="Grades rubric tasks.",
    )
    p.add_argument(
        "--timeout", type=int, default=300, help="Seconds per agent run. Default: 300."
    )
    p.add_argument(
        "--budget", type=float, default=1.0, help="USD cap per agent run. Default: 1.0."
    )
    p.add_argument(
        "--report",
        type=Path,
        help="Re-print the report for an earlier run directory, and exit.",
    )
    args = p.parse_args()

    if args.report:
        rows = [
            json.loads(line)
            for line in (args.report / "results.jsonl").read_text().splitlines()
        ]
        print(grade.summarize(rows))
        return

    variants = load(BENCH / "variants", args.variants)
    tasks_by_id = {t["id"]: t for t in json.loads(args.tasks_file.read_text())}
    for t in tasks_by_id.values():
        # A task's own bundle is relative to its tasks file; without one, it runs on the demo.
        t["bundle_path"] = str(
            (args.tasks_file.parent / t["bundle"]).resolve()
            if "bundle" in t
            else BUNDLE
        )
    unknown = set(args.tasks or []) - set(tasks_by_id)
    if unknown:
        sys.exit(f"unknown task: {', '.join(sorted(unknown))}")
    tasks = (
        [tasks_by_id[t] for t in args.tasks]
        if args.tasks
        else list(tasks_by_id.values())
    )
    for tool in ("claude", "docker", "go"):
        if not shutil.which(tool):
            sys.exit(f"{tool} not found on PATH")

    out = BENCH / "results" / time.strftime("%Y%m%d-%H%M%S")
    out.mkdir(parents=True)
    (out / "run.json").write_text(
        json.dumps(
            {
                **vars(args),
                "report": None,
                "tasks_file": str(args.tasks_file),
                "variants": variants,
                "tasks": tasks,
            },
            indent=2,
        )
    )
    shas = {v["name"]: resolve_ref(v["ref"]) if v.get("ref") else None for v in variants}
    run_json = json.loads((out / "run.json").read_text())
    run_json["builds"] = {n: s or "working tree" for n, s in shas.items()}
    (out / "run.json").write_text(json.dumps(run_json, indent=2))
    binaries = {
        s: build(s, out / f"keepsake-{(s or 'tree')[:12]}") for s in set(shas.values())
    }

    pg = Postgres()
    try:
        # One database per build, so two refs may carry different migrations.
        builds: dict[str | None, tuple[Path, str]] = {}
        for i, (s, binary) in enumerate(binaries.items()):
            db = "bench" if i == 0 else f"bench_{i}"
            if i:
                pg.database(db)
            sh(str(binary), "migrate", "--dsn", pg.dsn("okf_owner", "owner", db))
            builds[s] = (binary, db)
        jobs = [
            (t, v, n)
            for v in variants
            for t in tasks
            for n in range(1, args.trials + 1)
        ]
        rows: list[dict[str, Any]] = []
        lock = threading.Lock()

        def one(job: tuple[dict[str, Any], dict[str, Any], int]) -> None:
            t, v, n = job
            try:
                row = trial(args, pg, builds[shas[v["name"]]], t, v, n, out)
            except Exception as e:  # noqa: BLE001 - a harness fault fails the trial, not the run.
                row = {
                    "variant": v["name"],
                    "task": t["id"],
                    "kind": t["kind"],
                    "trial": n,
                    "passed": False,
                    "checks": {},
                    "error": f"harness: {e}",
                    **grade.metrics([], {}),
                }
            with lock:
                rows.append(row)
                with (out / "results.jsonl").open("a") as f:
                    f.write(json.dumps(row) + "\n")
                status = "PASS" if row["passed"] else "FAIL"
                print(
                    f"[{len(rows)}/{len(jobs)}] {v['name']} {t['id']} #{n} {status} "
                    f"{row['calls']} calls ${row['cost_usd']:.3f} {row['duration_s']:.0f}s",
                    flush=True,
                )

        with ThreadPoolExecutor(max_workers=args.jobs) as pool:
            list(pool.map(one, jobs))
    finally:
        pg.close()

    report = grade.summarize(rows)
    (out / "report.md").write_text(report + "\n")
    print(f"\n{report}\n\nTranscripts and results: {out}")


if __name__ == "__main__":
    main()
