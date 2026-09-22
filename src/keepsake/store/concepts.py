"""Concept persistence. Two statements, never one upsert: an upsert guarded by a
version predicate silently creates a row the caller believed it was updating.

Tables are named unqualified: `Store.scope` sets search_path to the validated
schema plus pg_catalog and nothing else, so a literal prefix would only break a
non-default KEEPSAKE_SCHEMA.
"""

import json
import re
from collections.abc import Iterator, Sequence
from contextlib import contextmanager
from dataclasses import asdict, dataclass
from datetime import date, datetime
from functools import partial
from typing import Any
from uuid import UUID

import psycopg
from psycopg.types.json import Jsonb

from keepsake.store.pool import Store
from okf_core import Concept

# Every column a write sets and a read returns, in `Concept`'s own field order. The
# INSERT column list, its placeholder run, the UPDATE SET clause and the SELECT list
# are all derived from this, so a seventh field cannot reach one statement and not
# another.
_FIELDS = ("type", "title", "description", "body", "frontmatter", "links")
_READ_COLS = ", ".join(("path", *_FIELDS, "version"))

# Not derived from _FIELDS: Summary is its own fixed shape, and _FIELDS' order is
# free to change for round-trip reasons.
_SUMMARY_COLS = "path, type, title, description, version, updated_at, tenant_id"

# Shared so `count()` and `page()` cannot drift apart. starts_with, not LIKE: an
# underscore is legal in a path and a wildcard in a pattern.
_STARTS_WITH = "starts_with(path, %s)"

# Anything outside a word is dropped rather than escaped, which is what keeps caller
# text from reaching to_tsquery as operators.
_WORD = re.compile(r"\w+")

# A grep snippet is a card, not a body: enough context to judge relevance, no more.
_SNIPPET_LEAD = 60
_SNIPPET_LEN = 200

# A caller-supplied regex drives an unindexed sequential scan on a thread the whole
# pod shares, so one pathological pattern would otherwise stall every sibling agent.
_GREP_TIMEOUT_MS = 5000

# YAML loads an unquoted 2026-01-01 as a date, which json rejects. It survives as a
# string, so a document with one no longer round-trips byte-identically.
_dumps = partial(json.dumps, default=str)


def _jsonb(obj: Any) -> Jsonb:
    return Jsonb(obj, dumps=_dumps)


@dataclass(frozen=True, slots=True)
class Conflict:
    """A stale expected_version. `current_body` is the text to merge against."""

    current_version: int
    current_body: str


@dataclass(frozen=True, slots=True)
class Revision:
    """One entry in the revision log. Carries no snapshot: that holds the body."""

    path: str
    version: int
    op: str
    updated_by: str
    created_at: datetime
    tenant_id: UUID


@dataclass(frozen=True, slots=True)
class Hit:
    """One search result. Carries no body: the agent searches, chooses, then reads."""

    path: str
    type: str
    title: str
    description: str
    score: float


@dataclass(frozen=True, slots=True)
class Summary:
    """One row in the admin console's concept table.

    `Hit` without the score, plus `tenant_id`: a `tenant_id=None` page mixes
    tenants, and the table is how an admin tells them apart.
    """

    path: str
    type: str
    title: str
    description: str
    version: int
    updated_at: datetime
    tenant_id: UUID


@dataclass(frozen=True, slots=True)
class Totals:
    concepts: int
    by_type: dict[str, int]
    revisions: int
    links: int
    orphans: int


# The match position, not the matched text: substring(body from pattern) returns the
# pattern back, which tells an agent nothing about relevance. One haystack rather than
# three columns keeps the snippet and the predicate from ever disagreeing.
_GREP = f"""
SELECT c.path,
       btrim(regexp_replace(
         substr(h.hay, greatest(m.pos - {_SNIPPET_LEAD}, 1), {_SNIPPET_LEN}),
         '\\s+', ' ', 'g'))
FROM concept AS c,
     LATERAL (SELECT concat_ws(chr(10), c.title, c.description, c.body)) AS h(hay),
     LATERAL (SELECT regexp_instr(h.hay, %s, 1, 1, 0, 'i')) AS m(pos)
WHERE m.pos > 0
ORDER BY c.path
LIMIT %s
"""


