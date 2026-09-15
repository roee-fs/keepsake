"""Outbound edges are markdown links to other concepts. External URLs are not edges.

Targets are written relative to the linking document, but an edge is matched against
a stored concept path, so every target is resolved against the source path here.
"""

from __future__ import annotations

import posixpath
import re

# The lookbehind keeps `![alt](img.png)` from becoming an edge. The optional trailing
# group is a CommonMark link title, which is not part of the destination.
_LINK = re.compile(r"(?<!!)\[[^\]]*\]\(\s*([^)\s]*)(?:\s+[^)]*)?\s*\)")
_EXTERNAL = re.compile(r"\A[a-z][a-z0-9+.-]*://|\A(?:mailto|tel):|\A//", re.IGNORECASE)


def _resolve(target: str, directory: str) -> str | None:
    """Return the concept path a link target names, or None if it names no concept."""
    if _EXTERNAL.match(target):
        return None
    # An in-page anchor leaves nothing behind and is not an edge.
    target = target.split("#", 1)[0].split("?", 1)[0]
    if not target:
        return None
    target = target.removesuffix(".md")
    # A rooted target resolves against the bundle root, but still resolves: `/../x`
    # escapes it just as `../x` does.
    if target.startswith("/"):
        target, directory = target.lstrip("/"), ""
    resolved = posixpath.normpath(posixpath.join(directory, target))
    # A leading `..` escaped the bundle root and `.` names a directory, so no stored
    # path can ever equal either.
    if not resolved or resolved == "." or resolved.split("/", 1)[0] == "..":
        return None
    return resolved


def extract_links(body: str, path: str) -> tuple[str, ...]:
    """Return the concept paths `body` links to, deduplicated in first-seen order.

    `path` is the linking concept's own path; targets resolve against its directory.
    """
    directory = posixpath.dirname(path)
    seen: dict[str, None] = {}
    for target in _LINK.findall(body):
        if (resolved := _resolve(target, directory)) is not None:
            seen.setdefault(resolved, None)
    return tuple(seen)
