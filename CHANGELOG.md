# Changelog

Notable changes, newest first. This project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); until 1.0 the minor
version is where breaking changes land.

`scripts/release.sh X.Y.Z` opens a release PR. Merging it publishes the image, the
chart and an SBOM, and tags `vX.Y.Z`. See CONTRIBUTING.md.

## Unreleased

### Added

- `bench/run.py` builds a variant's server from a git ref (`"ref": "origin/main"`), each with its
  own database, so a run compares server builds. `run.json` records each build's commit.
- The report adds input tokens, reads, link-only reads (a concept opened only through a link) and
  missed links (a required concept shown as a link and never read).
- `bench/musique.py` and `bench/iirc.py` turn MuSiQue and IIRC (both CC BY 4.0) into agent
  tasks, with bundles that differ only in their links. Tasks grade on `answer_any`, the answer
  or an alias as whole words.
- Prometheus metrics at `/metrics` on `KEEPSAKE_METRICS_PORT`, 9090 by default:
  tool calls by tool and outcome, tool latency, and pool connections. The port is
  not on the Service. The chart annotates pods for scraping.

## 0.3.0 — 2026-09-24

### Added

- `compose.yaml`: `docker compose up` runs a single-tenant keepsake on
  `localhost:8000`, with no Kubernetes.
- `docs/`: the MCP tool reference, operations, and alternatives.
- `auth.mode: jwt`. Each `/mcp` request carries an HS256 bearer token, and its
  `tctx.tenant` claim picks the tenant, so one release serves many tenants. The
  server checks `iss`, `aud` and `exp`, answers 401 for a bad token and 403 for
  one naming no tenant, and records `sub` as `updated_by`. Secrets come from
  `KEEPSAKE_JWT_SECRET_FILE`, one per line, so a secret rotates without downtime.
- `keepsake token --tenant T --sub S --ttl D`, which prints a token for one
  tenant, for clients that must not hold the secret.

### Changed

- The deployment sets `KEEPSAKE_AUTH_MODE`. `serve` refuses `--tenant` in jwt
  mode, and refuses any mode other than `none` or `jwt`.
- `serve` refuses the nil UUID as its tenant in none mode, instead of starting
  and then failing every tool call.
- `okf_search` ranks with BM25 (k1=0.9, b=0.4) over a new `posting` table: one
  row per concept and term, kept in step by a trigger on `concept`. On BEIR,
  nDCG@10 rises from 0.387 to 0.682 on SciFact, 0.258 to 0.327 on NFCorpus and
  0.029 to 0.227 on FiQA. On LongMemEval session retrieval, recall@5 rises from
  0.854 to 0.928. In a 100k-concept tenant, a search takes 6-150ms against
  260-440ms before; one common term takes 81ms against 339ms. These times skip
  row-level security. As the app role, a 3-term search takes 63ms against 41ms.
  `bench/rankers.py` reproduces all of it.
- A query word joined by `_`, such as `purge_tenant`, now matches either part
  rather than the phrase. Use `okf_grep` for an exact identifier.
- Search runs as the app role under row-level security, like every other read.
  It needs no `SECURITY DEFINER` and no extension.
- The MCP server sends `instructions`: treat keepsake as memory of any kind,
  search it before answering, follow links, prefer the more specific source, say
  when it holds no answer, and update rather than duplicate. Claude passes 49 of
  56 held-out LongMemEval questions with them, against 44 with a wording that
  described a team knowledge base, and 32-33 of the 33 demo tasks.
  `bench/longmemeval.py` builds the tuning and holdout sets.

### Removed

- The Swagger UI at `/api/docs`. It loaded its scripts from a CDN, and nothing
  linked to it. `/api/openapi.json` still serves the contract.

### Security

- `purge_tenant` no longer grants `EXECUTE` to `PUBLIC`. Since 0.1.0, any role
  with `USAGE` on the schema could purge any tenant by setting the GUC first.
  Only `okf_app` holds `USAGE` in `managed` mode.

### Upgrading

- The migration backfills `posting` from every concept inside its own
  transaction. Writes to `concept` wait until it commits, and the time grows
  with the number of concepts. Reads go on.
