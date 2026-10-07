# MCP tools

Keepsake serves MCP over streamable HTTP at `/mcp`. Every call acts on one tenant.
In `none` mode that tenant is `auth.fixedTenantId`. In `jwt` mode the bearer
token's `tctx.tenant` claim picks it. An agent never names a tenant.

The server also sends `instructions` for the agent's system prompt: search before
answering, follow links, prefer the more specific memory, and update rather than
duplicate. They are in `internal/server/schemas.go`.

A path is relative, with no `.md` suffix: `runbooks/db-failover`.

| Tool | Arguments | Returns |
|---|---|---|
| `list` | `prefix?` | Every path under `prefix`, with a count per type. The cheapest way to learn the shape of the tree. |
| `search` | `query`, `limit`, `prefix?` | Cards ranked by BM25 alone. Each card holds `path`, `type`, `title`, `description`, `score`, `snippet`, and the OKF signals `status`, `stale`, `trust`, `generated_at`, which label the card but do not change its rank. `snippet` holds the body's passages that match the query, or is empty when only the title or description matched. |
| `grep` | `pattern`, `limit` | Paths whose title, description or body match a POSIX regex, case-insensitively, each with a snippet. |
| `read` | `path` | The full concept: body, frontmatter, version, outbound links, backlinks and the OKF signals. `null` if the path is empty. |
| `create` | `path`, `type`, `title?`, `description?`, `body?`, `frontmatter?` | The new version. Fails if the path is taken. |
| `update` | `path`, `expected_version?`, and any field `create` takes | The new version. Omitted fields keep their values. |
| `relate` | `from_path`, `to_path` | Appends a link from one concept to the other. |

## Search

Matching is lexical. Terms are OR-ed, so each extra term broadens the result.
Distinctive keywords find more than a question does. `limit` is required, at most
200. Use `grep` for an exact identifier such as
`purge_tenant`, because search splits it at the `_`.

## Links

Links are read out of `body`. Nothing declares them separately. Write them as
markdown links to the target's bundle path: `[failover](/runbooks/db-failover.md)`.
`relate` appends such a link for you.

## Concurrent writes

`update` with `expected_version` is a compare-and-swap. If anything was
written since that version, nothing changes. The response carries the current
version and body to merge against. Writes to different concepts never contend.

A version is an opaque token from one sequence. It only grows, never repeats,
and skips numbers. A path deleted and created again gets a new version, so a
stale `expected_version` never matches it.

Every write that changes a concept appends a revision, attributed to the
caller: the JWT `sub`, or the fixed actor in `none` mode. `create` and
`update` also stamp frontmatter `generated: { by, at }` with that caller and
the time, replacing any `generated` the caller sent. `relate` keeps the
existing stamp. An update that changes nothing writes nothing and returns the
current version, even when `expected_version` is stale.

## OKF frontmatter

`frontmatter` MAY carry the OKF v0.2 families: `status`, `stale_after`,
`sources`, `usage_window`, and on an `Attested Computation` the contract fields
`runtime`, `parameters`, `computation`, `executor` and `attester`. A write that
changes one of these MUST follow OKF §5 and §10, or it is refused. Timestamps
MUST carry an offset, such as `2026-06-30T14:00:00Z`. A field that was already
stored is not rechecked, so an imported concept stays editable.

## Errors

An argument that fails the advertised schema comes back as a tool error the
agent can correct, not a transport failure. A dropped database connection
comes back as a retryable tool error.
