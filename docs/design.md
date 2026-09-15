# keepsake — Design

Stores [OKF v0.2](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md)
concepts in Postgres and serves them to agents as MCP tools. Concurrent writers
are safe because Postgres is. Tenant isolation is enforced by row-level security
rather than by a filter the caller supplies.

Postgres is the only store. There is no materialised bundle, no mount, and no
second read path.

## Why this exists

Every OKF implementation today is local-first, single-user, and git-backed. Git
cannot serve concurrent writers: each write rewrites `index.md` and `log.md`, so
agents touching unrelated concepts still collide, and commits contend on
`index.lock`.

No agent-memory product *enforces* tenant isolation. Mem0 scopes by a
caller-supplied `user_id`, Zep and Graphiti by a caller-supplied `group_id`. A
single mis-set identifier poisons the namespace permanently. Here the scope is
not expressible by the caller.

## Non-goals

- **Not competing on retrieval quality.** Mem0, Letta, and Zep own semantic
  retrieval and automatic fact extraction. We MUST NOT chase them.
- **No semantic search in v1.** Ranking is lexical. Vocabulary mismatch is a
  known, accepted limit. See "Retrieval".
- **Not solving deduplication, contradiction, or staleness.** All three are real.
  All three are out of v1.
- **No caller-supplied scope identifiers**, in any mode, ever.
- **Not a fork of OKF.** Conform to the spec. Extend nowhere.

## Architecture

```
agents ──MCP over HTTP──► keepsake (stateless Deployment)
                               │
                               ├─ okf_core: parse, validate, link graph, indexes
                               └─ store:    psycopg → Postgres
```

The server holds no state. All persistence is Postgres, so replicas scale
horizontally and a pod restart loses nothing.

`okf_core` MUST NOT import the store or the server. It is a pure library over
bytes and dataclasses, publishable on its own so other Python consumers can reuse
OKF parsing without running the server.

## Surfaces

| Surface | For |
|---|---|
| MCP tools over HTTP | Agents. The only runtime path. |
| HTTP API | The MCP layer's backend; also direct integration. |
| `okf` CLI | Operators — import, export, validate, migrate. Not an agent path. |

## Tool surface

Four reads, three writes. Every read is bounded by construction.

```
okf_list(prefix)                      → children plus per-type counts
okf_search(query, limit, prefix)      → ranked pointers: path, type, title,
                                        description, score. Never a body.
okf_grep(pattern, limit)              → exact-string matches: path plus snippet
okf_read(path)                        → concept, version, links, backlinks

okf_create(path, type, title, description, body, frontmatter, links)
okf_update(path, ..., expected_version=None)
okf_relate(from_path, to_path)
```

`limit` is REQUIRED on `okf_search` and `okf_grep`. There is no shell here to
supply `| head`, so bounding MUST live in the signature.

`okf_list` returns counts rather than a full tree. Listing every path at ten
thousand concepts costs roughly 97k tokens, measured — more than most context
budgets, before a single concept is read.

`okf_search` and `okf_grep` are different tools for different jobs. Search is
ranked and stemmed, for "what do we know about X". Grep is literal, for an exact
CVE id, IP, or rule name. Grep is *not* the better retrieval tool: it has no
stemming and no ranking.

Tool descriptions MUST state that search matching is literal and that distinctive
keywords beat questions.

## Data model

Two tables, both in a configurable schema (default `okf`).

```sql
concept
  tenant_id    uuid        NOT NULL          -- no FK; see "Tenant lifecycle"
  path         text        NOT NULL          -- 'architecture/layers'; the OKF concept id
  type         text        NOT NULL          -- OKF's one required frontmatter field
  title        text
  description  text
  body         text        NOT NULL DEFAULT ''
  frontmatter  jsonb       NOT NULL DEFAULT '{}'   -- unknown fields preserved verbatim
  links        text[]      NOT NULL DEFAULT '{}'   -- outbound edges
  search       tsvector    GENERATED ALWAYS AS (...) STORED   -- see "Retrieval"
  version      int         NOT NULL DEFAULT 1
  updated_by   text
  created_at, updated_at  timestamptz NOT NULL
  PRIMARY KEY (tenant_id, path)

concept_revision
  id           uuid        PK
  tenant_id    uuid        NOT NULL
  path         text        NOT NULL
  version      int         NOT NULL
  op           text        NOT NULL          -- create | update | delete
  snapshot     jsonb       NOT NULL          -- the full concept at this version
  updated_by   text
  created_at   timestamptz NOT NULL
  UNIQUE (tenant_id, path, version)
```

