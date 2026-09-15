"""Per-write validation. Bundle-level link resolution lives in the CLI, not here."""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    # Runtime import would be circular: okf_core re-exports validate.
    from okf_core import Concept

# A bundle materialises these two at its root when it is written, so a concept
# holding one would be overwritten on export and skipped on the way back in.
RESERVED_PATHS = frozenset({"index", "log"})

# Every limit here is a Postgres one, refused in advance so the agent gets a sentence
# it can act on rather than a driver error the transport reports as a protocol failure.
#
# All of them are counted in BYTES, because every limit they stand in for is. Measured
# in characters, a 1024-character CJK path is 3072 bytes and blows the btree limit
# these exist to keep it under — the cap would admit exactly what it was added to stop.
#
# A btree entry is capped at 2704 bytes and the primary key is (tenant_id, path).
MAX_PATH = 1024
# The `search` column is generated, so an oversized document fails the INSERT itself:
# a tsvector holds at most 1MB of lexemes, and positions roughly halve that again.
MAX_BODY = 256 * 1024
MAX_TITLE = 4096

_FIELD_LIMITS = (
    ("title", MAX_TITLE),
    ("description", MAX_TITLE),
    ("body", MAX_BODY),
    ("type", MAX_TITLE),
)


def _size(text: str) -> int:
    """Its length in UTF-8 bytes, which is the unit Postgres counts in."""
    return len(text.encode())


def _control(text: str) -> bool:
    """Any C0 control or DEL. Tab and newline included: a path is a file name."""
    return any(ch < " " or ch == "\x7f" for ch in text)


def validate(c: Concept) -> list[str]:
    """Return every rule the concept breaks. An empty list means valid."""
    errors: list[str] = []
    if not c.type.strip():
        errors.append("type is required")
    if c.path.startswith("/"):
        errors.append("path must be relative, not absolute")
    if ".." in c.path.split("/"):
        errors.append("path must not traverse upward")
    if not c.path.strip():
        errors.append("path is required")
    if c.path in RESERVED_PATHS:
        errors.append(f"path {c.path!r} is reserved for a generated bundle file")
    # A trailing or doubled slash. Each names a concept the store accepts and the
    # bundle export cannot write out as a file. The leading slash is stripped first
    # because "must be relative" above has already said that, better.
    elif c.path.strip() and "" in c.path.removeprefix("/").split("/"):
        errors.append("path must not have an empty segment")
    if "\x00" in c.path:
        errors.append("path must not contain a NUL byte")
    elif _control(c.path):
        errors.append("path must not contain a control character")
    if (size := _size(c.path)) > MAX_PATH:
        errors.append(f"path is too long: {size} bytes, at most {MAX_PATH}")

    for name, limit in _FIELD_LIMITS:
        value: str = getattr(c, name)
        # Postgres text holds no NUL, so this reaches the driver and fails the write
        # after validation has already passed it.
        if "\x00" in value:
            errors.append(f"{name} must not contain a NUL byte")
        if (size := _size(value)) > limit:
            errors.append(f"{name} is too long: {size} bytes, at most {limit}")
    return errors
