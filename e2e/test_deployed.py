"""What only a real deployment proves: the owner migrates the schema before the
server starts, /mcp answers over the network, and an app role that is exempt from
row-level security crash-loops the pod instead of serving cross-tenant reads.

`run.sh` installs the release these read; running them by hand needs that cluster.
"""

from __future__ import annotations

import base64
import json
import subprocess
import time
import urllib.request
from collections.abc import Callable, Sequence
from contextlib import suppress
from pathlib import Path
from typing import Any
from urllib.error import HTTPError

import pytest

from keepsake.cli import head

BASE = "http://localhost:30800"
CHART = str(Path(__file__).resolve().parent.parent / "charts" / "keepsake")

# Named on every command rather than taken from the ambient one. These tests install a
# Helm release, and a machine that runs this suite is likely to have other clusters.
CONTEXT = "kind-keepsake-e2e"
HEAD = head()

TOOL_NAMES = {
    "okf_list",
    "okf_search",
    "okf_grep",
    "okf_read",
    "okf_create",
    "okf_update",
    "okf_relate",
}

# The release `run.sh` installs. Named rather than left implicit: the upgrade test
# below reinstalls it, and the secret/login tests read its Secret by name.
RELEASE = "keepsake"

# The install args `run.sh` used, replayed verbatim by the upgrade test. Same values
# in means any password change can only come from the `lookup` guard misfiring, not
# from a config drift the test introduced itself.
_INSTALL_ARGS = [
    "--set",
    "image.repository=keepsake",
    "--set",
    "image.tag=e2e",
    "--set",
    "postgres.mode=existing",
    "--set",
    "postgres.dsn=postgres://okf_app:app@postgres:5432/keepsake",
    "--set",
    "postgres.ownerDsn=postgres://okf_owner:owner@postgres:5432/keepsake",
    "--set",
    "service.type=NodePort",
    "--set",
    "service.nodePort=30800",
]

# The second release, installed by the crash-loop test alone: it connects as the
# container's superuser, which is exactly what `verify` exists to refuse. Its own
# schema, so it cannot touch the schema the good release serves from.
BAD = "keepsake-bad"
BAD_SCHEMA = "okf_bad"
# Named for both roles, deliberately the same superuser: the chart requires an owner
# DSN in existing mode, and giving it the app's is what makes the server privileged.
BAD_DSN = "postgres://postgres:postgres@postgres:5432/keepsake"


def _run(args: Sequence[str], timeout: float = 300, check: bool = True) -> str:
    """Run a command, or fail the test with everything it printed."""
    result = subprocess.run(
        list(args), capture_output=True, text=True, timeout=timeout, check=False
    )
    assert not check or result.returncode == 0, (
        f"{' '.join(args)} exited {result.returncode}\n"
        f"stdout: {result.stdout}\nstderr: {result.stderr}"
    )
    return result.stdout


def _kubectl(*args: str, timeout: float = 300, check: bool = True) -> str:
    return _run(["kubectl", "--context", CONTEXT, *args], timeout=timeout, check=check)


def _logs(pod: str) -> str:
    """Unchecked: `kubectl logs` errors outright while a container is still starting,
    and the caller is polling for the crash that follows."""
    return _kubectl("logs", pod, "--tail=20", timeout=60, check=False)


def _psql(statement: str) -> str:
    """One value, read as the container's superuser. Setup and assertions only."""
    return _kubectl(
        "exec",
        "deploy/postgres",
        "--",
        "psql",
        "-U",
        "postgres",
        "-d",
        "keepsake",
        "-tAc",
        statement,
    ).strip()


