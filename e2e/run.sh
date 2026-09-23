#!/usr/bin/env bash
# Stand up the e2e cluster with up.sh, assert, tear down.
set -euo pipefail

cd "$(dirname "$0")/.."

CLUSTER=keepsake-e2e
# Never the caller's kubeconfig: every command here must reach this cluster and no
# other. Everything below inherits it, pytest's own kubectl calls included.
export KUBECONFIG="${TMPDIR:-/tmp}/$CLUSTER-kubeconfig"

# Set KEEPSAKE_E2E_KEEP=1 to leave the cluster up for debugging; $KUBECONFIG reaches it.
teardown() {
  local status=$?
  if ((status != 0)); then
    echo "=== e2e failed (exit $status): cluster state ==="
    kubectl get pods,jobs,svc -o wide || true
    kubectl describe pods || true
    for pod in $(kubectl get pods -o jsonpath='{.items[*].metadata.name}' || true); do
      echo "--- logs $pod ---"
      kubectl logs "$pod" --tail=50 --all-containers || true
    done
  fi
  if [[ -z "${KEEPSAKE_E2E_KEEP:-}" ]]; then
    CLUSTER=$CLUSTER bash e2e/down.sh
  fi
}
# INT and TERM as well as EXIT: bash runs an EXIT trap after a signal handler, but
# only if one is installed — without these, Ctrl-C leaks the cluster and its containers.
trap teardown EXIT INT TERM

# The ports and tag test_deployed.py assumes.
CLUSTER=$CLUSTER IMAGE_TAG=e2e NODE_PORT=30800 bash e2e/up.sh

uv run pytest e2e/test_deployed.py -v
