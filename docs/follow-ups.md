# Follow-ups

Known debt carried out of the v1 build, triaged by the whole-branch review as
acceptable to ship. Nothing here is a correctness defect in the isolation or
round-trip guarantees.

## Worth doing first

- **Drop the GIN index on `links`.** `backlinks` is the only predicate on the
  column, and `arraycontains` is not leakproof — so under `FORCE ROW LEVEL
  SECURITY` no query the app role runs can reach the index. It costs write
  amplification and buys nothing. Needs a migration. Measured evidence and the
  reason `= ANY` beats `@>` here are in `docs/design.md`.
- **Drop the GIN index on `search` too** — same root cause. `ts_match_vq` is not
  leakproof, so under `FORCE ROW LEVEL SECURITY` the planner never promotes it to
  an index condition and `okf_search` is always a sequential scan. Measured on
  postgres 17 at 10,000 rows: Seq Scan, 1,250 buffers, 3.25ms as the app role,
  against Bitmap Index Scan, 23 buffers, 0.16ms with `row_security = off`. A
  composite `gin (tenant_id, search)` with `btree_gin` does **not** help: the
  planner's security-level check is independent of index coverage. Keeping it
  costs ~11% on writes (2000 inserts: 266ms with, 238ms without) plus disk.
  Both indexes only return if the isolation model changes, so this is a
  schema-and-spec decision, not a migration to write today.
- **Revision retention.** Unimplemented, and `docs/design.md` notes the policy
  must be decided while `concept_revision` is still empty. It only gets harder.
- **`docs/design.md`'s install-modes block omits `ownerDsn`**, which is now a
  hard template-time requirement in `existing` mode.
- **`POOL_SIZE` parses with a bare `int()`**, so a garbage value is a traceback at
  import rather than a message. Failing loudly on a bad pool size is the right
  outcome; failing legibly would be better.
- **Revisions outlive their concept.** Deleting a row from `concept` leaves its
  `concept_revision` rows behind, and re-creating that path then fails on
  `(tenant_id, path, version)` — the version restarts at 1. No tool deletes, so
  an agent cannot reach this today; a delete tool has to deal with it, either by
  cascading or by continuing the version sequence.
- **A failed store call is a protocol error, not a tool error.** When the
  database is genuinely unreachable the agent sees `couldn't get a connection
  after 30.00 sec` as a transport failure rather than something it can act on.
  Readiness now takes the replica out of the Service first, so the window is
  small, but a retryable tool error would be better still.

## Tests

- No test exercises a non-default `KEEPSAKE_SCHEMA`; `test_isolation.py`,
  `test_concurrency.py` and `test_migration.py` hardcode `okf.`, so that path
  fails with a misleading "relation does not exist".
- Nothing asserts `Store.raw()` is actually read-only. `SET TRANSACTION READ
  ONLY` outside a transaction block is a Postgres *warning*, so a refactor could
  silently revert enforcement to advisory with every test still green.
- `test_search_never_returns_a_body`'s `hasattr` assertion is structurally
  guaranteed by `Hit` being `slots=True`; the real invariant is proved
  elsewhere, over the wire.
- `test_verify.py`'s catalog-shadowing decoys detect via an exception rather
  than the planted value. Real today; vacuous if `raw()` ever sets the GUC.
- `"0001"` is hardcoded in two e2e assertions, so every future migration edits
  the e2e.

## Operability

- `_render_log` renders the *oldest* revisions, so `log.md` shows nothing recent
  on an active corpus.
- No lifespan hook closes `app.state.store`; process exit covers it in a pod.
- The chart sets no `resources`, `securityContext`, or PodDisruptionBudget
  despite `replicaCount: 2`.
- `values.schema.json` does not pattern-match `auth.fixedTenantId` as a UUID or
  `postgres.schema` as an identifier; both fail at container start instead.
- `concept_revision` has no `CHECK (op IN (...))` and no index serving
  `revisions()`' ordering.

## CI

- No `permissions:` or `concurrency:` blocks, action refs unpinned,
  `push` + `pull_request` double-runs branches, `ty check` excludes `e2e/`, and
  `e2e.yaml` has no `timeout-minutes`. Worth one hardening pass on a public repo.
- `e2e/run.sh` traps `EXIT` but not `INT TERM`, so Ctrl-C leaks a kind cluster.

## Known ceilings, deliberately accepted

These are documented where they bite and are not bugs to fix:

- The four round-trip fidelity ceilings — see **Fidelity** in the README.
- `grep` is a case-insensitive POSIX regex, not a literal match, bounded by a
  5s `statement_timeout`.
- Text sizes are capped so a write cannot fail inside Postgres: 256KiB of body,
  4KiB of title or description, 1024 characters of path. `okf_search` and
  `okf_grep` cap `limit` at 200, advertised in the schema.
- `auth.mode` accepts only `none`. The `proxy` and `token` modes are on the
  roadmap and the schema refuses them until they exist.
- One tenant per deployment, via `auth.fixedTenantId`.
