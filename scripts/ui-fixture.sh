#!/usr/bin/env bash
# Brings up a real stack for Playwright: testcontainers Postgres, migrate, a
# seeded bundle imported into two tenants, then `keepsake serve`. Prints
# PORT=<port> once ready; tears the container down on exit.
#
# `exec` replaces this shell with the Python process, so a SIGTERM from
# Playwright's webServer reaches uvicorn directly and its graceful shutdown
# runs the container cleanup in scripts/ui_fixture.py's `with` block.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [ ! -d frontend/dist ]; then
  (cd frontend && bun install --frozen-lockfile && bun run build)
fi

exec uv run python3 scripts/ui_fixture.py
