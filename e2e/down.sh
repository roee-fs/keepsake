#!/usr/bin/env bash
# Delete the cluster `up.sh` created. Touches nothing else on the machine.
set -euo pipefail

CLUSTER=${CLUSTER:-keepsake-local}

kind delete cluster --name "$CLUSTER"
rm -f "${TMPDIR:-/tmp}/$CLUSTER-kubeconfig"
