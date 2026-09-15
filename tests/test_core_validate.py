from okf_core import Concept
from okf_core.validate import validate


def test_missing_type_is_an_error():
    assert validate(Concept(path="a/b", type="")) == ["type is required"]


def test_path_must_not_be_absolute_or_traverse():
    assert "path must be relative" in validate(Concept(path="/a/b", type="Concept"))[0]
    assert (
        "path must not traverse"
        in validate(Concept(path="a/../../b", type="Concept"))[0]
    )


def test_empty_path_is_an_error():
    assert validate(Concept(path="  ", type="Concept")) == ["path is required"]


def test_every_broken_rule_is_reported():
    """Rules are independent so one call shows the caller everything to fix."""
    assert validate(Concept(path="/a/b", type="")) == [
        "type is required",
        "path must be relative, not absolute",
    ]


def test_valid_concept_has_no_errors():
    assert validate(Concept(path="a/b", type="Concept")) == []


def test_the_generated_bundle_names_are_reserved_at_the_root():
    """A bundle writes index.md and log.md itself, so a concept holding one of those
    paths would be exported over and skipped on the way back in."""
    assert "reserved" in validate(Concept(path="index", type="Concept"))[0]
    assert "reserved" in validate(Concept(path="log", type="Concept"))[0]
    # Only at the root: deeper in the tree the name is ordinary knowledge.
    assert validate(Concept(path="architecture/index", type="Concept")) == []
