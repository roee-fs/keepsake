# Contributing to keepsake

Thanks for looking. This is a young project and the surface is small, so a
change that fits is usually easy to land.

## Getting set up

You need Go 1.27 and Docker.

```bash
go test ./...
```

The tests bring up PostgreSQL in a container through
[testcontainers](https://testcontainers.com/), so Docker MUST be running.
There is nothing to configure and no database to create by hand.

The Python tooling is only for `e2e/` and the chart tests. It needs
[uv](https://docs.astral.sh/uv/), which fetches Python 3.14 for you:
`uv sync && uv run pytest tests`.

## Before you open a pull request

Run what CI runs:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
uv run ruff check e2e tests
uv run pytest tests
```

`layering_test.go` is not optional decoration. It enforces two contracts that
the design depends on: `okf` never imports `internal/...`, because it is meant
to be publishable on its own; and `internal/store` never imports the server or
the CLI. If your change breaks one of those, the layering is usually the thing
to reconsider rather than the contract.

## The end-to-end suite

`e2e/` stands up a kind cluster, installs the Helm chart against a real
PostgreSQL and drives the deployed MCP server. CI runs it on every pull request;
you can run it yourself with `bash e2e/run.sh`, which needs `kind`, `kubectl`
and `helm`. It creates and deletes its own cluster and never touches your
current kubectl context.

For a cluster that stays up, `bash e2e/up.sh` installs the same release on a
`keepsake-local` cluster and prints the console URL and password. Re-run it after
a change; `bash e2e/down.sh` deletes it.

`harness/` is a much wider version of the same idea — two install modes,
concurrency, resilience, a real agent — and is deliberately **not** checked in.
If you need it, ask; it is kept out of the repository because it is a working
tool rather than a gate.

## Benchmarks

`bench/` measures search and agent behaviour. Each script needs Docker and Go.
`bench/benchmarks.pdf` holds the current results. A change that moves them MUST
update it.

- `python3 bench/beir.py` scores `okf_search` on BEIR over MCP. A change to
  search MUST keep SciFact nDCG@10 at 0.66 or above.
- `python3 bench/rankers.py` compares rankers on BEIR and LongMemEval, and times
  them in a 100k-concept tenant.
- `python3 bench/run.py` runs Claude over MCP on the tasks in
  `bench/tasks.json`, once per variant in `bench/variants/`. It needs the
  `claude` CLI and costs about $0.05 a run. A change to a tool description or to
  the server instructions MUST come with its result.
- `python3 bench/longmemeval.py` turns LongMemEval into two sets of 56 agent
  tasks for `run.py --tasks-file`: `tune.json` and `holdout.json`. Tune on the
  first. A result you report MUST come from the holdout, run once, after tuning
  is done.

## The console

The frontend needs [bun](https://bun.sh) (matches CI) and a running backend to
talk to:

```bash
cd frontend
bun install
bun run dev
```

Vite proxies `/api` and `/mcp` to `localhost:8000` — the session cookie is
`SameSite=Strict`, so a cross-origin dev server never gets it back. Point that
port at a real server: run `go run ./cmd/keepsake migrate` and
`go run ./cmd/keepsake serve` against a database you already have, with
`KEEPSAKE_DSN` and `KEEPSAKE_ADMIN_PASSWORD` set.

After any change to `frontend/openapi.json`, regenerate the generated client:

```bash
bash scripts/generate-client.sh
```

This assumes `frontend/node_modules` is already populated. On a fresh clone,
run `cd frontend && bun install` first — otherwise the `bunx` call inside the
script crashes in a way that looks like client drift and isn't.

### Screenshots

The console screenshots in the README live on the `pr-assets` branch, not in
this tree. They're refreshed by hand and aren't part of CI.

## What a good change looks like

- **Tests assert behaviour, not structure.** A test that would pass against a
  stub is not a test. If you are asserting that something is absent, assert on
  the value rather than on the shape: a check that a field is empty passes for
  free on a struct that never had it, and proves nothing about the query.
- **Comments explain why.** The code says what it does. A comment earns its line
  by recording a constraint, a measurement, or the reason an obvious alternative
  is wrong. There are a lot of those in this codebase and they are the most
  expensive thing in it.
- **Schema changes are migrations.** Never edit a migration that has shipped.
  Add a new one, and remember that `helm upgrade` re-runs the hook against a
  database already at head.
- **Anything touching isolation needs a test that fails without it.** The tenant
  boundary is the product. `internal/store/isolation_test.go` and
  `internal/store/verify_test.go` are where those live.

## Commits and pull requests

Commit subjects are imperative and lower case — "add a readiness probe", not
"Added readiness probe". Write a body when there is a fact the diff cannot show:
a measurement, a rejected alternative, the reason a workaround exists.

Keep a pull request to one change. A refactor and a fix in the same branch are
two pull requests.

## Cutting a release

For maintainers. Everything is driven by the tag, so there is nothing to click.

1. Set the version in `Chart.yaml`'s `version` and `appVersion`, which MUST agree.
   `tests/test_release_metadata.py` enforces this and the release refuses
   otherwise — `appVersion` is the image tag the chart pulls by default, so a
   drift there is an install that fails on a tag nobody built.
2. Rename `## Unreleased` in `CHANGELOG.md` to `## X.Y.Z`. The release checks for
   that heading and stops without it.
3. Merge, then `git tag vX.Y.Z && git push --tags`.

The tag publishes, in order: the image to `ghcr.io/roee-fs/keepsake`
for `linux/amd64` and `linux/arm64` with SLSA provenance, the chart to
`oci://ghcr.io/roee-fs/charts`, and a GitHub Release carrying a
CycloneDX SBOM. The release is created last, so it can never name an artefact
that was not pushed.

There is no rollback. To withdraw a release, publish a new patch version —
deleting a tag leaves anyone who already pulled the image holding it.

## Reporting bugs

Open an issue with the template. For anything touching tenant isolation, read
[SECURITY.md](SECURITY.md) first and report it privately instead.

## Code of conduct

Participation is covered by the [Code of Conduct](CODE_OF_CONDUCT.md).
