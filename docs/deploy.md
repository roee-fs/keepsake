# Deploy

These steps install keepsake into a Postgres you already run (`existing` mode)
and serve many tenants (`jwt` mode). Keepsake creates its own schema, `okf`, and
touches nothing else in the database.

## 1. Create the database roles

Keepsake needs two roles. The names are fixed.

| Role | Used by | Needs |
| --- | --- | --- |
| `keepsake_owner` | the migration Job | `CREATE` on the database. It will own the `okf` schema. |
| `keepsake_app` | the server | nothing yet. The migration grants what it needs. |

Run this as a role that can create roles, before the first install:

```sql
CREATE ROLE keepsake_owner LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '...';
CREATE ROLE keepsake_app   LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD '...';
GRANT CREATE ON DATABASE <database> TO keepsake_owner;
```

- `keepsake_app` MUST exist before the first migration. The migration grants it
  access only if it exists, and it never runs again for revisions already applied.
- The two roles MUST be different. The server refuses to start as a superuser,
  as a `BYPASSRLS` role, or as the owner of the schema or its tables.
- `keepsake_owner` SHOULD NOT be your application's migration role or a superuser. The
  chart stores its DSN in a Secret, and keepsake's migrations run with its rights.

## 2. Create the secrets

```bash
openssl rand -hex 32 | kubectl create secret generic keepsake-jwt --from-file=secrets=/dev/stdin
openssl rand -hex 32 | kubectl create secret generic keepsake-admin --from-file=password=/dev/stdin
```

`keepsake-jwt` signs and verifies tokens. Your orchestrator needs the same
value, and nothing an LLM drives may read it. `keepsake-admin` is the console
password. A GitOps install MUST set it, or the password changes on every sync.

## 3. Install the chart

```yaml
# values.yaml. It holds passwords, so do not commit it.
auth:
  mode: jwt
  jwt:
    issuer: <your token issuer>
    existingSecret: keepsake-jwt
admin:
  existingSecret: keepsake-admin
postgres:
  mode: existing
  dsn: postgres://keepsake_app:<password>@<host>:5432/<database>?sslmode=require
  ownerDsn: postgres://keepsake_owner:<password>@<host>:5432/<database>?sslmode=require
```

```bash
helm install keepsake oci://ghcr.io/roee-fs/charts/keepsake --version 0.4.0 \
  -f values.yaml --wait
```

A pre-install hook runs the `keepsake-migrate` Job as `keepsake_owner`. The server
then starts as `keepsake_app` and checks row-level security before it serves. If a
pod crash-loops, `kubectl logs` names the misconfiguration.

## 4. Check tenant isolation

```bash
kubectl port-forward svc/keepsake 8000:8000 &
token=$(kubectl exec deploy/keepsake -- keepsake token --tenant <uuid> --sub process:smoke --ttl 5m)
curl -s localhost:8000/mcp -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"okf_list","arguments":{}}}'
```

A request without a token MUST get a 401. A token for another tenant MUST NOT
list this tenant's concepts.

## 5. Connect your callers

- Your orchestrator signs a short-lived token per call. See
  [Tenancy and authority](../README.md#tenancy-and-authority) for the claims.
- Keep the Service `ClusterIP`. Allow only your callers to reach port 8000, for
  example with a NetworkPolicy or a mesh authorization policy.
- Scrape `/metrics` on port 9090 from the pods, not the Service.

## 6. Before production

- Set up backups. The chart configures none. See
  [`operations.md`](operations.md#backups).
- Read the **Upgrading** notes in [`CHANGELOG.md`](../CHANGELOG.md) before each
  upgrade. `helm rollback` does not revert a migration.
