# Operations

## Install

```bash
helm install keepsake oci://ghcr.io/roee-fs/charts/keepsake --version 0.4.0
```

`postgres.mode` picks the database:

- `managed` creates a CloudNativePG cluster. The CloudNativePG operator MUST
  already be installed. Override `postgres.cluster.ownerPassword` and
  `appPassword` for any install that matters.
- `existing` uses a Postgres you run. Set `postgres.dsn` for the server and
  `postgres.ownerDsn` for the migration. They MUST be different roles. A GitOps
  install sets `postgres.existingSecret` instead, naming a Secret with keys
  `app-dsn` and `owner-dsn` that exists before the migration hook runs.

## Database roles

The migration runs as the owner role. It creates the schema and grants the app
role what it needs. The server runs as the app role. At startup it refuses a
superuser, a `BYPASSRLS` role, or the owner of the tables, because each one
bypasses row-level security.

In `existing` mode, create the roles before the first install:

```sql
CREATE ROLE keepsake_owner LOGIN PASSWORD '...';
CREATE ROLE keepsake_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '...';
GRANT CREATE ON DATABASE keepsake TO keepsake_owner;
```

The migration MUST NOT run as `postgres.dsn`. The tables would belong to the
server's role, and the server would refuse to start for good.

## Upgrades

A Helm hook runs `keepsake migrate` before every install and upgrade. Read the
**Upgrading** section of each release in [`CHANGELOG.md`](../CHANGELOG.md)
before you upgrade. Some migrations need free disk or a grant first.
`helm rollback` does not revert a migration. The changelog gives the SQL for each
downgrade.

## Backups

The chart configures no backups. You MUST set up one of these:

- Postgres backups: CloudNativePG's `backup` section on the `<release>-db`
  cluster, or your provider's snapshots in `existing` mode.
- A per-tenant bundle, which is plain markdown you can commit anywhere. The pod's
  filesystem is read-only, so run the binary on your machine against the
  database. Get the binary with
  `go install github.com/roee-fs/keepsake/cmd/keepsake@v0.4.0`. In `managed` mode:

  ```bash
  kubectl port-forward svc/<release>-db-rw 5432:5432 &
  keepsake export ./backup --tenant <uuid> \
    --dsn postgres://keepsake_app:<app password>@localhost:5432/keepsake
  ```

`keepsake import` restores a bundle into a tenant.

## Sizing

- The `posting` table takes about 8 times the space of `concept`.
- Every write appends a revision, a delete included. Nothing prunes revisions yet.
- The database sees `postgres.poolSize` × `replicaCount` connections.

## Auth and access

- In `none` mode, anything that reaches the Service can read and write the one
  tenant. Keep the Service `ClusterIP`.
- In `jwt` mode, see [Tenancy and authority](../README.md#tenancy-and-authority).
  The signing secret MUST NOT be readable by anything an LLM drives.
- The console shares the port with `/mcp` and has its own password in
  `<release>-admin`. A GitOps install MUST set `admin.existingSecret`, or the
  password changes on every sync.

## Uploading a bundle

`PUT /bundle?prefix=P` replaces the caller's concepts under `P/` with a gzipped
tar of OKF files. In jwt mode the token MUST carry `"scope": "bundle"`, which
`keepsake token --scope bundle` mints. A token without it gets a 403. The file `x/y.md` becomes
the concept `P/x/y`. Concepts outside `P/` are untouched. A file that fails
`keepsake import`'s per-file checks refuses the whole upload with a 422 that
lists every such file.

```bash
COPYFILE_DISABLE=1 tar -czf bundle.tgz -C ./bundle .
curl -X PUT --data-binary @bundle.tgz \
  -H "Authorization: Bearer $TOKEN" \
  "https://keepsake.example/bundle?prefix=docs/runbooks"
```

- A replace deletes concepts. Their history stays, ending in a `delete`
  revision, and a concept whose content is unchanged is not rewritten. Operators
  MUST NOT give a `bundle`-scoped token to anything an LLM drives. Auth mode none has no
  tokens, so any client that reaches it can upload.
- An upload MUST hold at least one concept. The limits are 32 MiB compressed,
  64 MiB unpacked, 20,000 files, 1 MiB per file, and 256 KiB per concept body,
  as for any write.
- The server runs one upload at a time. Another upload meanwhile gets a 503 with
  `Retry-After: 5`.
- Links to concepts outside the bundle are not checked. Operators SHOULD run
  `keepsake validate` on the directory first to catch them.
- The archive MUST end with tar's end-of-archive marker. A stream cut between
  entries gets a 400, so it cannot delete the files it lost.

## Metrics

`keepsake serve` serves Prometheus metrics at `/metrics` on
`KEEPSAKE_METRICS_PORT`, 9090 by default. They are unauthenticated and on their
own port, so the Service and any Ingress in front of it never expose them. The
chart annotates each pod with `prometheus.io/scrape`, and Prometheus MUST scrape
the pods, not the Service.

| Metric | Labels |
| --- | --- |
| `keepsake_tool_calls_total` | `tool`, `outcome`: `ok`, `conflict`, `tool_error`, `unavailable`, `error` |
| `keepsake_tool_call_duration_seconds` | `tool` |
| `keepsake_db_pool_{acquired,idle,total,max}_connections` | none |

`conflict` counts writes refused by `expected_version`. No metric carries a
tenant. The Go runtime and process metrics are included as well.

## Logs

`keepsake serve` writes JSON lines to stderr. `logLevel` in the chart, or
`KEEPSAKE_LOG_LEVEL`, sets the level: `debug`, `info` (the default), `warn` or
`error`.

| Message | Level | Fields |
| --- | --- | --- |
| `tool call` | info | `tool`, `outcome`, `duration_ms`, `tenant`, `actor` |
| `database unavailable` | warn | `tool` or `route`, `err` |
| `console login` / `console login refused` | info / warn | `remote_addr` |
| `api` | error | `method`, `route`, `err` |
| `tool call failed` | error | `tool`, `err` |
| `bundle replaced` | info | `tenant`, `actor`, `written`, `deleted` |
| `refused /mcp request` | warn | `reason` |
| `refused /bundle request` | warn | `reason`, and `actor` once the token verifies |

No line carries a tool's arguments, a concept body, a token or a password.
`remote_addr` is the TCP peer, so behind a proxy it names the proxy. At `warn`,
the per-call lines are dropped.

## Health

`/readyz` answers once the startup check has passed and the pool can hand out a
connection. The chart uses it as the readiness probe. There is no liveness
probe on purpose: a pod that refuses to start already crash-loops, and a restart
would hide the reason.
