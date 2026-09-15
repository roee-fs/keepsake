# Follow-ups

Known debt carried out of the v1 build. Nothing here is a correctness defect in
the isolation or round-trip guarantees.

## Open

- **Revision retention.** Still unimplemented, and still the item that only gets
  harder: `docs/design.md` notes the policy must be decided while
  `concept_revision` is nearly empty. Migration 0002 added the index that a
  pruning pass would need, but the policy — how long, pruned by what, and whether
  an export has to happen first — is a decision, not a patch.
- **Revisions outlive their concept.** Deleting a row from `concept` leaves its
  `concept_revision` rows behind, and re-creating that path then fails on
  `(tenant_id, path, version)` — the version restarts at 1. No tool deletes, so
  an agent cannot reach this today; a delete tool has to deal with it, either by
  cascading or by continuing the version sequence.
- **`test_verify.py`'s catalog-shadowing decoys detect via an exception** rather
  than the planted value. Real today; vacuous if `raw()` ever sets the GUC.
- **`POOL_SIZE` and `KEEPSAKE_PORT` are read at import.** Both now refuse a bad
  value legibly, but a config error still surfaces as a crash at startup rather
  than as a validated setting. The chart's `values.schema.json` catches the same
  mistakes earlier, so this only bites a deployment that is not the chart.

## Operability

- The chart sets no `nodeSelector`, `tolerations` or `affinity` passthrough. A
  `topologySpreadConstraint` covers the case that mattered; the rest is
  boilerplate until someone needs it.
- `docs/design.md` describes the store but not the operational surface the
  harness established — `/readyz`, the pool's reconnect bound, the worker-thread
  model. Worth one pass when the design doc is next touched.

## CI

- `e2e.yaml` runs on every pull request and takes several minutes. Worth a path
  filter once the repo has traffic.

## Known ceilings, deliberately accepted

These are documented where they bite and are not bugs to fix:

- The four round-trip fidelity ceilings — see **Fidelity** in the README. All
  four come from the `jsonb` round trip. Storing the serialized document as the
  source of truth and treating the columns as a derived index would remove them,
  which is a design change rather than a fix.
- `grep` is a case-insensitive POSIX regex, not a literal match, bounded by a
  5s `statement_timeout`.
- Text sizes are capped so a write cannot fail inside Postgres: 256KiB of body,
  4KiB of title or description, 1024 characters of path. `okf_search` and
  `okf_grep` cap `limit` at 200, advertised in the schema.
- `auth.mode` accepts only `none`. The `proxy` and `token` modes are on the
  roadmap and the schema refuses them until they exist.
- One tenant per deployment, via `auth.fixedTenantId`.
- **Nothing with a chart default is `required` in `values.schema.json`.**
  `helm upgrade --reuse-values` renders against the previous release's computed
  values, so a newly required key fails every in-place upgrade on the release
  that adds it. A test pins this.

## Done

Kept for the reasoning, which is the part that was expensive.

- **Both GIN indexes dropped** (migration 0002). `ts_match_vq` and
  `arraycontains` are not leakproof, so under `FORCE ROW LEVEL SECURITY` the
  planner never promotes either to an index condition: measured on postgres 17
  at 10,000 rows, the app role got a sequential scan (1,250 buffers, 3.25ms)
  where `row_security = off` got a bitmap index scan (23 buffers, 0.16ms). A
  composite `gin (tenant_id, search)` with `btree_gin` does not help — the
  planner's security-level check is independent of index coverage. They cost
  ~11% on writes and bought nothing. They come back only if the isolation model
  changes.
- **`concept_revision` gained `CHECK (op IN ('create','update'))` and the index
  serving `revisions()`' ordering** (migration 0002).
- **`_render_log` rendered the oldest revisions**, so `log.md` showed nothing
  recent on an active corpus. `revisions()` now takes the newest window and
  returns it oldest-first.
- **A failed store call was a protocol error.** Connection-level failures —
  `psycopg.OperationalError`, which covers `PoolTimeout`, `PoolClosed` and the
  "terminating connection due to administrator command" a restart produces — are
  now a tool error the agent can retry. Its siblings under `DatabaseError`
  (integrity, programming, data) still surface as protocol errors, because those
  are defects here and a polite retry would bury them.
- **The chart set no `resources`, `securityContext` or PodDisruptionBudget.**
  All three now ship, plus a topology spread. The memory limit is set and the CPU
  limit deliberately is not: a CPU limit throttles rather than kills, which turns
  a busy pod into a slow one and makes the readiness probe flap.
- **The image ran as `nobody`**, a name the kubelet cannot resolve, so
  `runAsNonRoot` would have refused to start the pod. It is `USER 65534` now.
- **`values.schema.json` did not pattern-match `auth.fixedTenantId` or
  `postgres.schema`.** Both are checked at template time now, the schema name
  because it is formatted into DDL rather than bound.
- **No lifespan hook closed `app.state.store`.** One wraps the MCP app's own
  lifespan now; Starlette 1.x dropped `add_event_handler`.
- **`_env_port` used `isdigit()`**, which admits non-decimal digits that `int()`
  then rejects, in the function whose contract is never to raise.
- **Nothing asserted `Store.raw()` is read-only.** `SET TRANSACTION READ ONLY`
  outside a transaction block is a Postgres *warning*, so enforcement could have
  degraded to advisory silently. The test asserts `ReadOnlySqlTransaction`
  specifically — without the setting the error is `InsufficientPrivilege`, so the
  exception type is what discriminates.
- **No test exercised a non-default `KEEPSAKE_SCHEMA`.** One now migrates into
  `okf_elsewhere` through the real command and round-trips a concept through it.
- **`test_search_never_returns_a_body` asserted `hasattr`**, which `Hit` being
  `slots=True` guarantees regardless of the query. It checks the values now.
- **`"0001"` was hardcoded in five assertions.** `keepsake.cli.head()` reads it
  from the packaged scripts.
- **`docs/design.md`'s install-modes block omitted `ownerDsn`**, which is a hard
  template-time requirement in `existing` mode.
- **CI had no `permissions:` or `concurrency:`, unpinned action refs, double
  runs on every branch, and no `timeout-minutes`.** All fixed; `ty` covers `e2e/`
  now too.
- **`e2e/run.sh` trapped `EXIT` but not `INT TERM`**, so Ctrl-C leaked a cluster.