Three properties carry the design:

**Path is the identity.** `PRIMARY KEY (tenant_id, path)` matches OKF, where the
file path is the concept id. A duplicate path is a constraint violation, so
"search before write" stops being a convention an agent might forget.

**Derived state lives in the same row or is computed at read.** `search` is
generated and cannot drift from `body`. `links` is a column, so edges commit
atomically with the concept owning them. Nothing derived is written to a row
another writer must also touch — precisely the mechanism that breaks git-backed
OKF under concurrency.

**`tenant_id` is NOT NULL.** There is no shared-row branch, so an unset scope
reads nothing rather than everything.

### Indexes

- GIN on `search` — ranked discovery.
- GIN on `links` — **does not serve backlinks, and cannot.** Only
  `links @> ARRAY[:path]` has GIN operator-class support (`:path = ANY(links)`
  has none), and `arraycontains` is not leakproof, so under `FORCE ROW LEVEL
  SECURITY` Postgres refuses to push it below the policy's security qual and no
  index condition is available in either form. Backlinks therefore scan the
  tenant's own rows. Measured on postgres 17 at 50k rows: the same index on the
  same data without RLS plans a Bitmap Index Scan (0.04ms); with RLS both forms
  plan a Seq Scan, `= ANY` at 2.5ms and `@>` at 10.1ms. The query uses `= ANY`
  for that reason. Fixing this needs a leakproof predicate, not a different
  index.
- The primary key serves prefix scans on `path`.

### Revision retention

`concept_revision` stores a full snapshot per write and grows without bound. At
ten thousand concepts with twenty revisions each it exceeds the live corpus.

Retention is configurable: keep the last N versions per concept, or M days,
whichever the operator sets. A prune runs on write. This MUST be decided while
the table is empty.

## Retrieval

### v1: tsvector, OR semantics, `ts_rank_cd`

```sql
search tsvector GENERATED ALWAYS AS (
  setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
  setweight(to_tsvector('english', coalesce(description, '')), 'B') ||
  setweight(to_tsvector('english', coalesce(body, '')), 'C')
) STORED
```

The expression MUST use the two-argument `to_tsvector(regconfig, text)`. The
one-argument form depends on `default_text_search_config`, is only STABLE, and
Postgres rejects it in a generated column.

**Terms are OR-ed, never AND-ed.** Measured: with `websearch_to_tsquery`, which
ANDs, four of five realistic agent queries returned nothing — every additional
word made a match *less* likely, which is backwards for an agent phrasing a
question. OR plus ranking recovers them with no zero-result cliff, because
`ts_rank_cd` already ranks a document matching three terms above one matching
one. Ranking restores the precision AND was providing.

The query string MUST be tokenised server-side and re-joined with `|` before
reaching `to_tsquery`. Passing caller text to `to_tsquery` directly admits
tsquery *syntax* errors and operators, independently of SQL parameterisation.

### Known limit, accepted

Lexical matching does not survive vocabulary mismatch. "login credentials" will
not find a concept saying "authentication", and no lexical ranker fixes that —
BM25 included, since it scores documents containing your terms and cannot score
documents that do not. Measured, not assumed.

### Growth path

The contract — query string in, ranked pointers out — is what stays stable. No
abstraction is built for this; the contract *is* the growth mechanism, and
callers never see the algorithm or a raw `ts_rank_cd` value.

1. **v1** — tsvector, OR, `ts_rank_cd`.
2. **Typo tolerance** — add `pg_trgm`. No API change.
3. **Hybrid** — add `embedding halfvec(N)` with an HNSW index and fuse the two
   ranked lists with reciprocal rank fusion. No API change.

Rank fusion rather than weighted scores, because `ts_rank_cd` is unbounded and
cosine distance is `[0, 2]`; the scores are not comparable and normalising them
is corpus-dependent. RRF uses only positions.

Only the vector stage addresses vocabulary mismatch. That is the upgrade that
matters, not `ts_rank_cd` → BM25.

**Filtered-ANN warning for when hybrid lands:** RLS is a filter, so every vector
query in this system is a filtered vector query. At pgvector's default
`hnsw.ef_search = 40`, a predicate matching a small share of rows returns almost
nothing. `hnsw.iterative_scan = strict_order` (pgvector 0.8.0+) is the targeted
fix.

