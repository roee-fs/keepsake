# Security policy

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Report it through GitHub's private vulnerability reporting:
[**open a draft advisory**](https://github.com/roee-fs/keepsake/security/advisories/new).
That channel is private to you and the maintainers, and it needs no account
beyond the GitHub one you already have.

You will get an initial response within **14 days**, and an assessment of
severity and a fix timeline within **30 days** of that. If you have heard
nothing in 14 days, please assume the report was lost and open a public issue
saying only that you sent a private report on a given date — no details.

## What we consider a vulnerability

keepsake's central claim is that one tenant's concepts cannot reach another
tenant, and that the claim is enforced by PostgreSQL rather than by application
code. Anything that undermines that is in scope, and we would rather hear about
it early than be right:

- Any read or write that crosses a tenant boundary, by any route.
- Anything that lets the server run with a database role that can bypass row
  level security — a superuser, a `BYPASSRLS` role, or the owner of the tables
  the policy guards. `keepsake.store.verify` exists to refuse exactly this at
  startup, so a way past that check is a vulnerability in itself.
- A tool argument that reaches SQL, a shell, a file path or a schema name
  without validation.
- A concept body, or any other tenant data, appearing in an error message, a
  log line or a tool result that the caller was not entitled to read.
- Anything in `charts/keepsake` that grants a workload more than it needs.

Out of scope, because they are documented behaviour rather than defects:

- The admin console reading across tenants. A console session sets the
  `okf.admin` GUC, and an `admin_read` policy on every tenant table admits rows to
  a connection that has set it. That policy is `FOR SELECT`, so no write crosses a
  tenant boundary. The console authenticates with a password session of its own,
  from `KEEPSAKE_ADMIN_PASSWORD`. That is a separate mechanism from `auth.mode`
  below, which governs the `/mcp` surface alone and is unchanged by any of this:
  an agent on `/mcp` still names no tenant and still reads only its own. A
  cross-tenant read reached without a console session is in scope.
- The round-trip fidelity ceilings in the README.
- `okf_grep` accepting a regular expression. It is a deliberate capability,
  bounded by a 5s statement timeout.
- The absence of authentication. `auth.mode` accepts only `none` today, and the
  server is meant to sit behind something that authenticates. This is stated in
  the README and the chart, and is a roadmap item rather than a bug — but if you
  have found a way to reach a keepsake that its operator believed was private,
  we want to know.
- The admin console has no login throttling. One account, one password, no
  lockout after failed attempts — that password is the whole perimeter, keep
  the Service `ClusterIP` and reach it by `kubectl port-forward`. Fronting it
  with anything else needs throttling added first.

Worth knowing even though it's an operational footgun rather than a
vulnerability: a GitOps install that omits `admin.existingSecret` regenerates
the admin password on every sync, because `helm template` and `helm template |
kubectl apply` — how Argo CD and similar tools render this chart — never
evaluate `lookup`, which is what would otherwise keep it stable across
upgrades. Set `admin.existingSecret` for any GitOps-managed install.

## Supported versions

Pre-release: `main` is the only supported version, and there are no backports
yet. This section will name real versions once the project starts tagging.
