#!/usr/bin/env bash
# Stand up a kind cluster with the chart installed against a plain Postgres.
#
# Idempotent: the cluster is reused if it exists, the image is rebuilt and reloaded,
# and the release is `helm upgrade --install`. `run.sh` calls this with the e2e values.
set -euo pipefail

cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-keepsake-local}
IMAGE_TAG=${IMAGE_TAG:-local}
# Distinct from the e2e cluster's 30800, so both can run on one machine.
NODE_PORT=${NODE_PORT:-30900}
CONSOLE_NODE_PORT=${CONSOLE_NODE_PORT:-30901}
# Never the caller's kubeconfig: every command here must reach this cluster and no other.
export KUBECONFIG=${KUBECONFIG:-"${TMPDIR:-/tmp}/$CLUSTER-kubeconfig"}

if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: $NODE_PORT
        hostPort: $NODE_PORT
      - containerPort: $CONSOLE_NODE_PORT
        hostPort: $CONSOLE_NODE_PORT
EOF
else
  kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
  # A cluster created before the console had its own port maps no host port for it.
  docker port "$CLUSTER-control-plane" "$CONSOLE_NODE_PORT/tcp" >/dev/null 2>&1 ||
    echo "warning: $CLUSTER maps no host port $CONSOLE_NODE_PORT. Run kind delete cluster --name $CLUSTER to reach the console." >&2
fi
# Every command below is destructive against whatever context it lands in.
context=$(kubectl config current-context)
[[ "$context" == "kind-$CLUSTER" ]] || { echo "refusing to run against $context"; exit 1; }

docker build -t "keepsake:$IMAGE_TAG" .
kind load docker-image "keepsake:$IMAGE_TAG" --name "$CLUSTER"

kubectl apply -f e2e/postgres.yaml
kubectl wait --for=condition=available deploy/postgres --timeout=180s

upgrading=false; helm status keepsake >/dev/null 2>&1 && upgrading=true

# ownerDsn, not just dsn: left unset the migration runs as the app role, which here
# cannot create the schema at all, and on a database where it can would end up owning
# it — which the server then refuses to serve against, for good.
helm upgrade --install keepsake charts/keepsake \
  --set image.repository=keepsake --set image.tag="$IMAGE_TAG" \
  --set postgres.mode=existing \
  --set postgres.dsn="postgres://keepsake_app:app@postgres:5432/keepsake" \
  --set postgres.ownerDsn="postgres://keepsake_owner:owner@postgres:5432/keepsake" \
  --set service.type=NodePort --set service.nodePort="$NODE_PORT" \
  --set service.consoleNodePort="$CONSOLE_NODE_PORT" \
  --wait --timeout 180s
# A reused tag leaves the pod on the old image, since the Deployment spec did not change.
if $upgrading; then
  kubectl rollout restart deploy/keepsake
  kubectl rollout status deploy/keepsake --timeout=180s
fi

password=$(kubectl get secret keepsake-admin -o jsonpath='{.data.password}' | base64 -d)
echo
echo "console:    http://localhost:$CONSOLE_NODE_PORT  (password: $password)"
echo "mcp:        http://localhost:$NODE_PORT/mcp"
echo "kubeconfig: $KUBECONFIG"
