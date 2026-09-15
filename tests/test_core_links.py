from okf_core import parse
from okf_core.links import extract_links


def test_extracts_relative_links_and_strips_md():
    body = "See [layers](../architecture/layers.md) and [auth](decisions/auth.md)."
    assert extract_links(body) == ("../architecture/layers", "decisions/auth")


def test_ignores_external_urls():
    assert extract_links("[docs](https://example.com/x.md)") == ()


def test_deduplicates_preserving_order():
    body = "[a](x.md) [b](y.md) [c](x.md)"
    assert extract_links(body) == ("x", "y")


def test_parse_populates_links_from_the_body():
    doc = "---\ntype: Concept\n---\nSee [layers](architecture/layers.md).\n"
    assert parse(doc, "p").links == ("architecture/layers",)
