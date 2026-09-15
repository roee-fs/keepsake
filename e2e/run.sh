#!/usr/bin/env bash
# Stand up a kind cluster, install the chart against a plain Postgres, assert, tear down.
set -euo pipefail

cd "$(dirname "$0")/.."

CLUSTER=keepsake-e2e
IMAGE=keepsake:e2e
# Never the caller's kubeconfig: every command here must reach this cluster and no
# other. Everything below inherits it, pytest's own kubectl calls included.
export KUBECONFIG="${TMPDIR:-/tmp}/keepsake-e2e-kubeconfig"

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
    kind delete cluster --name "$CLUSTER"
    rm -f "$KUBECONFIG"
  fi
}
trap teardown EXIT

kind create cluster --name "$CLUSTER" --config e2e/kind.yaml --kubeconfig "$KUBECONFIG"
# The cluster is only ever addressed through $KUBECONFIG, but assert it anyway: every
# command below is destructive against whatever context it lands in.
context=$(kubectl config current-context)
[[ "$context" == "kind-$CLUSTER" ]] || { echo "refusing to run against $context"; exit 1; }

docker build -t "$IMAGE" .
kind load docker-image "$IMAGE" --name "$CLUSTER"

kubectl apply -f e2e/postgres.yaml
kubectl wait --for=condition=available deploy/postgres --timeout=180s

# ownerDsn, not just dsn: left unset the migration runs as the app role, which here
# cannot create the schema at all, and on a database where it can would end up owning
# it — which the server then refuses to serve against, for good.
helm install keepsake charts/keepsake \
  --set image.repository=keepsake --set image.tag=e2e \
  --set postgres.mode=existing \
  --set postgres.dsn="postgres://okf_app:app@postgres:5432/keepsake" \
  --set postgres.ownerDsn="postgres://okf_owner:owner@postgres:5432/keepsake" \
  --set service.type=NodePort --set service.nodePort=30800 \
  --wait --timeout 180s

uv run pytest e2e/test_deployed.py -v
