"""Per-write validation. Bundle-level link resolution lives in the CLI, not here."""

from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    # Runtime import would be circular: okf_core re-exports validate.
    from okf_core import Concept


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
    return errors
