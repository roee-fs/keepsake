# Operations

## Install

```bash
helm install keepsake oci://ghcr.io/roee-fs/charts/keepsake --version 0.3.0
```

`postgres.mode` picks the database:

- `managed` creates a CloudNativePG cluster. The CloudNativePG operator MUST
  already be installed. Override `postgres.cluster.ownerPassword` and
  `appPassword` for any install that matters.
- `existing` uses a Postgres you run. Set `postgres.dsn` for the server and
  `postgres.ownerDsn` for the migration. They MUST be different roles.

## Database roles

The migration runs as the owner role. It creates the schema and grants the app
role what it needs. The server runs as the app role. At startup it refuses a
superuser, a `BYPASSRLS` role, or the owner of the tables, because each one
bypasses row-level security.

In `existing` mode, create the roles before the first install:

```sql
CREATE ROLE okf_owner LOGIN PASSWORD '...';
CREATE ROLE okf_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '...';
GRANT CREATE ON DATABASE keepsake TO okf_owner;
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
  `go install github.com/roee-fs/keepsake/cmd/keepsake@v0.3.0`. In `managed` mode:

  ```bash
  kubectl port-forward svc/<release>-db-rw 5432:5432 &
  keepsake export ./backup --tenant <uuid> \
    --dsn postgres://okf_app:<app password>@localhost:5432/keepsake
  ```

`keepsake import` restores a bundle into a tenant.

## Sizing

- The `posting` table takes about 8 times the space of `concept`.
- Every write appends a revision. Nothing prunes revisions yet.
- The database sees `postgres.poolSize` × `replicaCount` connections.

## Auth and access

- In `none` mode, anything that reaches the Service can read and write the one
  tenant. Keep the Service `ClusterIP`.
- In `jwt` mode, see [Serving many tenants](../README.md#serving-many-tenants).
  The signing secret MUST NOT be readable by anything an LLM drives.
- The console shares the port with `/mcp` and has its own password in
  `<release>-admin`. A GitOps install MUST set `admin.existingSecret`, or the
  password changes on every sync.

## Health

`/readyz` answers once the startup check has passed and the pool can hand out a
connection. The chart uses it as the readiness probe. There is no liveness
probe on purpose: a pod that refuses to start already crash-loops, and a restart
would hide the reason.