- The backfill MUST have free disk of about 20 times the size of `concept`: 8
  for `posting` and 12 for WAL. In `managed` mode WAL shares the volume, and
  `helm upgrade` never resizes it, so grow `spec.storage.size` on the
  `<release>-db` cluster first.
- `posting` takes about 8 times the space of `concept`: 4.6 GB against 576 MB
  for 195k synthetic concepts of about 150 words. A write that changes a
  concept's text also rewrites its postings: 0.71ms per concept against 0.11ms.
- In `existing` mode, a server role other than `okf_app` needs `SELECT`,
  `INSERT` and `DELETE` on `posting`. Without them, `okf_search` fails, and so
  does every create, delete, and update that changes a concept's text or path,
  from 0.2.0 pods too. `posting` does not exist until the migration runs,
  so you MUST grant them first, as the owner role: `ALTER DEFAULT PRIVILEGES FOR
  ROLE <owner> IN SCHEMA okf GRANT SELECT, INSERT, DELETE ON TABLES TO <server
  role>;`. `managed` mode grants them to `okf_app`.
- The server never calls `purge_tenant`. A role that runs it by hand needs
  `EXECUTE` on it, which `PUBLIC` no longer holds.
- A 0.2.0 pod accepts the new schema, so pods MAY roll in any order.
- A rollback to 0.2.0 MUST first take the schema back to revision 0004, as the
  owner role, with `okf` replaced by your `KEEPSAKE_SCHEMA` if you set one:
  `BEGIN; DROP TRIGGER concept_posting ON okf.concept; DROP TABLE okf.posting;
  DROP FUNCTION okf.posting_sync(); UPDATE okf.alembic_version SET version_num =
  '0004'; COMMIT;`.

## 0.2.0 — 2026-09-23

### Added

- An `admin_read` policy on every tenant table, letting a connection that sets
  `okf.admin` read across tenants. It is `FOR SELECT`, so writes stay scoped to
  one tenant even for an admin.
- An admin console, served by the same pod and port as `/mcp` (one image, no
  second Service, no CORS): login, an overview (concepts, types, revisions,
  orphans, a writes-per-day chart, recent activity), browse with a path tree
  and search/grep, and concept detail with rendered markdown, frontmatter,
  links, backlinks and revision history. Helm generates the password at
  install into a `<release>-admin` Secret; the session cookie's signing key
  derives from it, so changing the password invalidates every open session.
- A link graph of one tenant's concepts in the console, backed by
  `GET /api/graph`. It draws up to 500 concepts and marks link targets no
  concept holds.
- `demo/run.sh`, which installs the chart on kind, round-trips a bundle and has
  an agent edit it over MCP, then diffs the export.

### Changed

- The server and CLI are a single static Go binary. The image no longer
  contains Python. Behaviour is the same except for the following:
  - `import` refuses `!!binary`, `!!set`, `!!omap` and application tags (`!foo`)
    with `<file>: unsupported YAML tag <tag>`.
  - `import` and `validate` report a file that is not valid UTF-8, or whose
    frontmatter is not a mapping, as an error with exit 1 instead of a traceback.
  - YAML syntax errors keep the `<file>: ` prefix, but the parser's wording after
    it differs.
  - A CLI usage error still exits 2, but its message MAY differ. Long flags MUST
    be spelled in full.
  - `export` writes rows the Python release could not, such as integers over
    4300 digits and deeply nested frontmatter.
  - A tool-argument error still names the field and the expected type. The rest
    of the sentence differs.
  - A malformed MCP request on the handshake-era transport (no
    `mcp-protocol-version` header, or a handshake version) gets the same HTTP
    status and JSON-RPC error code, but its message MAY differ. On either
    transport, a method keepsake does not serve answers -32601 even where its
    params are invalid and Python answered -32602.
  - Floats in JSON keep their value, but their text MAY differ (`1e-05` vs
    `0.00001`).
  - Admin API timestamps are always in UTC. The instant is unchanged.
  - An unmatched admin route answers 404 or 405 with a plain-text body. `HEAD`
    works on every `GET` route.
  - A trailing-slash redirect is still a 307 to the same place, but its
    `Location` is relative, not absolute, and a `GET` gets a short HTML body.
  - A login body with invalid UTF-8 or a lone surrogate answers 422, not 500.
  - Logout clears the cookie with the login cookie's `HttpOnly`, `SameSite` and
    `Secure` attributes, where Starlette's `delete_cookie` sent `SameSite=lax`.
  - There is no per-request access log, unlike uvicorn's.
  - `okf_relate` refuses a `to_path` whose appended link would not read back as
    that path, such as `./b/y` or `b y`. Python appended the link anyway and
    reported success.
  - A write or `import` whose frontmatter holds a NUL byte is refused with
    `frontmatter must not contain a NUL byte`. Python passed it to Postgres,
    which refused the write with a driver error.
  - The startup check refuses a permissive tenant policy that does not read
    exactly `(tenant_id = (current_setting('okf.current_tenant'::text))::uuid)`.
    Python accepted any expression that mentioned `okf.current_tenant`, such as
    one ending in `OR true`.
  - At most `KEEPSAKE_POOL_SIZE - 1` tool calls (at least one) hold a
    connection at once, so the console always has one. Further calls queue,
    where Python answered "temporarily unavailable" after a 30-second wait.
  - `migrate` against a database stamped with a revision this release does not
    know prints `keepsake: Can't locate revision identified by '<id>'` and exits
    1. Python raised a traceback, also with exit 1.
