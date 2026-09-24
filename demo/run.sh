#!/usr/bin/env bash
# Shared memory for agents: install keepsake on kind, import a knowledge base, and watch
# one agent learn what another agent wrote. Usage: demo/run.sh [--keep]
set -euo pipefail

cd "$(dirname "$0")/.."

KEEP=false
for arg in "$@"; do
  case $arg in
    --keep) KEEP=true ;;
    *) echo "usage: demo/run.sh [--keep]" >&2; exit 2 ;;
  esac
done

CLUSTER=keepsake-demo
# Distinct from up.sh's 30900 and the e2e's 30800, so all three can run on one machine.
NODE_PORT=31000
PG_NODE_PORT=31432
TENANT=00000000-0000-0000-0000-000000000001
IMAGE=${IMAGE:-ghcr.io/roee-fs/keepsake:$(sed -n 's/^appVersion: "\(.*\)"$/\1/p' charts/keepsake/Chart.yaml)}
# Haiku 4.5 often skips the search or writes a stray concept; Sonnet 5 follows the brief.
MODEL=${MODEL:-claude-sonnet-5}
MCP=http://localhost:$NODE_PORT/mcp
export KUBECONFIG="${TMPDIR:-/tmp}/$CLUSTER-kubeconfig"

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
beat() { bold "[${SECONDS}s] $*"; }
shown() { printf '\033[2m$ %s\033[0m\n' "$*"; "$@"; }

missing=()
for tool in docker kind kubectl helm jq curl claude; do
  command -v "$tool" >/dev/null || missing+=("$tool")
done
if ((${#missing[@]})); then
  echo "missing: ${missing[*]}. Install from:" >&2
  echo "  docker  https://docs.docker.com/get-docker/" >&2
  echo "  kind    https://kind.sigs.k8s.io/docs/user/quick-start/#installation" >&2
  echo "  kubectl https://kubernetes.io/docs/tasks/tools/" >&2
  echo "  helm    https://helm.sh/docs/intro/install/" >&2
  echo "  jq      https://jqlang.org/download/" >&2
  echo "  claude  https://docs.claude.com/en/docs/claude-code/setup" >&2
  exit 1
fi

teardown() {
  rm -rf "${transcript:-}" "${agent_dir:-}"
  $KEEP || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap teardown EXIT

# The keepsake CLI, run from the same image on the kind network so it reaches Postgres
# through the node.
keepsake() {
  docker run --rm --network kind -v "$PWD/demo/bundle:/bundle:ro" \
    -e KEEPSAKE_DSN="postgres://okf_app:app@$CLUSTER-control-plane:$PG_NODE_PORT/keepsake" \
    -e KEEPSAKE_TENANT_ID="$TENANT" \
    "$IMAGE" keepsake "$@"
}

READER="You answer questions from a knowledge base that several agents share through the \
keepsake MCP tools. Search it and read what you find before you answer. Answer in at most \
two sentences, name the concept paths you used, and if it records why something is so, \
say why."
WRITER="You keep a knowledge base that several agents share through the keepsake MCP tools. \
When you learn something it lacks, search for the concepts it contradicts and read each \
one. Update each with expected_version set to the version you read, and say in the \
correction what changed and why, so the next agent to read it learns both."
MCP_CONFIG=$(jq -cn --arg url "$MCP" '{mcpServers: {keepsake: {type: "http", url: $url}}}')
transcript=$(mktemp)
# Agents start in an empty directory with only project settings, so your own Claude Code
# settings, hooks, plugins and CLAUDE.md never reach them. Your login still does.
agent_dir=$(mktemp -d)

# One fresh Claude session with only keepsake for memory. It prints each MCP call as it
# happens, then the answer, which it also leaves in $answer. Extra flags go to claude.
agent() {
  local who=$1 system=$2 prompt=$3 start=$SECONDS
  shift 3
  printf '\033[1m%s:\033[0m %s\n' "$who" "$prompt"
  (cd "$agent_dir" && claude -p "$prompt" --setting-sources project --model "$MODEL" \
    --append-system-prompt "$system" --strict-mcp-config --mcp-config "$MCP_CONFIG" \
    --tools "" --allowedTools mcp__keepsake --output-format stream-json --verbose "$@") |
    tee "$transcript" |
    jq -rj --unbuffered 'select(.type == "assistant") | .message.content[]
      | select(.type == "tool_use")
      | "\u001b[2m  → \(.name | sub("mcp__keepsake__"; "")) \(.input | del(.body) | tojson | .[:100])\u001b[0m\n"'
  answer=$(jq -r 'select(.type == "result") | .result' "$transcript")
  printf '\033[32m  %s\033[0m\n' "$answer"
  printf '\033[2m  answered in %ss\033[0m\n' "$((SECONDS - start))"
}

QUESTION="Who do I page before a Postgres failover?"
# Agent A only answers, so it cannot fix what it reads instead of answering.
READ_ONLY=(--disallowedTools mcp__keepsake__okf_create mcp__keepsake__okf_update mcp__keepsake__okf_relate)

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

beat "Import an on-call team's knowledge base"
shown keepsake import /bundle

beat "Agent A asks"
agent "Agent A" "$READER" "$QUESTION" "${READ_ONLY[@]}"

beat "Agent B, which just ran a failover drill, tells keepsake what it learned"
agent "Agent B" "$WRITER" "I just ran a Postgres failover drill. The runbook said to page #dba-oncall. \
Nobody answered: that channel was archived when the DBA team became data platform. Data \
platform answered on #data-platform-oncall. Record this."

beat "Agent A asks again, in a new session"
agent "Agent A" "$READER" "$QUESTION" "${READ_ONLY[@]}"
if [[ $answer != *data-platform-oncall* ]]; then
  echo "agent A did not learn what agent B recorded" >&2
  exit 1
fi

if $KEEP; then
  bold "Still running. Point Claude Code at it:"
  echo "  claude mcp add --transport http keepsake $MCP"
  echo "Delete it with: kind delete cluster --name $CLUSTER"
else
  beat "Tear down"
  shown helm uninstall keepsake
  shown kind delete cluster --name "$CLUSTER"
fi
