# 🧠 Keepsake

A Kubernetes-native memory substrate for fleets of agents.

![demo: install, import, an agent edits over MCP, export, diff](https://github.com/roee-fs/keepsake/blob/pr-assets/demo.gif?raw=true)

## Try it

What one agent learns, the next one knows. One command shows it:

```bash
git clone https://github.com/roee-fs/keepsake && cd keepsake && ./demo/run.sh
```

It needs Docker, [kind](https://kind.sigs.k8s.io/), kubectl, Helm, jq and a
logged-in [Claude Code](https://docs.claude.com/en/docs/claude-code/setup).
It installs the chart on a throwaway kind cluster and imports
an on-call team's knowledge base. Agent A asks who to page before a Postgres
failover and gets a stale answer. Agent B, which just ran a failover drill, records
what it learned. Agent A asks again in a new session and gets the right answer and
the reason. The agents are real Claude sessions whose only memory is keepsake, over
MCP. Then it deletes the cluster.

Add `--keep` to leave the cluster up and point your own agent at it.

## Run it

On your machine, with Docker only:

```bash
docker compose up -d
claude mcp add --transport http keepsake http://localhost:8000/mcp
```

The console is at `http://localhost:8000`, password `keepsake`. Load a bundle with
`docker compose run --rm -v "$PWD/demo/bundle:/bundle:ro" keepsake keepsake import /bundle`.
This setup is for trying keepsake out. It serves one tenant to anything that
reaches the port.

On Kubernetes:

```bash
helm install keepsake oci://ghcr.io/roee-fs/charts/keepsake --version 0.4.0
```

[`docs/operations.md`](docs/operations.md) covers Postgres modes, roles,
upgrades and backups.

## Deploy it

These steps install keepsake into a Postgres you already run (`existing` mode)
and serve many tenants (`jwt` mode). Keepsake creates its own schema, `okf`, and
touches nothing else in the database.

### 1. Create the database roles

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

### 2. Create the secrets

```bash
openssl rand -hex 32 | kubectl create secret generic keepsake-jwt --from-file=secrets=/dev/stdin
openssl rand -hex 32 | kubectl create secret generic keepsake-admin --from-file=password=/dev/stdin
```

`keepsake-jwt` signs and verifies tokens. Your orchestrator needs the same
value, and nothing an LLM drives may read it. `keepsake-admin` is the console
password. A GitOps install MUST set it, or the password changes on every sync.

### 3. Install the chart

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

### 4. Check tenant isolation

```bash
kubectl port-forward svc/keepsake 8000:8000 &
token=$(kubectl exec deploy/keepsake -- keepsake token --tenant <uuid> --sub process:smoke --ttl 5m)
curl -s localhost:8000/mcp -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"okf_list","arguments":{}}}'
```

A request without a token MUST get a 401. A token for another tenant MUST NOT
list this tenant's concepts.

### 5. Connect your callers

- Your orchestrator signs a short-lived token per call. See
  [Serving many tenants](#serving-many-tenants) for the claims.
- Keep the Service `ClusterIP`. Allow only your callers to reach port 8000, for
  example with a NetworkPolicy or a mesh authorization policy.
- Scrape `/metrics` on port 9090 from the pods, not the Service.

### 6. Before production

- Set up backups. The chart configures none. See
  [`docs/operations.md`](docs/operations.md#backups).
- Read the **Upgrading** notes in [`CHANGELOG.md`](CHANGELOG.md) before each
  upgrade. `helm rollback` does not revert a migration.

## Why

Agents lose everything when the context window closes. Keepsake gives them a
durable, shared knowledge base they read and write over MCP — stored in
Postgres, isolated per tenant by the database itself, and importable and
exportable as a plain [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
(OKF) bundle of markdown files.

```
agent ──MCP──> keepsake ──> Postgres (RLS)
                   │
                   └── keepsake import / export ──> markdown bundle
```

## What it is

- **An MCP server.** Seven bounded tools: `okf_list`, `okf_search`, `okf_grep`,
  `okf_read`, `okf_create`, `okf_update`, `okf_relate`. Streamable HTTP, so a
  fleet of pods shares one endpoint.
- **A Postgres schema.** One row per concept, keyed `(tenant_id, path)`.
  Optimistic concurrency via an optional `expected_version`, so two agents
  editing the same concept across an LLM call cannot silently clobber each
  other — and agents editing *different* concepts never contend at all.
- **A Helm chart.** Install with a managed CloudNativePG cluster, or point it at
  a Postgres you already run and it installs into its own schema.
- **An OKF bundle on the way in and out.** The format is the interchange layer,
  not the storage layer. `keepsake export` regenerates `index.md` and `log.md` at
  export time; they are never stored, so nothing derived can drift.

## How it runs

![keepsake on Kubernetes](assets/architecture.png)

Agents call the Service over MCP, and operators use the console through the
same Service. A Helm hook runs the migration Job before every install and
upgrade.

## Versioning

Every concept carries a `version`. Every write gives it a new one, drawn from a
single sequence that no write ever reuses. A version is a token, not a count:
it only ever grows, and gaps between versions are normal.

Every write also appends a revision: who wrote what, and when. Deletes are
revisions too, so the log is append-only and a deleted concept's history stays
readable in the console.

To edit safely, an agent reads a concept and passes its `version` back as
`expected_version`. If anyone wrote the concept since, including by deleting and
re-creating it, the write changes nothing. The agent gets the current version and
body to merge against.

An import or upload skips a concept whose content is unchanged, so pushing the
same bundle twice creates no revisions and bumps no versions.

## How it differs

**Tenancy is enforced, not filtered.** This is the real gap in the market. Mem0
takes a caller-supplied `user_id`, Zep and Graphiti take a `group_id`, and both
*filter* on it — a single mis-set id poisons a namespace permanently. The ones
that take isolation seriously escape to physical separation (a database per
tenant, a graph per tenant). Keepsake sets a Postgres GUC inside the request
transaction and lets row-level security do the enforcing. An agent never names a
tenant, so it cannot name the wrong one. The server refuses to start if it is
connected as a superuser or as the schema owner, because both silently bypass
RLS.

**No lock-in.** MIT, no hosted tier, no registry, no required runtime. Your
knowledge is markdown; `keepsake export` hands it back byte-for-byte.

## Looking at what agents stored

The chart serves a read-only admin console from the same pod and port as `/mcp`
— one image, no second Service, no CORS. Helm generates the password at
install, into a `<release>-admin` Secret:

```bash
kubectl get secret <release>-admin -o jsonpath='{.data.password}' | base64 -d
kubectl port-forward svc/<release> 8000:8000
```

Then log in at `http://localhost:8000` with that password.

![Console overview](https://github.com/roee-fs/keepsake/blob/pr-assets/overview-one-tenant.png?raw=true)

## Serving many tenants

By default a release serves one tenant, `auth.fixedTenantId`, to anything that
reaches it. `auth.mode: jwt` serves many: each `/mcp` request carries an HS256
bearer token, and its `tctx.tenant` claim picks the tenant.

```bash
openssl rand -hex 32 | kubectl create secret generic keepsake-jwt --from-file=secrets=/dev/stdin
helm install keepsake charts/keepsake --set auth.mode=jwt \
  --set auth.jwt.issuer=platform --set auth.jwt.existingSecret=keepsake-jwt
```

Your orchestrator holds the same secret and signs a short-lived token per call.
The HMAC key is the line's text as-is, not its hex-decoded bytes:
`{"iss": "platform", "aud": "keepsake", "sub": "support-agent/1.4", "tctx": {"tenant": "<uuid>"}, "exp": …}`.
keepsake records `sub` as the writer, and stamps it as OKF `generated.by` on every
concept the token writes. It SHOULD follow the OKF actor convention, and MUST be
`human:<id>` for a person.
The secret MUST NOT be readable by anything an LLM drives. A client that can run
code, or that only takes a static header, gets a token for its own tenant instead:

```bash
kubectl exec deploy/keepsake -- keepsake token --tenant <uuid> --sub human:alice --ttl 720h
```

## Roadmap

**v1 — the substrate**

- [x] `okf`: OKF parse/serialize with round-trip fidelity
- [x] Link extraction and per-write validation
- [x] Schema migration: concepts, revisions, RLS policies, tenant purge
- [x] Tenant-scoped connection handling
- [x] Writes with optional compare-and-swap and an append-only revision log
- [x] Read paths: read, list, search, grep, backlinks
- [x] Startup verification that refuses a privileged database role
- [x] MCP server over streamable HTTP
- [x] CLI: import, export, validate, migrate, serve
- [x] Helm chart with managed and existing Postgres modes
- [x] kind end-to-end suite
- [x] Tool arguments validated against their advertised schemas, so a bad one is
      something the agent can correct rather than a transport failure
- [x] Concurrent writes served in parallel, with readiness that follows the
      database rather than the bound port

**Next**

- [x] `jwt` auth mode: one release serves many tenants, each request's tenant
      taken from a signed token
- [ ] OIDC and Kubernetes ServiceAccount tokens for `jwt` mode, verified against
      the issuer's published keys
- [x] A review UI over what agents believe — the thing `git diff` gave OKF for
      free and every hosted memory product dropped
- [ ] Semantic search (pgvector + reciprocal rank fusion over the lexical index)
- [ ] Revision retention and pruning policy
- [ ] Per-tenant rate limits
- [ ] Deduplication, contradiction resolution, staleness lifecycle

## Status

Pre-release. Nothing here is stable yet.

## Docs

- [`docs/tools.md`](docs/tools.md) — the MCP tools, their arguments and results
- [`docs/operations.md`](docs/operations.md) — install, roles, upgrades, backups
- [`docs/alternatives.md`](docs/alternatives.md) — when to use something else
- [`CHANGELOG.md`](CHANGELOG.md) — what changed, newest first
- [`bench/benchmarks.pdf`](bench/benchmarks.pdf) — search quality against speed, measured

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) has the setup, the checks CI runs, and what
a good change looks like. `go test ./...` is the whole loop; the tests bring up
PostgreSQL in a container themselves.

Found something touching tenant isolation? [`SECURITY.md`](SECURITY.md) — report
it privately, not as an issue.

Participation is covered by the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

MIT — see [`LICENSE`](LICENSE).