# Sequential within the tenant, and the GIN index on `links` cannot help:
# `links @> ARRAY[path]` is the only form the index serves, and arraycontains is not
# leakproof, so FORCE ROW LEVEL SECURITY keeps it out of the index condition. `= ANY`
# is the cheaper of the two filters. Written as a scalar subquery so one read can take
# the concept and its backlinks in a single statement.
_BACKLINKS = (
    "ARRAY(SELECT b.path FROM concept b WHERE %s = ANY(b.links) ORDER BY b.path)"
)


def _tsquery(query: str) -> str:
    """OR the terms. AND semantics returned nothing for realistic agent queries."""
    return " | ".join(_WORD.findall(query))


def _values(c: Concept) -> tuple[Any, ...]:
    """The bound parameters for `_FIELDS`, in that order."""
    return (
        c.type,
        c.title,
        c.description,
        c.body,
        _jsonb(c.frontmatter),
        list(c.links),
    )


def _row(row: Sequence[Any] | None) -> Sequence[Any]:
    """A bare aggregate's `fetchone()`, which always returns exactly one row."""
    assert row is not None
    return row


def _concept(row: Sequence[Any]) -> Concept:
    """A row selected as `_READ_COLS`, which is `Concept`'s own field order."""
    path, type_, title, description, body, frontmatter, links, version = row[:8]
    return Concept(
        path, type_, title, description, body, frontmatter, tuple(links), int(version)
    )


