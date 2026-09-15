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
- **Revision retention.** Unimplemented, and `docs/design.md` notes the policy
  must be decided while `concept_revision` is still empty. It only gets harder.
- **`docs/design.md`'s install-modes block omits `ownerDsn`**, which is now a
  hard template-time requirement in `existing` mode.
- **`_env_port` uses `str.isdigit()`**, which admits non-decimal digits that
  `int()` then rejects — in the function whose job is to not raise.
  `isdecimal()` is the fix.

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
- `export_bundle` opens one transaction per concept.
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
- Store calls block the event loop; `anyio.to_thread` is the upgrade path.
- `auth.mode` accepts only `none`. The `proxy` and `token` modes are on the
  roadmap and the schema refuses them until they exist.
- One tenant per deployment, via `auth.fixedTenantId`.
