# Changelog

Notable changes, newest first. This project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); until 1.0 the minor
version is where breaking changes land.

Releases are cut by tagging `vX.Y.Z`, which publishes the image, the chart and an
SBOM. `pyproject.toml`, `Chart.yaml`'s `version` and its `appVersion` must all
agree with the tag — a test enforces it and the release workflow refuses
otherwise.

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

### Security

- The admin console has a single account and no login throttling — see
  [`SECURITY.md`](SECURITY.md) for the threat model. Keep the Service
  `ClusterIP` and reach the console by `kubectl port-forward`.

### Upgrading

- The migration adds a policy that an earlier release's startup check does not
  recognise. During `helm upgrade` the migration hook runs before the new pods
  roll, so running pods are unaffected — but an old pod that restarts inside that
  window crash-loops until the rollout reaches it. It is minutes wide and
  self-resolving.
- A GitOps install (Argo CD, or `helm template | kubectl apply`) must set
  `admin.existingSecret`. `lookup` returns nothing under `helm template`, so an
  unguarded install regenerates the admin password on every sync.

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
