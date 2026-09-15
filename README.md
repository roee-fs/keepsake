# 🧠 Keepsake

A Kubernetes-native memory substrate for fleets of agents.

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

## How it differs

**Every other OKF implementation is local-first, single-user, and git-backed.**
Git works beautifully for one developer and fails for a fleet: every write
rewrites `index.md` and `log.md`, so two agents touching entirely unrelated
concepts still collide on the same files, and `index.lock` contention plus
fetch/rebase loops do the rest. Keepsake keeps derived state in the row that
owns it, or computes it at read.

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

### Fidelity

Byte-for-byte has four known exceptions, each pinned by a test:

- CRLF line endings are normalised to LF.
- Comments in frontmatter are dropped.
- An unquoted YAML date (`2026-01-01`) comes back as a string.
- A hand-written block-style list re-emits flow-style (`tags: [a, b]`).

## Roadmap

**v1 — the substrate**

- [x] `okf_core`: OKF parse/serialize with round-trip fidelity
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

**Next**

- [ ] `proxy` and `token` auth modes (v1 ships `none`)
- [ ] A review UI over what agents believe — the thing `git diff` gave OKF for
      free and every hosted memory product dropped
- [ ] Semantic search (pgvector + reciprocal rank fusion over the lexical index)
- [ ] Revision retention and pruning policy
- [ ] Per-tenant rate limits
- [ ] Deduplication, contradiction resolution, staleness lifecycle

## Status

Pre-release. Nothing here is stable yet.

## Docs

- [`docs/design.md`](docs/design.md) — the specification
- [`docs/plans/`](docs/plans/) — implementation plans

## License

MIT
