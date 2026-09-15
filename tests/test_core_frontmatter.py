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


def test_plain_python_frontmatter_emits_leaf_lists_in_flow_style():
    """Frontmatter returning from a jsonb column is plain dicts and lists, with
    no ruamel style metadata. Task 10's byte-identical round trip depends on
    this re-emitting the way it was written."""
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
