"""Outbound edges are markdown links to other concepts. External URLs are not edges."""

from __future__ import annotations

import re

_LINK = re.compile(r"\[[^\]]*\]\(([^)\s]+)\)")
_EXTERNAL = re.compile(r"\A[a-z][a-z0-9+.-]*://|\A(?:mailto|tel):", re.IGNORECASE)


def extract_links(body: str) -> tuple[str, ...]:
    """Return the concept paths the body links to, deduplicated in first-seen order."""
    seen: dict[str, None] = {}
    for target in _LINK.findall(body):
        if _EXTERNAL.match(target):
            continue
        seen.setdefault(target.removesuffix(".md"), None)
    return tuple(seen)