class ConceptStore:
    def __init__(self, store: Store) -> None:
        self._store = store

    def create(self, tenant_id: UUID, c: Concept, actor: str) -> int | None:
        """Insert a concept. None means the path is already taken."""
        with self._store.scope(tenant_id) as conn:
            return self._insert(conn, tenant_id, c, actor)

    def update(
        self, tenant_id: UUID, c: Concept, actor: str, expected_version: int | None
    ) -> int | Conflict:
        """Write a concept. `expected_version` makes it a compare-and-swap; None is
        last-write-wins. Raises KeyError if the path does not exist."""
        with self._store.scope(tenant_id) as conn:
            return self._overwrite(conn, tenant_id, c, actor, expected_version)

    def import_many(
        self, tenant_id: UUID, bundle: Sequence[Concept], actor: str
    ) -> int:
        """Store a whole bundle in one transaction, last write winning per path.

        Raises KeyError if a path is deleted underneath the import.
        """
        with self._store.scope(tenant_id) as conn:
            for c in bundle:
                if self._insert(conn, tenant_id, c, actor) is None:
                    # The bundle is the authority the operator is replaying, so there
                    # is no version to compare and no conflict to resolve.
                    self._overwrite(conn, tenant_id, c, actor, None)
        return len(bundle)

    def read(self, tenant_id: UUID, path: str) -> Concept | None:
        """The whole concept, body included. None means no such path."""
        with self._store.scope(tenant_id) as conn:
            row = conn.execute(
                f"SELECT {_READ_COLS} FROM concept WHERE path = %s", (path,)
            ).fetchone()
        return None if row is None else _concept(row)

    def read_with_backlinks(
        self, tenant_id: UUID, path: str
    ) -> tuple[Concept, list[str]] | None:
        """The concept and the paths linking to it. One statement, so the backlinks
        cannot be read from a later snapshot than the concept."""
        with self._store.scope(tenant_id) as conn:
            row = conn.execute(
                f"SELECT {_READ_COLS}, {_BACKLINKS} FROM concept WHERE path = %s",
                (path, path),
            ).fetchone()
        return None if row is None else (_concept(row), [str(p) for p in row[-1]])

    def read_all(self, tenant_id: UUID) -> list[Concept]:
        """Every concept, body included, path-ordered, from one snapshot."""
        # ponytail: the whole corpus materialises at once. Stream it through a named
        # cursor if a bundle ever outgrows the process exporting it.
        with self._store.scope(tenant_id) as conn:
            rows = conn.execute(
                f"SELECT {_READ_COLS} FROM concept ORDER BY path"
            ).fetchall()
        return [_concept(row) for row in rows]

    def revisions(self, tenant_id: UUID, limit: int) -> list[Revision]:
        """The most recent `limit` revisions, returned oldest first.

        The newest window, not the oldest: bounded at the wrong end, the log a bundle
        carries showed the first entries ever written and nothing that had happened
        since. Ordered ascending on the way out because that is how it renders.
        """
        if limit <= 0:
            return []
        with self._store.scope(tenant_id) as conn:
            rows = conn.execute(
                # A bulk import shares one clock reading, so created_at alone is not a
                # total order; path and version break the tie the same way both ways.
                "SELECT path, version, op, updated_by, created_at, tenant_id FROM ("
                "  SELECT path, version, op, coalesce(updated_by, '') AS updated_by,"
                "         created_at, tenant_id"
                "  FROM concept_revision"
                "  ORDER BY created_at DESC, path DESC, version DESC LIMIT %s"
                ") recent ORDER BY created_at, path, version",
                (limit,),
            ).fetchall()
        return [
            Revision(str(r[0]), int(r[1]), str(r[2]), str(r[3]), r[4], r[5])
            for r in rows
        ]

    def backlinks(self, tenant_id: UUID, path: str) -> list[str]:
        """The paths whose outbound links name `path`."""
        with self._store.scope(tenant_id) as conn:
            row = conn.execute(f"SELECT {_BACKLINKS}", (path,)).fetchone()
        return [] if row is None else [str(p) for p in row[0]]

    def list_(self, tenant_id: UUID, prefix: str) -> list[tuple[str, str]]:
        """`(path, type)` for every concept under `prefix`. An empty prefix is all."""
        with self._store.scope(tenant_id) as conn:
            # starts_with, not LIKE: an underscore is legal in a path and a wildcard
            # in a pattern.
            rows = conn.execute(
                "SELECT path, type FROM concept "
                "WHERE starts_with(path, %s) ORDER BY path",
                (prefix,),
            ).fetchall()
        return [(str(r[0]), str(r[1])) for r in rows]

    @contextmanager
    def _connect(self, tenant_id: UUID | None) -> Iterator[psycopg.Connection]:
        """Dispatch `tenant_id=None` to `admin_scope()`, any other value to `scope()`.

        Centralised so a per-method copy cannot get the two backwards.
        """
        with (
            self._store.admin_scope()
            if tenant_id is None
            else self._store.scope(tenant_id)
        ) as conn:
            yield conn

    def page(
        self, tenant_id: UUID | None, prefix: str, limit: int, offset: int
    ) -> list[Summary]:
        """A path-ordered page of concepts under `prefix`. `tenant_id=None` mixes
        every tenant."""
        if limit <= 0:
            return []
        with self._connect(tenant_id) as conn:
            rows = conn.execute(
                # path alone is not a total order under admin_scope(): two tenants can
                # share a path, so tenant_id breaks the tie the same way both ways.
                f"SELECT {_SUMMARY_COLS} FROM concept WHERE {_STARTS_WITH} "
                "ORDER BY path, tenant_id LIMIT %s OFFSET %s",
                (prefix, limit, offset),
            ).fetchall()
        return [
            Summary(str(r[0]), str(r[1]), str(r[2]), str(r[3]), int(r[4]), r[5], r[6])
            for r in rows
        ]

    def count(self, tenant_id: UUID | None, prefix: str) -> int:
        """How many concepts `page` would cover for the same `tenant_id`/`prefix`."""
        with self._connect(tenant_id) as conn:
            row = conn.execute(
                f"SELECT count(*) FROM concept WHERE {_STARTS_WITH}", (prefix,)
            ).fetchone()
        return int(_row(row)[0])

    def totals(self, tenant_id: UUID | None) -> Totals:
        """Corpus-wide counts for the admin console's summary tiles."""
        with self._connect(tenant_id) as conn:
            concepts, links = _row(
                conn.execute(
                    "SELECT count(*), coalesce(sum(cardinality(links)), 0) FROM concept"
                ).fetchone()
            )
            by_type = conn.execute(
                "SELECT type, count(*) FROM concept GROUP BY type"
            ).fetchall()
            revisions = _row(
                conn.execute("SELECT count(*) FROM concept_revision").fetchone()
            )[0]
            # An orphan is a concept no concept of its own tenant links to. The
            # anti-join correlates on tenant_id as well as path: under admin_scope()
            # one tenant's link would otherwise hide another tenant's orphan, since a
            # path is unique only within a tenant. Sequential like `_BACKLINKS`, and
            # for the same reason: the GIN index only serves `@>`, and arraycontains is
            # not leakproof under FORCE ROW LEVEL SECURITY.
            orphans = _row(
                conn.execute(
                    "SELECT count(*) FROM concept c WHERE NOT EXISTS ("
                    "  SELECT 1 FROM concept b"
                    "  WHERE b.tenant_id = c.tenant_id AND c.path = ANY(b.links))"
                ).fetchone()
            )[0]
        return Totals(
            concepts=int(concepts),
            by_type={str(t): int(n) for t, n in by_type},
            revisions=int(revisions),
            links=int(links),
            orphans=int(orphans),
        )

    def activity(self, tenant_id: UUID | None, limit: int) -> list[Revision]:
        """The most recent `limit` revisions, newest first, unlike `revisions()`."""
        if limit <= 0:
            return []
        with self._connect(tenant_id) as conn:
            rows = conn.execute(
                # Under admin_scope() two tenants can hold the same path at the same
                # version and timestamp, so tenant_id is what makes the order total —
                # without it a LIMITed feed picks arbitrarily between them. Descending
                # like every column before it.
                "SELECT path, version, op, coalesce(updated_by, ''), created_at, "
                "tenant_id "
                "FROM concept_revision "
                "ORDER BY created_at DESC, path DESC, version DESC, tenant_id DESC "
                "LIMIT %s",
                (limit,),
            ).fetchall()
        return [
            Revision(str(r[0]), int(r[1]), str(r[2]), str(r[3]), r[4], r[5])
            for r in rows
        ]

    def revisions_for(
        self, tenant_id: UUID | None, path: str, limit: int
    ) -> list[Revision]:
        """The most recent `limit` revisions of one path, newest first.

        Unlike `activity()`, the cap is per path, not per tenant: a concept's own
        history cannot be crowded out by other concepts' unrelated revisions.
        """
        if limit <= 0:
            return []
        with self._connect(tenant_id) as conn:
            rows = conn.execute(
                "SELECT path, version, op, coalesce(updated_by, ''), created_at, "
                "tenant_id "
                "FROM concept_revision WHERE path = %s "
                "ORDER BY version DESC LIMIT %s",
                (path, limit),
            ).fetchall()
        return [
            Revision(str(r[0]), int(r[1]), str(r[2]), str(r[3]), r[4], r[5])
            for r in rows
        ]

    def daily_writes(self, tenant_id: UUID | None, days: int) -> list[tuple[date, int]]:
        """Concept-revision counts for the last `days` days, oldest first and
        zero-filled so a quiet day does not vanish from the chart."""
        if days <= 0:
            return []
        with self._connect(tenant_id) as conn:
            rows = conn.execute(
                "SELECT d::date, count(r.created_at) FROM generate_series("
                "  (current_date - (%s::int - 1))::timestamp, current_date::timestamp,"
                "  interval '1 day'"
                ") AS d "
                "LEFT JOIN concept_revision r ON r.created_at::date = d::date "
                "GROUP BY d ORDER BY d",
                (days,),
            ).fetchall()
        return [(r[0], int(r[1])) for r in rows]

    def graph(
        self, tenant_id: UUID, limit: int
    ) -> list[tuple[str, str, str, list[str]]]:
        """`(path, type, title, links)` for the first `limit` concepts by path."""
        with self._store.scope(tenant_id) as conn:
            rows = conn.execute(
                "SELECT path, type, title, links FROM concept ORDER BY path LIMIT %s",
                (limit,),
            ).fetchall()
        return [(str(r[0]), str(r[1]), str(r[2]), list(r[3])) for r in rows]

    def tenants(self) -> list[tuple[UUID, int]]:
        """Every tenant holding at least one concept, and its count. Admin-only:
        there is no tenant registry besides this table."""
        with self._store.admin_scope() as conn:
            rows = conn.execute(
                "SELECT tenant_id, count(*) FROM concept "
                "GROUP BY tenant_id ORDER BY tenant_id"
            ).fetchall()
        return [(r[0], int(r[1])) for r in rows]

    def search(
        self, tenant_id: UUID, query: str, limit: int, prefix: str | None
    ) -> list[Hit]:
        """Ranked cards, never bodies. An empty result beats an invalid tsquery."""
        terms = _tsquery(query)
        if not terms or limit <= 0:
            return []
        with self._store.scope(tenant_id) as conn:
            rows = conn.execute(
                "SELECT path, type, title, description, "
                "       ts_rank_cd(search, to_tsquery('english', %s)) AS score "
                "FROM concept "
                "WHERE search @@ to_tsquery('english', %s) "
                "  AND starts_with(path, coalesce(%s::text, '')) "
                "ORDER BY score DESC, path LIMIT %s",
                (terms, terms, prefix, limit),
            ).fetchall()
        return [
            Hit(str(r[0]), str(r[1]), str(r[2]), str(r[3]), float(r[4])) for r in rows
        ]

    def grep(self, tenant_id: UUID, pattern: str, limit: int) -> list[tuple[str, str]]:
        """`(path, snippet)` for every concept matching the POSIX regex `pattern`.

        Raises ValueError if Postgres cannot compile the pattern, or if matching it
        exceeds the statement timeout.
        """
        if limit <= 0:
            return []
        try:
            with self._store.scope(tenant_id) as conn:
                # SET LOCAL takes no parameter; the value is an int literal in this file.
                conn.execute(f"SET LOCAL statement_timeout = {_GREP_TIMEOUT_MS}")
                rows = conn.execute(_GREP, (pattern, limit)).fetchall()
        except psycopg.errors.InvalidRegularExpression as exc:
            # Postgres would otherwise surface a bare SQLSTATE to the agent.
            raise ValueError(f"unusable regular expression: {pattern!r}") from exc
        except psycopg.errors.QueryCanceled as exc:
            raise ValueError(
                f"the regular expression took longer than {_GREP_TIMEOUT_MS}ms: "
                f"{pattern!r}"
            ) from exc
        return [(str(r[0]), str(r[1])) for r in rows]

    @staticmethod
    def _insert(
        conn: psycopg.Connection, tenant_id: UUID, c: Concept, actor: str
    ) -> int | None:
        columns = ", ".join(("tenant_id", "path", *_FIELDS, "updated_by"))
        placeholders = ",".join(["%s"] * (len(_FIELDS) + 3))
        row = conn.execute(
            f"INSERT INTO concept ({columns}) VALUES ({placeholders}) "
            "ON CONFLICT (tenant_id, path) DO NOTHING RETURNING version",
            (tenant_id, c.path, *_values(c), actor),
        ).fetchone()
        if row is None:
            return None
        version = int(row[0])
        ConceptStore._revise(conn, tenant_id, c, version, "create", actor)
        return version

    @staticmethod
    def _overwrite(
        conn: psycopg.Connection,
        tenant_id: UUID,
        c: Concept,
        actor: str,
        expected_version: int | None,
    ) -> int | Conflict:
        assignments = ", ".join(f"{f}=%s" for f in _FIELDS)
        row = conn.execute(
            f"UPDATE concept SET {assignments}, updated_by=%s, "
            "version=version+1, updated_at=now() "
            "WHERE path=%s AND (%s::int IS NULL OR version=%s) RETURNING version",
            (*_values(c), actor, c.path, expected_version, expected_version),
        ).fetchone()
        if row is not None:
            version = int(row[0])
            ConceptStore._revise(conn, tenant_id, c, version, "update", actor)
            return version
        current = conn.execute(
            "SELECT version, body FROM concept WHERE path=%s", (c.path,)
        ).fetchone()
        if current is None:
            raise KeyError(c.path)
        return Conflict(current_version=int(current[0]), current_body=str(current[1]))

    @staticmethod
    def _revise(
        conn: psycopg.Connection,
        tenant_id: UUID,
        c: Concept,
        version: int,
        op: str,
        actor: str,
    ) -> None:
        conn.execute(
            "INSERT INTO concept_revision "
            "(tenant_id, path, version, op, snapshot, updated_by) VALUES (%s,%s,%s,%s,%s,%s)",
            (
                tenant_id,
                c.path,
                version,
                op,
                _jsonb(asdict(c) | {"version": version}),
                actor,
            ),
        )
