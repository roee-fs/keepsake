from okf_core import Concept, parse, serialize

DOC = """---
type: Concept
title: Five Layer Architecture
description: How the layers stack.
tags: [architecture, tooling]
custom_vendor_field: keep-me
---
The spec sits beneath the convention.
"""


def test_parse_promotes_known_fields_and_keeps_unknown():
    c = parse(DOC, "architecture/layers")
    assert c.type == "Concept"
    assert c.title == "Five Layer Architecture"
    assert c.frontmatter["custom_vendor_field"] == "keep-me"
    assert "type" not in c.frontmatter


def test_round_trip_is_byte_identical():
    assert serialize(parse(DOC, "architecture/layers")) == DOC


MINIMAL = """---
type: Concept
title: Five Layer Architecture
---
The spec sits beneath the convention.
"""


def test_round_trip_of_scalar_only_frontmatter_stays_block_style():
    """Flow style is for leaf collections nested in the frontmatter, never for
    the frontmatter mapping itself, which is a leaf when every value is scalar."""
    assert serialize(parse(MINIMAL, "architecture/layers")) == MINIMAL


QUOTED_TITLE = """---
type: Concept
title: "Five Layer Architecture"
nickname: "keep-me"
---
The spec sits beneath the convention.
"""

BLOCK_DESCRIPTION = """---
type: Concept
description: |
  The spec sits beneath the convention.
  The convention sits beneath the habit.
---
Body.
"""


def test_round_trip_preserves_scalar_style_of_promoted_fields():
    """A promoted field must keep its quoting and block style, which coercing it
    through str() would discard: ruamel carries style on str subclasses."""
    assert serialize(parse(QUOTED_TITLE, "architecture/layers")) == QUOTED_TITLE
    assert (
        serialize(parse(BLOCK_DESCRIPTION, "architecture/layers")) == BLOCK_DESCRIPTION
    )


def test_non_string_known_field_is_coerced():
    assert parse("---\ntype: 42\n---\nb\n", "p").type == "42"


def test_a_known_field_left_empty_is_empty_not_the_word_none():
    """`title:` with nothing after it is how a hand-written document says blank, and
    YAML loads it as None."""
    c = parse("---\ntype: Concept\ntitle:\ndescription:\n---\nb\n", "p")
    assert (c.title, c.description) == ("", "")


def test_empty_frontmatter_parses_to_an_empty_mapping():
    c = parse("---\n---\nThe spec sits beneath the convention.\n", "p")
    assert c.frontmatter == {}
    assert c.body == "The spec sits beneath the convention.\n"


CRLF = "---\r\ntype: Concept\r\ntitle: T\r\n---\r\nBody.\r\n"
CRLF_AS_LF = "---\ntype: Concept\ntitle: T\n---\nBody.\n"


def test_crlf_document_normalises_to_lf():
    """LF is OKF's canonical line ending. Asserting the whole document, not just
    the frontmatter, is what catches a body that kept its CRLF endings."""
    assert serialize(parse(CRLF, "p")) == CRLF_AS_LF


def test_plain_python_frontmatter_emits_leaf_lists_in_flow_style():
    """Frontmatter returning from a jsonb column is plain dicts and lists, with no
    ruamel style metadata, so leaf collections must re-emit in flow style to match
    how they were written."""
    c = Concept(
        path="architecture/layers",
        type="Concept",
        title="Five Layer Architecture",
        description="How the layers stack.",
        body="The spec sits beneath the convention.\n",
        frontmatter={
            "tags": ["architecture", "tooling"],
            "custom_vendor_field": "keep-me",
        },
    )
    assert serialize(c) == DOC
