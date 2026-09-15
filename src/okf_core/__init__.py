"""Open Knowledge Format: parsing, validation, and serialisation. No database."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from okf_core.frontmatter import join, split


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


def parse(text: str, path: str) -> Concept:
    """Parse an OKF document. `frontmatter` keeps only the unknown fields."""
    meta, body = split(text)
    return Concept(
        path=path,
        type=str(meta.pop("type", "")),
        title=str(meta.pop("title", "")),
        description=str(meta.pop("description", "")),
        body=body,
        frontmatter=meta,
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
