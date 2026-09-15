import pytest

from okf_core import parse
from okf_core.links import extract_links


def test_extracts_relative_links_and_strips_md():
    body = "See [layers](../architecture/layers.md) and [auth](decisions/auth.md)."
    assert extract_links(body, "concepts/overview") == (
        "architecture/layers",
        "concepts/decisions/auth",
    )


def test_sibling_target_resolves_against_the_source_directory():
    assert extract_links("[auth](decisions/auth.md)", "a/b/c") == (
        "a/b/decisions/auth",
    )


def test_root_relative_target_is_taken_from_the_bundle_root():
    assert extract_links("[x](/a/b.md)", "deep/nested/doc") == ("a/b",)


def test_target_escaping_the_bundle_root_is_dropped():
    """A resolved path still leading with `..` can never equal a stored path."""
    assert extract_links("[x](../../outside.md)", "a/doc") == ()


@pytest.mark.parametrize("target", ["/../../etc.md", "/a/../../b.md", "/", ".."])
def test_degenerate_targets_are_not_edges(target: str):
    """Each once resolved to something no stored path can equal, junking `links` and
    making `keepsake validate` report a link to an unknown concept."""
    assert extract_links(f"[x]({target})", "a/doc") == ()


def test_anchor_only_target_is_not_an_edge():
    """A table of contents links every heading and must yield no edges."""
    assert (
        extract_links("See [Overview](#overview) and [Setup](#setup).", "a/doc") == ()
    )


def test_fragment_and_query_are_stripped_from_the_target():
    assert extract_links("[x](b.md#section) [y](c.md?v=1)", "a/doc") == ("a/b", "a/c")


def test_images_are_not_edges():
    assert extract_links("![diagram](d.png) and [real](a.md)", "x/doc") == ("x/a",)


def test_link_with_a_title_is_extracted():
    assert extract_links('[x](a.md "Title")', "p/doc") == ("p/a",)


def test_ignores_external_urls():
    body = "[a](https://example.com/x.md) [b](//example.com/y.md) [c](mailto:a@b.c)"
    assert extract_links(body, "a/doc") == ()


def test_deduplicates_preserving_order():
    body = "[a](x.md) [b](y.md) [c](x.md)"
    assert extract_links(body, "d/doc") == ("d/x", "d/y")


def test_parse_resolves_links_against_the_concept_path():
    doc = "---\ntype: Concept\n---\nSee [layers](../architecture/layers.md).\n"
    assert parse(doc, "concepts/overview").links == ("architecture/layers",)
