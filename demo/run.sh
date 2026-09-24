#!/usr/bin/env bash
# The round-trip drill: install keepsake on kind, import a bundle, let an agent edit it
# over MCP, export, and diff. Usage: demo/run.sh [bundle-dir] [--keep]
set -euo pipefail

cd "$(dirname "$0")/.."

BUNDLE=demo/bundle
KEEP=false
for arg in "$@"; do
  case $arg in
    --keep) KEEP=true ;;
    -*) echo "usage: demo/run.sh [bundle-dir] [--keep]" >&2; exit 2 ;;
    *) BUNDLE=$arg ;;
  esac
done
[[ -d $BUNDLE ]] || { echo "not a directory: $BUNDLE" >&2; exit 2; }
BUNDLE=$(cd "$BUNDLE" && pwd)

CLUSTER=keepsake-demo
# Distinct from up.sh's 30900 and the e2e's 30800, so all three can run on one machine.
NODE_PORT=31000
PG_NODE_PORT=31432
TENANT=00000000-0000-0000-0000-000000000001
OUT=${OUT:-/tmp/keepsake-demo-export}
IMAGE=${IMAGE:-ghcr.io/roee-fs/keepsake:$(sed -n 's/^appVersion: "\(.*\)"$/\1/p' charts/keepsake/Chart.yaml)}
MCP=http://localhost:$NODE_PORT/mcp
export KUBECONFIG="${TMPDIR:-/tmp}/$CLUSTER-kubeconfig"

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
beat() { bold "[${SECONDS}s] $*"; }
shown() { printf '\033[2m$ %s\033[0m\n' "$*"; "$@"; }

missing=()
for tool in docker kind kubectl helm jq curl git; do
  command -v "$tool" >/dev/null || missing+=("$tool")
