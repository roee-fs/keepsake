# Changelog

Notable changes, newest first. This project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); until 1.0 the minor
version is where breaking changes land.

Releases are cut by tagging `vX.Y.Z`, which publishes the image, the chart and an
SBOM. `Chart.yaml`'s `version` and its `appVersion` MUST agree with the tag — a
test enforces it and the release workflow refuses otherwise.

## Unreleased

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