- `okf_relate` leaves one blank line before the link it appends. On a body that
  ended in a newline, which is every imported concept, it left two.
- `okf_create`, `okf_update` and `okf_relate` store CRLF in a body as LF, as
  `import` does. They stored it verbatim, so `export` could write CRLF.

### Security

- The admin console has a single account and no login throttling — see
  [`SECURITY.md`](SECURITY.md) for the threat model. Keep the Service
  `ClusterIP` and reach the console by `kubectl port-forward`.

### Upgrading

- Existing databases upgrade in place, and the chart needs no value change.
- A console session the Python server issued stays valid on the Go server.
- The migration adds a policy that an earlier release's startup check does not
  recognise. During `helm upgrade` the migration hook runs before the new pods
  roll, so running pods are unaffected — but an old pod that restarts inside that
  window crash-loops until the rollout reaches it. It is minutes wide and
  self-resolving.
- A GitOps install (Argo CD, or `helm template | kubectl apply`) must set
  `admin.existingSecret`. `lookup` returns nothing under `helm template`, so an
  unguarded install regenerates the admin password on every sync.
- A rollback to 0.1.0 MUST first take the schema back to revision 0002, as the
  owner role. 0.1.0's startup check rejects the `admin_read` policy, and
  `helm rollback` does not revert a migration. This release's image has no
  Python, so run Alembic's downgrade by hand, with `okf` replaced by your
  `KEEPSAKE_SCHEMA` if you set one:
  `BEGIN; DROP POLICY admin_read ON okf.concept; DROP POLICY admin_read ON
  okf.concept_revision; UPDATE okf.alembic_version SET version_num = '0002';
  COMMIT;`.

## 0.1.0 — 2026-09-15

### Added

- Row-level tenant isolation enforced by PostgreSQL, with a startup check that
  refuses to serve when the server's role could bypass it — a superuser, a
  `BYPASSRLS` role, or the owner of the guarded tables.
- Seven MCP tools over streamable HTTP: `okf_list`, `okf_search`, `okf_grep`,
  `okf_read`, `okf_create`, `okf_update`, `okf_relate`.
- `okf_core`: OKF parse and serialise with documented round-trip fidelity, kept
  publishable on its own by an import-linter contract.
- CLI: `import`, `export`, `validate`, `migrate`, `serve`.
- Helm chart with `managed` (CloudNativePG) and `existing` Postgres modes,
  resource requests, a non-root read-only security context, a
  PodDisruptionBudget, and a `/readyz` probe that follows the database rather
  than the bound port.
- Tool arguments validated against their advertised JSON Schema before dispatch,
  so a bad argument is something an agent can correct rather than a transport
  failure.

### Security

- Connection-level database failures surface as retryable tool errors; integrity
  and programming errors still surface as protocol errors, because those are
  defects rather than weather.
- The pool checks a connection before handing it out and bounds its reconnect
  backoff, so a database restart does not leave replicas serving dead
  connections.
