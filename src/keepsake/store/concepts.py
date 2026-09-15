"""Concept persistence. Two statements, never one upsert: an upsert guarded by a
version predicate silently creates a row the caller believed it was updating.

Tables are named unqualified: `Store.scope` sets search_path to the validated
schema plus pg_catalog and nothing else, so a literal prefix would only break a
non-default KEEPSAKE_SCHEMA.
"""

import json
from dataclasses import asdict, dataclass
from functools import partial
from typing import Any
from uuid import UUID

import psycopg
from psycopg.types.json import Jsonb

from keepsake.store.pool import Store
from okf_core import Concept

_COLS = (
    "tenant_id, path, type, title, description, body, frontmatter, links, updated_by"
)

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


class ConceptStore:
    def __init__(self, store: Store) -> None:
        self._store = store

    def create(self, tenant_id: UUID, c: Concept, actor: str) -> int | None:
        """Insert a concept. None means the path is already taken."""
        with self._store.scope(tenant_id) as conn:
            row = conn.execute(
                f"INSERT INTO concept ({_COLS}) "
                "VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s) "
                "ON CONFLICT (tenant_id, path) DO NOTHING RETURNING version",
                (
                    tenant_id,
                    c.path,
                    c.type,
                    c.title,
                    c.description,
                    c.body,
                    _jsonb(c.frontmatter),
                    list(c.links),
                    actor,
                ),
            ).fetchone()
            if row is None:
                return None
            version = int(row[0])
            self._revise(conn, tenant_id, c, version, "create", actor)
            return version

    def update(
        self, tenant_id: UUID, c: Concept, actor: str, expected_version: int | None
    ) -> int | Conflict:
        """Write a concept. `expected_version` makes it a compare-and-swap; None is
        last-write-wins. Raises KeyError if the path does not exist."""
        with self._store.scope(tenant_id) as conn:
            row = conn.execute(
                "UPDATE concept SET type=%s, title=%s, description=%s, body=%s, "
                "frontmatter=%s, links=%s, updated_by=%s, version=version+1, updated_at=now() "
                "WHERE path=%s AND (%s::int IS NULL OR version=%s) RETURNING version",
                (
                    c.type,
                    c.title,
                    c.description,
                    c.body,
                    _jsonb(c.frontmatter),
                    list(c.links),
                    actor,
                    c.path,
                    expected_version,
                    expected_version,
                ),
            ).fetchone()
            if row is not None:
                version = int(row[0])
                self._revise(conn, tenant_id, c, version, "update", actor)
                return version
            current = conn.execute(
                "SELECT version, body FROM concept WHERE path=%s", (c.path,)
            ).fetchone()
            if current is None:
                raise KeyError(c.path)
            return Conflict(
                current_version=int(current[0]), current_body=str(current[1])
            )

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