## Tenancy

RLS is the enforcement mechanism in every mode.

```sql
ALTER TABLE okf.concept ENABLE ROW LEVEL SECURITY;
ALTER TABLE okf.concept FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON okf.concept
  USING      (tenant_id = current_setting('okf.current_tenant')::uuid)
  WITH CHECK (tenant_id = current_setting('okf.current_tenant')::uuid);
```

Four requirements, each load-bearing:

1. **`WITH CHECK` is mandatory.** `USING` governs visibility of existing rows;
   `WITH CHECK` governs values of rows written. A policy with only `USING` blocks
   cross-tenant reads while permitting a cross-tenant *insert*.
2. **`FORCE` is mandatory**, because a table's owner bypasses RLS by default.
3. **The GUC is `okf.current_tenant`.** It MUST be namespaced; a generic
   `app.current_tenant` collides with the host application in schema mode.
4. **`SET LOCAL`, never `SET`.** A session-scoped setting on a pooled connection
   is inherited by the next request. This also keeps the server correct behind
   PgBouncer in transaction mode.

Use the one-argument `current_setting`. It raises when unset. The two-argument
form returns NULL and yields an empty result — also fail-closed, but silently.

### Tenant lifecycle

There is **no `tenant` table and no foreign key**, in either install mode. In
schema mode the host application already owns tenant identity, and a foreign key
across that boundary is a coupling to regret.

Deletion is a function both modes call:

```sql
okf.purge_tenant(uuid)
```

An explicit purge beats a cascade for erasure obligations: it is auditable and
can be logged.

## Auth modes

The mode determines only how the server *learns* the tenant id. Enforcement is
identical in all three.

| Mode | Source of tenant id |
|---|---|
| `none` (v1) | A single fixed id from config |
| `proxy` | A configurable trusted header, populated by an authenticating proxy |
| `token` | Opaque bearer token, SHA-256 hashed, resolved via an `okf.api_token` table |

v1 ships `none`. `tenant_id` and the RLS policies exist from day one, so adding a
mode is an auth change rather than a schema migration.

When `token` lands, tokens MUST be hashed with SHA-256, not bcrypt or argon2.
Those are password hashes, deliberately slow to resist dictionary attacks against
low-entropy input. A 256-bit random token has no dictionary.

## Write path

`okf_create` fails when the path exists.

`okf_update` takes an **optional** `expected_version`. When supplied and stale,
it returns a conflict carrying the **current version and body** — not a bare
error code. When omitted, last write wins.

Optional rather than required, deliberately. The founding problem — unrelated
concurrent writes colliding — is solved by Postgres alone; compare-and-swap
addresses the rarer same-concept case. `concept_revision` is append-only, so a
clobbered update is recoverable rather than lost. Requiring version bookkeeping
on every write costs more than it returns.

When it *is* supplied, returning the current body is what makes it worthwhile: an
LLM merges prose well, and handing it both versions beats any automatic
resolution for the cost of one round trip. `okf_read` returns the live version,
so a caller that wants CAS always has a fresh one.

Two statements, not one upsert. A single
`INSERT ... ON CONFLICT DO UPDATE ... WHERE version = :expected` silently creates
a row when the caller believed it was updating one.

```sql
-- create: zero rows means the path is taken
INSERT INTO okf.concept (...) VALUES (...)
ON CONFLICT (tenant_id, path) DO NOTHING
RETURNING version;

-- update: zero rows means a version conflict or a missing row
UPDATE okf.concept SET ..., version = version + 1, updated_at = now()
WHERE tenant_id = current_setting('okf.current_tenant')::uuid
  AND path = :path AND (:expected IS NULL OR version = :expected)
RETURNING version;
```

`concept_revision` is inserted in the same transaction.

Backlinks are never written; they are computed at read from `links`.

## Validation

All validation is server-side, in `okf_core`, on the write path.

Enforced per write:

