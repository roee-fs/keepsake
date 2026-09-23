#!/usr/bin/env bash
# Regenerates frontend/src/client from frontend/openapi.json, the hand-maintained
# API contract. CI re-runs this and diffs the output to catch drift; see ci.yaml.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../frontend"
bunx @hey-api/openapi-ts -i openapi.json -o src/client -c @hey-api/client-fetch
