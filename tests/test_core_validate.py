from okf_core import Concept
from okf_core.validate import MAX_BODY, MAX_PATH, validate


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


def test_a_multibyte_path_is_measured_in_bytes_not_characters() -> None:
    """The btree limit these caps stand in for is a byte limit. Measured in
    characters, 1024 CJK characters is 3072 bytes and blows the 2704-byte index
    entry the cap exists to keep it under — admitting exactly what it stops."""
    path = "漢" * MAX_PATH  # 1024 characters, 3072 bytes
    assert len(path) == MAX_PATH
    [error] = validate(Concept(path=path, type="Concept"))
    assert error == f"path is too long: {MAX_PATH * 3} bytes, at most {MAX_PATH}"

    # And the limit is still reachable in full when the path is ASCII.
    assert validate(Concept(path="a" * MAX_PATH, type="Concept")) == []


def test_a_multibyte_body_is_measured_in_bytes_too() -> None:
    """Same defect, same reason: the tsvector ceiling is counted in bytes."""
    body = "漢" * MAX_BODY
    [error] = validate(Concept(path="a/b", type="Concept", body=body))
    assert error.startswith("body is too long:")
    assert error.endswith(f"bytes, at most {MAX_BODY}")
