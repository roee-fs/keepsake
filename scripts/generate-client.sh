#!/usr/bin/env bash
# Regenerates frontend/openapi.json and frontend/src/client from the FastAPI
# schema. CI re-runs this and diffs the output to catch drift; see ci.yaml.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# create_api(concepts, auth) never touches either argument while registering
# routes -- they're only read from request.app.state at request time -- so
# placeholders build the real schema with no database. If that stops being
# true, this script breaks for a reason unrelated to client drift.
uv run python -c '
import json

from keepsake.server.api import create_api

app = create_api(None, None)
with open("frontend/openapi.json", "w") as f:
    json.dump(app.openapi(), f, indent=2)
    f.write("\n")
'

cd frontend
bunx @hey-api/openapi-ts -i openapi.json -o src/client -c @hey-api/client-fetch
