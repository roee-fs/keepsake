# Alternatives

Keepsake is for many agents sharing memory, often across many tenants. If one
developer runs one agent against one repo, a git-backed tool is simpler and you
SHOULD use it.

| | Keepsake | Git-backed OKF (okf-agent-memory) | Mem0, Zep, Graphiti |
|---|---|---|---|
| Storage | Postgres, one row per concept | Markdown in a repo | Vector store or graph |
| Transport | MCP over streamable HTTP | MCP over stdio, CLI | SDK or HTTP API |
| Concurrent writers | Compare-and-swap per concept | One head per vault, retried on conflict | Varies by product |
| Tenancy | Postgres row-level security. The agent never names a tenant. | One vault per key | A caller-supplied id, filtered on |
| Format | OKF on import and export | OKF on disk | Proprietary |
| Search | BM25 in Postgres | BM25 in memory | Embeddings |
| Install | Helm chart, or Docker Compose to try it | One binary | Hosted or self-run service |

## Git-backed OKF

[okf-agent-memory](https://github.com/okf-memory/okf-agent-memory) keeps an OKF
bundle in the repo and adds an encrypted sync hub. It is the better choice for
a single developer. It installs with Homebrew, and a reviewer reads memory
changes in `git diff`.

A fleet hits two limits:

- Every write rewrites `index.md` and `log.md`. Two agents writing unrelated
  concepts still touch the same files.
- The sync hub advances one head per vault with compare-and-swap. A writer that
  loses the race fetches, merges and retries. On a real conflict it leaves
  conflict files for a human.

Keepsake computes `index.md` and `log.md` at export and never stores them.
Agents writing different concepts never contend.

## Mem0, Zep and Graphiti

These extract facts from conversations and retrieve them by embedding. They
isolate tenants by filtering on an id the caller supplies. One wrong id writes
into another tenant's memory. Keepsake sets the tenant from the server's auth
and lets Postgres enforce it.

Keepsake has no semantic search yet. The roadmap has pgvector with reciprocal
rank fusion. `bench/benchmarks.pdf` measures the lexical ranker.