done
if ((${#missing[@]})); then
  echo "missing: ${missing[*]}. Install from:" >&2
  echo "  docker  https://docs.docker.com/get-docker/" >&2
  echo "  kind    https://kind.sigs.k8s.io/docs/user/quick-start/#installation" >&2
  echo "  kubectl https://kubernetes.io/docs/tasks/tools/" >&2
  echo "  helm    https://helm.sh/docs/intro/install/" >&2
  echo "  jq      https://jqlang.org/download/" >&2
  exit 1
fi

# Only the default is ours to delete; any other OUT MUST not exist yet.
if [[ -e $OUT && $OUT != /tmp/keepsake-demo-export ]]; then
  echo "refusing to overwrite $OUT" >&2
  exit 2
fi

teardown() {
  $KEEP || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap teardown EXIT

# The keepsake CLI, run from the same image on the kind network so it reaches Postgres
# through the node. --user keeps exported files owned by the caller.
keepsake() {
  docker run --rm --network kind --user "$(id -u):$(id -g)" \
    -v "$BUNDLE:/bundle:ro" -v "$OUT:/out" \
    -e KEEPSAKE_DSN="postgres://okf_app:app@$CLUSTER-control-plane:$PG_NODE_PORT/keepsake" \
    -e KEEPSAKE_TENANT_ID="$TENANT" \
    "$IMAGE" keepsake "$@"
}

# One tools/call, printed as the curl it is. A tool error fails the demo.
mcp() {
  local request
  request=$(jq -cn --arg name "$1" --argjson args "$2" \
    '{jsonrpc: "2.0", id: 1, method: "tools/call", params: {name: $name, arguments: $args}}')
  printf '\033[2m$ curl %s -d %s\033[0m\n' "$MCP" "'${request:0:90}…'" >&2
  curl -sS --fail-with-body "$MCP" -H 'Content-Type: application/json' -H 'Accept: application/json' -d "$request" |
    jq -e '.result | if .isError then error(.content[0].text) else .structuredContent end'
}

beat "Stand up keepsake on kind"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: $NODE_PORT
        hostPort: $NODE_PORT
EOF
# Every command below is destructive against whatever context it lands in.
context=$(kubectl config current-context)
[[ "$context" == "kind-$CLUSTER" ]] || { echo "refusing to run against $context" >&2; exit 1; }

# A registry image is pulled by the node, since kind cannot side-load a multi-platform
# one. Only a local build, which no registry has, is loaded.
if ! docker pull -q "$IMAGE" >/dev/null 2>&1; then
  kind load docker-image "$IMAGE" --name "$CLUSTER"
fi
kubectl apply -f e2e/postgres.yaml
kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: postgres-demo
spec:
  type: NodePort
  selector:
    app: postgres
  ports:
    - port: 5432
      nodePort: $PG_NODE_PORT
EOF
kubectl wait --for=condition=available deploy/postgres --timeout=180s
helm install keepsake charts/keepsake \
  --set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" \
  --set postgres.mode=existing \
  --set postgres.dsn="postgres://okf_app:app@postgres:5432/keepsake" \
  --set postgres.ownerDsn="postgres://okf_owner:owner@postgres:5432/keepsake" \
  --set service.type=NodePort --set service.nodePort="$NODE_PORT" \
  --wait --timeout 180s >/dev/null
# The pod being ready does not mean the host port forwards to it yet.
for _ in $(seq 60); do curl -sf "http://localhost:$NODE_PORT/readyz" >/dev/null && break; sleep 1; done
password=$(kubectl get secret keepsake-admin -o jsonpath='{.data.password}' | base64 -d)
bold "Ready in ${SECONDS}s. Console: http://localhost:$NODE_PORT (password: $password)"

beat "Import your bundle"
rm -rf "$OUT" && mkdir -p "$OUT"
tar -C "$BUNDLE" --exclude=.git -cf - . | tar -C "$OUT" -xf -
git -C "$OUT" init -q
git -C "$OUT" add -A
git -C "$OUT" -c user.name=demo -c user.email=demo@localhost commit -qm "the bundle you brought"
shown keepsake import /bundle

beat "Export it straight back and diff against the original"
shown keepsake export /out
# index.md and log.md are generated at export, never stored.
git -C "$OUT" add -A
if git -C "$OUT" diff --cached --quiet -- . ':!index.md' ':!log.md'; then
  shown git -C "$OUT" diff --cached --stat -- . ':!index.md' ':!log.md'
  echo "No diff: every concept came back byte-for-byte."
elif [[ $BUNDLE == "$PWD/demo/bundle" ]]; then
  git -C "$OUT" --no-pager diff --cached -- . ':!index.md' ':!log.md'
  echo "the demo bundle did not round-trip" >&2
  exit 1
else
  bold "Each difference below SHOULD be one of the six fidelity exceptions in the README. Anything else is a bug; please report it."
  git -C "$OUT" --no-pager diff --cached -- . ':!index.md' ':!log.md'
fi
git -C "$OUT" -c user.name=demo -c user.email=demo@localhost commit -qm "exported by keepsake"

if [[ $BUNDLE == "$PWD/demo/bundle" ]]; then
  beat "An agent fixes a stale runbook over MCP"
  runbook=$(mcp okf_read '{"path": "runbooks/db-failover"}')
  fixed=$(jq -c '{path, expected_version: .version,
    body: (.body | sub("the DBA on-call in `#dba-oncall`"; "the data platform on-call in `#data-platform-oncall`"))}' <<<"$runbook")
  mcp okf_update "$fixed" >/dev/null
  mcp okf_create "$(jq -cn '{
    path: "incidents/2026-09-failover-drill", type: "Incident", title: "Failover drill",
    description: "A drill paged a channel nobody reads.",
    frontmatter: {tags: ["postgres", "drill"]},
    body: "The [failover runbook](/runbooks/db-failover.md) paged `#dba-oncall`, which [data platform](/teams/data-platform.md) archived in August. Nobody answered. The runbook now pages `#data-platform-oncall`.\n"}')" >/dev/null
  mcp okf_relate '{"from_path": "services/postgres", "to_path": "incidents/2026-09-failover-drill"}' >/dev/null

  beat "Export again and diff"
  shown keepsake export /out
  git -C "$OUT" add -A
  shown git -C "$OUT" --no-pager diff --cached --stat
  git -C "$OUT" --no-pager diff --cached
  if ! diff <(git -C "$OUT" diff --cached --name-only) demo/expected-changes.txt; then
    echo "the agent changed files demo/expected-changes.txt does not list" >&2
    exit 1
  fi
  # Set by demo.tape, so the recording holds on the diff.
  sleep "${PAUSE:-0}"
fi

if $KEEP; then
  bold "Still running. Point Claude Code at it:"
  echo "  claude mcp add --transport http keepsake $MCP"
  echo "Delete it with: kind delete cluster --name $CLUSTER"
else
  beat "Walk away"
  shown helm uninstall keepsake
  shown kind delete cluster --name "$CLUSTER"
  shown ls "$OUT"
  bold "The cluster is gone. Your knowledge isn't: $OUT"
  echo "Rerun with --keep to point your own agent at it."
fi
