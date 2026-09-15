"""OKF frontmatter split and round-trip. ruamel preserves key order and scalar
style; comments attached to the frontmatter mapping are not preserved."""

from __future__ import annotations

import io
import re
from typing import Any

from ruamel.yaml import YAML
from ruamel.yaml.comments import CommentedMap

# The closing fence is anchored per-line so that empty frontmatter and CRLF
# documents both match.
_FENCE = re.compile(r"\A---\r?\n(.*?)^---\r?\n(.*)\Z", re.DOTALL | re.MULTILINE)

_yaml = YAML()
_yaml.preserve_quotes = True
# Frontmatter survives a jsonb round trip as plain dicts and lists, with ruamel's
# style metadata stripped. These two restore how it was originally written.
# The ceiling: flow is the one canonical style for a style-less leaf collection,
# so a block-style list written by hand comes back from the database as flow.
_yaml.default_flow_style = None
_yaml.width = 4096


def split(text: str) -> tuple[dict[str, Any], str]:
    """Return (frontmatter mapping, body). A document without a fence has no frontmatter."""
    if not (m := _FENCE.match(text)):
        return {}, text
    return dict(_yaml.load(m.group(1)) or {}), m.group(2)


def join(meta: dict[str, Any], body: str) -> str:
    """Render a frontmatter mapping and body back into one OKF document."""
    buf = io.StringIO()
    root = CommentedMap(meta)
    # `default_flow_style = None` would otherwise collapse an all-scalar mapping.
    root.fa.set_block_style()
    _yaml.dump(root, buf)
    return f"---\n{buf.getvalue()}---\n{body}"
