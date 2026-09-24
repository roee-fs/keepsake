# Alternatives

Agent memory products fall into four groups. Keepsake targets a gap between
them: memory in an open format, run on your own Kubernetes, shared by a fleet
of agents across many tenants.

Keepsake meets four requirements together:

- **Cloud-native.** A Helm chart, several replicas, and Postgres underneath.
- **Concurrent reads and writes.** Compare-and-swap per concept, so agents
  writing different concepts never contend.
- **Multi-tenant.** Postgres row-level security enforces the tenant. The agent
  never names it.
- **Visible to operators.** A console shows what agents stored.

If you don't need all four, one of the groups below is probably simpler.

| Group | Examples | Choose it when | Trade-off against keepsake |
|---|---|---|---|
| Model providers | Claude memory tool, OpenAI Agents SDK sessions, Amazon Bedrock AgentCore Memory, Databricks Lakebase | You are committed to one provider's platform | Memory lives in that platform's model and API. Moving it means rebuilding it. |
| Memory products | Mem0, Zep and Graphiti, Letta, Hindsight, OpenViking, Memori | You want facts extracted from conversations automatically, or embedding search today | Each defines its own memory model and storage format. Mem0, Zep and Graphiti isolate tenants by filtering on an id the caller passes. |
| Databases with memory features | SurrealDB, Redis, MongoDB, Typesense | You already run the database and want to design memory yourself | You build the memory model, tools, tenancy and review UI. |
| Custom and local | Cloudflare Agent Memory, okf-agent-memory | One developer, one repo, or one edge platform | They don't serve a fleet across tenants from one shared deployment. |

## Model providers

The big model providers each ship a memory feature for their own agent stack.
They are the fastest path if your agents already live there. The memory is
shaped by that provider's API, so a second provider, or leaving the first,
means migrating it. Keepsake stores OKF, markdown with YAML frontmatter, and
`keepsake export` writes it back out as a plain bundle.

## Memory products

These products extract memories from conversations and retrieve them by
embedding or graph traversal. Keepsake does neither yet. Agents write concepts
explicitly through MCP, and search is BM25. The roadmap has pgvector with
reciprocal rank fusion, and `bench/benchmarks.pdf` measures the current ranker.

Tenancy is where they differ most. Mem0 takes a caller-supplied `user_id`, and
Zep and Graphiti take a `group_id`. Both filter on it, so one wrong id reads or
writes another tenant's memory. Keepsake takes the tenant from the server's auth
and lets Postgres enforce it.

## Databases with memory features

General-purpose databases now market memory features: vector search, document
storage and caching. They are building blocks. The memory model, the agent
tools, tenant isolation and a review UI are still yours to build. Keepsake is
that layer, built on Postgres.

## Custom and local

[okf-agent-memory](https://github.com/okf-memory/okf-agent-memory) keeps an OKF
bundle in a git repo, with an optional encrypted sync hub. It is the better
choice for a single developer. It installs with Homebrew, and a reviewer reads
memory changes in `git diff`. A fleet hits two limits:

- Every write rewrites `index.md` and `log.md`. Two agents writing unrelated
  concepts still touch the same files.
- The sync hub advances one head per vault with compare-and-swap. A writer that
  loses the race fetches, merges and retries. On a real conflict it leaves
  conflict files for a human.

Keepsake computes `index.md` and `log.md` at export and never stores them.

Platform-specific memory, such as Cloudflare Agent Memory, fits agents that
already run on that platform.