- `type` is present and non-empty (OKF's one required field).
- `path` is well-formed and collision-free.
- Frontmatter parses, and unknown fields round-trip into `frontmatter` jsonb.

Enforced on demand by `okf validate`, not per write:

- Link-graph resolvability. Checking every outbound link on every write means a
  cross-row query per mutation, which invites lock contention for a property
  legitimately violated midway through authoring a set of related concepts.

## OKF conformance

Import and export are CLI operations. They are not in the runtime path and no
agent calls them.

- `okf import <dir>` — path derived from file path, unknown frontmatter into
  `jsonb`.
- `okf export <dir>` — generates `index.md` and `log.md` at export time, which is
  the correct place to materialise derived files. `log.md` renders from
  `concept_revision`.

Export is the portability guarantee: a bundle can be `git clone`d, `cat`ed, and
read by any other OKF tool. It is also how an operator inspects the corpus with
ordinary shell tools.

The conformance gate is a round-trip test: import, export, byte-compare, modulo
the generated index and log. `ruamel.yaml` is used specifically because it
preserves key order and formatting across the round trip.

### Fidelity ceilings

Four inputs are known not to survive byte-identically. Each is pinned by a test
rather than kept out of the fixtures:

- **CRLF line endings** are normalised to LF.
- **Comments in frontmatter** are dropped.
- **Unquoted YAML dates** (`2026-01-01`) come back as strings, because `jsonb`
  has no date scalar.
- **Block-style leaf collections** written by hand come back flow-style: a
  hand-written multi-line `tags:` list re-emits as `tags: [a, b]`, because
  frontmatter read from `jsonb` carries no style metadata.

## Install modes

```yaml
postgres:
  mode: managed        # managed | existing
  schema: okf
  dsn: ""              # required when mode = existing
```

- **`managed`** — the chart provisions a CloudNativePG `Cluster`, creates roles,
  runs migrations.
- **`existing`** — points at a DSN and creates only its schema, inheriting the
  host's backups, PITR, and operational tooling.

Schema mode imposes requirements the code MUST satisfy:

- Every object is schema-qualified. Connections `SET search_path = okf,
  pg_catalog` explicitly. A mutable `search_path` is a privilege-escalation
  vector, and it matters more once a `SECURITY DEFINER` function exists.
- Two roles. `okf_owner` owns the schema and runs migrations. `okf_app` is what
  the server connects as, and it **MUST NOT be the owner and MUST NOT be a
  superuser**.
- No `CREATE EXTENSION` is assumed. Extensions are database-scoped and some
  require superuser. A future pgvector dependency MUST be detected, never
  assumed.
- The server owns its own connections in both modes. It MUST NOT share a pool
  with a host application, because `SET LOCAL` protects only within its own
  transaction.

## Startup verification

The server MUST refuse to start when any of these fail:

1. The connecting role is a superuser.
2. The connecting role owns the schema.
3. `relrowsecurity AND relforcerowsecurity` is false for any okf table.

Pointing okf at an existing database using the host's existing app role is the
obvious thing for an adopter to do, and it silently disables RLS. This check
converts the most likely catastrophic misconfiguration into a crash loop with a
clear message. It is roughly fifteen lines and it is the highest-value control in
the design.

## Testing

Four tests carry the isolation guarantee. The middle two are the ones nobody
writes:

1. Write as A, read as B → empty. Separately for `okf_read`, `okf_list`,
   `okf_search`, `okf_grep`, and backlinks. A policy can be correct for one query
   shape and wrong for another.
2. Insert a row carrying B's tenant id while scoped to A → rejected. Catches a
   missing `WITH CHECK`.
3. Two sequential requests as different tenants on the **same pooled
   connection** → the second sees only its own rows. Catches `SET` vs `SET LOCAL`.
4. `relrowsecurity AND relforcerowsecurity` asserted for every table with a
   `tenant_id` column. Catches a migration shipping a table without a policy.

Plus:

5. **Concurrency.** Two sessions read version 1 and both update with
   `expected_version=1`. Exactly one succeeds.
6. **Round-trip.** Import, export, byte-compare, including unknown-field
   preservation.
7. **Bounded reads.** Every read tool respects `limit` and never returns a body
   from `okf_search`.

Not tested: that Postgres MVCC works.

## Design constraints not covered by tests

Memory is a leak amplifier. Other systems leak transiently — a bad query returns
rows once. A memory system persists what it sees, and an agent may write observed
content back as a new concept, which RLS then faithfully protects as the wrong
tenant's data, permanently and searchably.

Therefore:

- There is no writable shared scope.
- Every concept carries provenance (`updated_by`, `concept_revision`).
- Error messages and logs MUST NOT carry concept bodies.

## Deferred

Semantic and hybrid search (pgvector plus rank fusion), `proxy` and `token` auth
modes, per-tenant rate limits and quotas, deduplication, contradiction
resolution, staleness lifecycle, and the review UI.
