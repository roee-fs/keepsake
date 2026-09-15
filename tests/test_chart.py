"""What the chart renders, read as objects rather than as text.

The load-bearing property is not that `okf_owner` appears somewhere in the output:
it is that the migration Job takes the owner DSN and the server takes the app DSN.
A substring assertion passes on a comment, so every check here is pinned to the
container, annotation or field it belongs to.
"""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Any

import pytest
from ruamel.yaml import YAML

CHART = Path(__file__).resolve().parent.parent / "charts" / "keepsake"

pytestmark = pytest.mark.skipif(
    shutil.which("helm") is None, reason="helm is not installed"
)

MANAGED = {"postgres.mode": "managed"}
EXISTING = {
    "postgres.mode": "existing",
    "postgres.dsn": "postgres://app@db/keepsake",
    "postgres.ownerDsn": "postgres://owner@db/keepsake",
}


def _render(values: dict[str, str], release: str = "keepsake") -> list[dict[str, Any]]:
    """Every manifest the chart produces, hooks included."""
    args = ["helm", "template", release, str(CHART)]
    for key, value in values.items():
        args += ["--set", f"{key}={value}"]
    result = subprocess.run(args, capture_output=True, text=True, check=True)
    return [doc for doc in YAML(typ="safe").load_all(result.stdout) if doc]


def _only(docs: list[dict[str, Any]], kind: str) -> dict[str, Any]:
    matches = [d for d in docs if d["kind"] == kind]
    assert len(matches) == 1, f"expected one {kind}, got {len(matches)}"
    return matches[0]


def _named(docs: list[dict[str, Any]], kind: str, name: str) -> dict[str, Any]:
    matches = [d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name]
    assert len(matches) == 1, f"expected one {kind}/{name}, got {len(matches)}"
    return matches[0]


def _container(workload: dict[str, Any]) -> dict[str, Any]:
    containers = workload["spec"]["template"]["spec"]["containers"]
    assert len(containers) == 1
    return containers[0]


def _env(workload: dict[str, Any]) -> dict[str, Any]:
    return {e["name"]: e for e in _container(workload)["env"]}


def _dsn_secret_key(workload: dict[str, Any], release: str = "keepsake") -> str:
    """The Secret key the workload's KEEPSAKE_DSN resolves to."""
    ref = _env(workload)["KEEPSAKE_DSN"]["valueFrom"]["secretKeyRef"]
    assert ref["name"] == f"{release}-dsn"
    return str(ref["key"])


def test_managed_mode_renders_a_cnpg_cluster() -> None:
    cluster = _only(_render(MANAGED), "Cluster")
    assert cluster["apiVersion"] == "postgresql.cnpg.io/v1"


def test_existing_mode_renders_no_cluster() -> None:
    assert [d for d in _render(EXISTING) if d["kind"] == "Cluster"] == []


@pytest.mark.parametrize("omitted", ["postgres.dsn", "postgres.ownerDsn"])
def test_existing_mode_without_both_dsns_is_rejected_by_the_schema(
    omitted: str,
) -> None:
    """Omitting ownerDsn ran the migration as the app role, which owns the schema it
    creates and which `verify` then refuses to serve as, unrecoverably."""
    args = ["helm", "template", "keepsake", str(CHART)]
    for key, value in EXISTING.items():
        if key != omitted:
            args += ["--set", f"{key}={value}"]
    result = subprocess.run(args, capture_output=True, text=True, check=False)
    assert result.returncode != 0
    assert omitted.removeprefix("postgres.") in result.stderr


def test_migration_job_is_a_pre_install_hook() -> None:
    job = _only(_render(MANAGED), "Job")
    assert job["metadata"]["annotations"]["helm.sh/hook"] == "pre-install,pre-upgrade"


def test_the_database_is_created_before_the_migration_runs() -> None:
    docs = _render(MANAGED)
    cluster = _only(docs, "Cluster")["metadata"]["annotations"]
    job = _only(docs, "Job")["metadata"]["annotations"]
    assert "pre-install" in cluster["helm.sh/hook"]
    assert int(cluster["helm.sh/hook-weight"]) < int(job["helm.sh/hook-weight"])


def test_secrets_are_installed_before_the_hooks_that_read_them() -> None:
    docs = _render(MANAGED)
    weight = int(_only(docs, "Job")["metadata"]["annotations"]["helm.sh/hook-weight"])
    for secret in (d for d in docs if d["kind"] == "Secret"):
        annotations = secret["metadata"]["annotations"]
        assert "pre-install" in annotations["helm.sh/hook"]
        assert int(annotations["helm.sh/hook-weight"]) < weight


