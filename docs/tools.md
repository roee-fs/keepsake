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
| `okf_list` | `prefix?` | Every path under `prefix`, with a count per type. The cheapest way to learn the shape of the tree. |
| `okf_search` | `query`, `limit`, `prefix?` | Cards ranked by BM25: `path`, `type`, `title`, `description`, `score`. Never a body. |
| `okf_grep` | `pattern`, `limit` | Paths whose title, description or body match a POSIX regex, case-insensitively, each with a snippet. |
| `okf_read` | `path` | The full concept: body, frontmatter, version, outbound links and backlinks. `null` if the path is empty. |
| `okf_create` | `path`, `type`, `title?`, `description?`, `body?`, `frontmatter?` | The new version. Fails if the path is taken. |
| `okf_update` | `path`, `expected_version?`, and any field `okf_create` takes | The new version. Omitted fields keep their values. |
| `okf_relate` | `from_path`, `to_path` | Appends a link from one concept to the other. |

## Search

Matching is lexical. Terms are OR-ed, so each extra term broadens the result.
Distinctive keywords find more than a question does. `limit` is required, at most
200. Use `okf_grep` for an exact identifier such as
`purge_tenant`, because search splits it at the `_`.

A concept with `status: deprecated` in its frontmatter scores 0.3 times its BM25.
One whose `stale_after` date has passed scores 0.6 times. Its card carries
`status: "deprecated"` or `"stale"`. A current concept's card has no `status`.

To retire a concept, `okf_update` it with `status: deprecated` added to its
frontmatter and a link to its replacement. `frontmatter` is replaced whole, so
pass every key `okf_read` returned.

## Links

Links are read out of `body`. Nothing declares them separately. Write them as
markdown links to the target's bundle path: `[failover](/runbooks/db-failover.md)`.
`okf_relate` appends such a link for you.

## Concurrent writes

`okf_update` with `expected_version` is a compare-and-swap. If anything was
written since that version, nothing changes. The response carries the current
version and body to merge against. Writes to different concepts never contend.

Every write appends a revision, attributed to the caller: the JWT `sub`, or
the fixed actor in `none` mode.

## Provenance and trust

Each create, and each update that changes the title, description or body, sets
`generated: {by: <caller>, at: <now>}`. A caller's own `generated` is
overwritten.

`verified` records a human review. No tool can add or change it. A call that
tries gets a tool error. Passing it back unchanged, as `okf_read` returned it,
is allowed. An update that changes the text drops it, and the response carries
`"unverified": true`. `okf_relate` keeps it, because a new link doesn't change
what was reviewed. A human sets `verified` in the bundle and runs `keepsake import`.

## Errors

An argument that fails the advertised schema comes back as a tool error the
agent can correct, not a transport failure. A dropped database connection
comes back as a retryable tool error.
