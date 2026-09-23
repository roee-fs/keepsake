# Differential harness

Temporary. It runs the Python server and the Go server side by side and compares
them operation by operation. Delete it with the Python server.

## Setup

One throwaway Postgres with the roles `tests/conftest.py` creates:

```bash
docker run -d --name okf-diff-pg -e POSTGRES_PASSWORD=admin -p 127.0.0.1:55435:5432 postgres:17
docker exec okf-diff-pg psql -U postgres \
  -c "CREATE ROLE okf_owner LOGIN PASSWORD 'owner'" \
  -c "CREATE ROLE okf_app LOGIN PASSWORD 'app'" \
  -c "GRANT CREATE ON DATABASE postgres TO okf_owner"
uv sync
```

`DIFF_PG` overrides `127.0.0.1:55435/postgres`. Ports 18001 (Python) and 18002 (Go)
MUST be free.

## Run

```bash
for s in $(seq 1 20); do uv run python scripts/differential/run.py $s || break; done
```

Each seed runs four passes, each from an empty database: the Python MCP client in
`legacy` mode and in `auto` mode (which negotiates 2026-07-28), and the go-sdk client
(`goclient/`) at its default version and at 2025-11-25. Each pass drops and
re-migrates `okf_py` (Python `migrate`) and `okf_go` (Go `migrate`), starts both
servers on one tenant, and then:

- sends 2,000 seeded tool calls to both through that client, and compares
  `isError`, the structured content (key order exact, floats with `rel_tol=1e-6`,
  versions exact) and error text;
- for the Python client, compares each call's raw JSON-RPC response, envelope and
  key order included;
- in the first pass, compares the protocol surface: tools/list, initialize,
  server/discover, notifications, Accept and Content-Type refusals, other HTTP
  methods, and malformed or unsupported requests (divergence 11: status and
  JSON-RPC code only);
- calls every `/api` route with good and bad parameters and compares status codes
  and parsed bodies;
- exports both schemas with both CLIs and runs `diff -r` over the four bundles.

It prints the first 300 differences per era, a count by kind, and exits 1 if there
is any.

Tolerated differences are each named in `run.py` next to the check they relax.

## Teardown

```bash
docker rm -f okf-diff-pg
```