def test_server_and_migration_use_different_roles() -> None:
    docs = _render(MANAGED)
    assert _dsn_secret_key(_only(docs, "Job")) == "owner-dsn"
    assert _dsn_secret_key(_only(docs, "Deployment")) == "app-dsn"
    dsns = _named(docs, "Secret", "keepsake-dsn")["stringData"]
    assert "//okf_owner:" in dsns["owner-dsn"]
    assert "//okf_app:" in dsns["app-dsn"]


def test_managed_mode_creates_the_app_role_during_bootstrap() -> None:
    """The migration's grant is guarded by a pg_roles check: no role, no grant."""
    initdb = _only(_render(MANAGED), "Cluster")["spec"]["bootstrap"]["initdb"]
    assert initdb["owner"] == "okf_owner"
    sql = initdb["postInitApplicationSQL"]
    assert any(s.startswith("CREATE ROLE okf_app ") for s in sql)


def test_existing_mode_runs_the_migration_as_the_owner_dsn() -> None:
    dsns = _named(_render(EXISTING), "Secret", "keepsake-dsn")["stringData"]
    assert dsns["owner-dsn"] == "postgres://owner@db/keepsake"
    assert dsns["app-dsn"] == "postgres://app@db/keepsake"


def test_the_server_reads_the_variables_the_cli_reads() -> None:
    tenant = "11111111-1111-1111-1111-111111111111"
    values = dict(
        MANAGED, **{"auth.fixedTenantId": tenant, "postgres.schema": "okf_other"}
    )
    docs = _render(values)
    server = _env(_only(docs, "Deployment"))
    assert server["KEEPSAKE_TENANT_ID"]["value"] == tenant
    assert server["KEEPSAKE_SCHEMA"]["value"] == "okf_other"
    assert _env(_only(docs, "Job"))["KEEPSAKE_SCHEMA"]["value"] == "okf_other"
    assert _container(_only(docs, "Deployment"))["command"] == ["keepsake", "serve"]
    assert _container(_only(docs, "Job"))["command"] == ["keepsake", "migrate"]


def test_the_service_type_and_node_port_are_configurable() -> None:
    default = _only(_render(MANAGED), "Service")
    assert default["spec"]["type"] == "ClusterIP"
    assert "nodePort" not in default["spec"]["ports"][0]

    exposed = _only(
        _render(
            dict(MANAGED, **{"service.type": "NodePort", "service.nodePort": "30800"})
        ),
        "Service",
    )
    assert exposed["spec"]["type"] == "NodePort"
    assert exposed["spec"]["ports"][0]["nodePort"] == 30800


def test_labels_derive_from_the_release_name() -> None:
    docs = _render(MANAGED, release="other")
    deployment = _only(docs, "Deployment")
    assert deployment["spec"]["template"]["metadata"]["labels"]["app"] == "other"
    assert _only(docs, "Service")["spec"]["selector"] == {"app": "other"}
    job = _only(docs, "Job")
    assert job["metadata"]["labels"]["app"] == "other-migrate"
    assert job["spec"]["template"]["metadata"]["labels"]["app"] == "other-migrate"
    assert _dsn_secret_key(deployment, release="other") == "app-dsn"


def test_readiness_asks_the_server_and_nothing_restarts_it() -> None:
    """A bound port says the process started; /readyz says it can still serve. After a
    database failover those differ for as long as the pool takes to rebuild, and a
    replica that keeps its place in the Service meanwhile fails every request routed
    to it.

    No liveness probe: a pod that is correctly refusing to start would be restarted
    by one, which turns a legible crash into restart noise.
    """
    deployment = _only(_render(MANAGED), "Deployment")
    container = _container(deployment)
    probe = container["readinessProbe"]["httpGet"]
    assert (probe["path"], probe["port"]) == ("/readyz", "http")
    assert "livenessProbe" not in container
    # The probe watches the port the server was told to bind, not the CLI's default.
    port = container["ports"][0]
    assert port["name"] == "http"
    assert _env(deployment)["KEEPSAKE_PORT"]["value"] == str(port["containerPort"])


def test_no_workload_takes_the_service_link_variables() -> None:
    """Every workload the chart ships must opt out of the injected link variables."""
    docs = _render(MANAGED)
    for kind in ("Deployment", "Job"):
        spec = _only(docs, kind)["spec"]["template"]["spec"]
        assert spec["enableServiceLinks"] is False, kind


def test_the_bootstrap_password_is_escaped_into_the_sql() -> None:
    values = dict(MANAGED, **{"postgres.cluster.appPassword": "it's"})
    initdb = _only(_render(values), "Cluster")["spec"]["bootstrap"]["initdb"]
    assert initdb["postInitApplicationSQL"] == [
        "CREATE ROLE okf_app LOGIN PASSWORD 'it''s'"
    ]
