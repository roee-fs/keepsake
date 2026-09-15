"""Open Knowledge Format: parsing, validation, and serialisation. No database."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from okf_core.frontmatter import join, split
from okf_core.links import extract_links
from okf_core.validate import validate

__all__ = ["Concept", "extract_links", "parse", "serialize", "validate"]


@dataclass(frozen=True, slots=True)
class Concept:
    path: str
    type: str
    title: str = ""
    description: str = ""
    body: str = ""
    frontmatter: dict[str, Any] = field(default_factory=dict)
    links: tuple[str, ...] = ()
    version: int = 1


def _promote(meta: dict[str, Any], key: str) -> str:
    """Pop a known field. ruamel carries scalar style on str subclasses and str() on
    a subclass returns a plain str, so only a non-string may be coerced."""
    value = meta.pop(key, "")
    return value if isinstance(value, str) else str(value)


def parse(text: str, path: str) -> Concept:
    """Parse an OKF document. `frontmatter` keeps only the unknown fields."""
    meta, body = split(text)
    return Concept(
        path=path,
        type=_promote(meta, "type"),
        title=_promote(meta, "title"),
        description=_promote(meta, "description"),
        body=body,
        frontmatter=meta,
        links=extract_links(body, path),
    )


def serialize(c: Concept) -> str:
    """Render a concept back to an OKF document, known fields first."""
    meta: dict[str, Any] = {"type": c.type}
    if c.title:
        meta["title"] = c.title
    if c.description:
        meta["description"] = c.description
    meta.update(c.frontmatter)
    return join(meta, c.body)