def _rpc(method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
    payload = json.dumps(
        {"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}
    ).encode()
    request = urllib.request.Request(
        f"{BASE}/mcp",
        data=payload,
        headers={"Content-Type": "application/json", "Accept": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        answer: dict[str, Any] = json.loads(response.read())
    assert "error" not in answer, f"{method}: {answer['error']}"
    return answer


def _call(name: str, arguments: dict[str, Any]) -> Any:
    """Call a tool and hand back its structured content, or fail with its error."""
    result = _rpc("tools/call", {"name": name, "arguments": arguments})["result"]
    assert not result.get("isError"), f"{name}: {result['content'][0]['text']}"
    return result["structuredContent"]


def _advertised() -> dict[str, dict[str, Any]]:
    return {t["name"]: t for t in _rpc("tools/list")["result"]["tools"]}


def _admin_password() -> str:
    """Read the generated admin password the way an operator would."""
    encoded = _kubectl(
        "get", "secret", f"{RELEASE}-admin", "-o", "jsonpath={.data.password}"
    )
    return base64.b64decode(encoded).decode()


def _login(password: str) -> str:
    """POST /api/session and hand back the cookie header to replay on later requests."""
    request = urllib.request.Request(
        f"{BASE}/api/session",
        data=json.dumps({"password": password}).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=30) as response:
        set_cookie = response.headers["Set-Cookie"]
    assert set_cookie, "login did not set a session cookie"
    return set_cookie.split(";", 1)[0]


def _api_get(path: str, cookie: str | None = None) -> tuple[int, Any]:
    """GET a console API route. Returns the status so callers can assert a 401
    without an exception handler of their own."""
    headers = {"Cookie": cookie} if cookie else {}
    request = urllib.request.Request(f"{BASE}{path}", headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.status, json.loads(response.read())
    except HTTPError as exc:
        return exc.code, None


def _until[T](
    read: Callable[[], T],
    what: str,
    timeout: float = 120,
    catch: tuple[type[BaseException], ...] = (),
) -> T:
    """Poll until `read` answers with something, then return it. Anything in `catch`
    counts as not yet."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if seen := read():
                return seen
        except catch:
            pass
        time.sleep(2)
    raise AssertionError(f"{what} was still empty after {timeout:.0f}s")


@pytest.fixture(scope="session", autouse=True)
def _wait_for_the_node_port() -> None:
    """Helm reports the pods ready before kube-proxy has finished programming the
    NodePort, and a request that arrives in between is reset. Waits for the datapath
    and never fails: a test that still cannot reach the endpoint says so itself.
    """
    with suppress(AssertionError):
        _until(lambda: _rpc("tools/list"), "the NodePort", timeout=60, catch=(OSError,))


def test_the_owner_migrated_the_schema() -> None:
    """Helm deletes the migration Job once it succeeds, so the database is the only
    place its outcome stays legible. The owner ran it, not the app role: a schema the
    app role owns is one the server refuses to serve against, and dropping it is the
    only way back."""
    assert _psql("SELECT version_num FROM okf.alembic_version") == HEAD
    owner = _psql("SELECT nspowner::regrole FROM pg_namespace WHERE nspname = 'okf'")
    assert owner == "okf_owner"


def test_every_server_pod_is_ready() -> None:
    """Ready means `verify` passed and the port bound, which means the migration had
    already finished: the server refuses to start against an unmigrated database."""
    ready = _kubectl(
        "get",
        "pods",
        "-l",
        "app=keepsake",
        "-o",
        r'jsonpath={.items[*].status.conditions[?(@.type=="Ready")].status}',
    ).split()
    assert ready, "no pod carries the release's label"
    assert set(ready) == {"True"}, f"pod readiness: {ready}"


def test_mcp_endpoint_advertises_the_tool_surface() -> None:
    assert set(_advertised()) == TOOL_NAMES


def test_search_and_grep_require_a_limit() -> None:
    tools = _advertised()
    for name in ("okf_search", "okf_grep"):
        assert "limit" in tools[name]["inputSchema"]["required"]


def test_a_concept_round_trips_through_the_deployed_server() -> None:
    _call(
        "okf_create",
        {
            "path": "e2e/smoke",
            "type": "Concept",
            "title": "Smoke",
            "description": "",
            "body": "deployed",
        },
    )
    hits = _call("okf_search", {"query": "smoke", "limit": 5})
    assert any(h["path"] == "e2e/smoke" for h in hits["results"])


def test_console_serves_the_built_image_bundle() -> None:
    """GET / must come from the image's own /app/static, built by the Dockerfile's
    `bun` stage, not a bundle `bun run build` produced on the test machine. A
    dev-source reference (`/src/main.tsx`) here would mean the image shipped with
    no bundle at all -- Starlette's SPA fallback still 200s an empty static dir's
    absent index.html would not, so this also rules that out."""
    with urllib.request.urlopen(f"{BASE}/", timeout=30) as response:
        assert response.status == 200
        assert "text/html" in response.headers["Content-Type"]
        body = response.read().decode()
    assert 'src="/assets/' in body, f"no Vite-built entry script in: {body!r}"


def test_console_api_rejects_an_unauthenticated_request() -> None:
    """A deployed instance's API must not be open. `require_session` is a router-level
    dependency, but nothing short of a real HTTP request over the network proves it
    actually guards the mounted route."""
    status, _ = _api_get("/api/stats")
    assert status == 401


def test_the_generated_password_logs_in_and_reads_the_seeded_totals() -> None:
    """First use, ever, of a chart-generated password: every earlier test picked its
    own. Reads it the way an operator would (`kubectl get secret ... | base64 -d`),
    logs in, and confirms the session it grants sees real data -- not just a 200."""
    cookie = _login(_admin_password())
    status, before = _api_get("/api/stats", cookie)
    assert status == 200

    _call(
        "okf_create",
        {
            "path": "e2e/console-login",
            "type": "Concept",
            "title": "Console login proof",
            "description": "",
            "body": "seeded by the deployed-image e2e",
        },
    )

    status, after = _api_get("/api/stats", cookie)
    assert status == 200
    assert after["concepts"] == before["concepts"] + 1


def test_helm_upgrade_leaves_the_admin_password_unchanged() -> None:
    """Closes a debt from Task 1: the `lookup` guard in admin-secret.yaml is what
    makes the password survive an upgrade, and it is the single most likely thing
    about that template to regress silently. A broken guard would rotate the
    password on every sync and lock the operator out of a running install.

    This runs a real `helm upgrade` against the live release -- not a second read of
    the same Secret -- and then logs in with the *old* password against the
    *upgraded* release, so a guard that silently rotated the value would fail the
    login rather than just an equality check against a stale local variable.
    """
    before = _admin_password()

    _run(
        [
            "helm",
            "upgrade",
            "--install",
            RELEASE,
            CHART,
            "--kube-context",
            CONTEXT,
            *_INSTALL_ARGS,
            "--wait",
            "--timeout",
            "180s",
        ]
    )

    after = _admin_password()
    assert after == before

    cookie = _login(before)
    status, _ = _api_get("/api/stats", cookie)
    assert status == 200


def test_a_privileged_app_role_crash_loops_the_pod() -> None:
    """The isolation story rests on `verify` firing in a real deployment, not on a
    unit test calling it directly."""
    _run(
        [
            "helm",
            "upgrade",
            "--install",
            BAD,
            CHART,
            "--kube-context",
            CONTEXT,
            "--set",
            "image.repository=keepsake",
            "--set",
            "image.tag=e2e",
            "--set",
            "postgres.mode=existing",
            "--set",
            f"postgres.dsn={BAD_DSN}",
            "--set",
            f"postgres.ownerDsn={BAD_DSN}",
            "--set",
            f"postgres.schema={BAD_SCHEMA}",
            # One pod to read logs from, and no claim on the good release's nodePort.
            "--set",
            "replicaCount=1",
            "--set",
            "service.type=ClusterIP",
            # Shorter than the subprocess timeout, so a hook that hangs reports as
            # helm's own error rather than as a killed command.
            "--timeout",
            "120s",
        ]
    )
    # Helm waits on its hooks, so the migration has run by now. Asserted because a
    # hook that failed would leave no pod at all, and this test would then be red for
    # a reason that has nothing to do with `verify`.
    assert _psql(f"SELECT version_num FROM {BAD_SCHEMA}.alembic_version") == HEAD

    # Scoped to the release: the good release's pods are healthy and would say nothing.
    pod = _until(
        lambda: _kubectl(
            # [*], not [0]: an index into an empty list is an error, not an empty answer.
            "get",
            "pods",
            "-l",
            f"app={BAD}",
            "-o",
            "jsonpath={.items[*].metadata.name}",
        ),
        f"the {BAD} pod",
    ).strip()
    logs = _until(lambda: _logs(pod), f"{pod} logs")
    assert "must not connect as a superuser" in logs
