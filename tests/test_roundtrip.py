"""The conformance gate: a bundle imported into Postgres and exported again is the
same bytes. That promise is what makes the store safe to adopt.

Three input classes are known not to round-trip and are pinned here as ceilings
rather than kept out of the fixtures: CRLF line endings, comments in frontmatter,
and unquoted YAML dates.
"""

import uuid
from pathlib import Path

import psycopg
import pytest
from psycopg import sql
from starlette.applications import Starlette

from keepsake.cli import (
    CliError,
    _render_log,
    export_bundle,
    import_bundle,
    main,
    migrate,
    validate_bundle,
)
from keepsake.store import SCHEMA
from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store
from okf_core import Concept

DOC = """---
type: Concept
title: Five Layer Architecture
description: How the layers stack.
tags: [architecture, tooling]
custom_vendor_field: keep-me
---
The spec sits beneath the convention. Beneath that, the café.
"""


@pytest.fixture
def tenant() -> uuid.UUID:
    """A tenant of its own per test: the database outlives the function-scoped store."""
    return uuid.uuid4()


def _bundle(tmp_path: Path, text: str = DOC, name: str = "layers.md") -> Path:
    src = tmp_path / "in"
    (src / "architecture").mkdir(parents=True, exist_ok=True)
    (src / "architecture" / name).write_text(text)
    return src


def test_round_trip_is_byte_identical(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    import_bundle(concepts, tenant, _bundle(tmp_path))
    out = tmp_path / "out"
    export_bundle(concepts, tenant, out)

    assert (out / "architecture" / "layers.md").read_text() == DOC


def test_the_unknown_fields_are_what_postgres_holds(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    """The round trip is only a promise if the bytes came back out of the database."""
    import_bundle(concepts, tenant, _bundle(tmp_path))

    stored = concepts.read(tenant, "architecture/layers")
    assert stored is not None
    assert stored.frontmatter == {
        "tags": ["architecture", "tooling"],
        "custom_vendor_field": "keep-me",
    }


def test_round_trip_survives_a_separate_connection_pool(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path, pg_dsn: str
) -> None:
    """Exported by a store that shares nothing with the importer but Postgres."""
    import_bundle(concepts, tenant, _bundle(tmp_path))

    out = tmp_path / "out"
    other = Store(pg_dsn)
    try:
        export_bundle(ConceptStore(other), tenant, out)
    finally:
        other.close()

    assert (out / "architecture" / "layers.md").read_text() == DOC


def test_export_generates_index_and_log(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    import_bundle(concepts, tenant, _bundle(tmp_path))
    concepts.create(tenant, Concept(path="glossary", type="Concept"), "test")
    out = tmp_path / "out"
    export_bundle(concepts, tenant, out)

    index = (out / "index.md").read_text()
    assert "## architecture" in index
    assert "[architecture/layers](architecture/layers.md)" in index
    # A concept at the root has no first segment to be grouped under.
    assert "## (top level)" in index
    assert "[glossary](glossary.md)" in index

    log = (out / "log.md").read_text()
    assert "architecture/layers" in log
    assert "create" in log


def test_the_log_is_dated_and_says_when_it_is_truncated(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    import_bundle(concepts, tenant, _bundle(tmp_path))
    import_bundle(concepts, tenant, _bundle(tmp_path, name="other.md"))

    # Three revisions: the first file, the same file replayed as an update by the
    # second import, and the second file.
    full = _render_log(concepts, tenant, 10)
    assert full.count("- `architecture/") == 3
    assert "omitted" not in full
    # Oldest first: a log that reads backwards is not a log.
    assert full.index("`architecture/layers` v1") < full.index(
        "`architecture/layers` v2"
    )

    revisions = concepts.revisions(tenant, 10)
    assert f"## {revisions[0].created_at.date().isoformat()}" in full
    # Negative, not zero: Postgres answers LIMIT 0 with no rows by itself, so only a
    # negative limit reaches the guard.
    assert concepts.revisions(tenant, -1) == []
    assert "omitted" in _render_log(concepts, tenant, 1)


def test_generated_files_are_not_stored_as_concepts(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    out = tmp_path / "out"
    export_bundle(concepts, tenant, out)
    import_bundle(concepts, tenant, out)
    assert concepts.read(tenant, "index") is None
    assert concepts.read(tenant, "log") is None


def test_only_the_bundle_root_reserves_the_generated_names(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    """A concept genuinely named `index` deeper in the tree is knowledge, not a
    generated file, and skipping it would drop it on import."""
    src = _bundle(tmp_path, name="index.md")
    assert import_bundle(concepts, tenant, src) == 1
    assert concepts.read(tenant, "architecture/index") is not None


def test_import_counts_the_concepts_it_wrote(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    src = _bundle(tmp_path)
    (src / "index.md").write_text("# Index\n")
    (src / "log.md").write_text("# Log\n")
    assert import_bundle(concepts, tenant, src) == 1


def test_reimporting_a_path_updates_it(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    import_bundle(concepts, tenant, _bundle(tmp_path))
    import_bundle(concepts, tenant, _bundle(tmp_path, DOC.replace("beneath", "above")))

    stored = concepts.read(tenant, "architecture/layers")
    assert stored is not None
    assert "above" in stored.body
    assert stored.version == 2


def test_export_refuses_a_path_that_escapes_the_bundle(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    """A traversing path never reaches the store through a tool, and the export
    still MUST NOT write outside the directory the operator named."""
    concepts.create(tenant, Concept(path="../escape", type="Concept"), "test")
    out = tmp_path / "out"

    with pytest.raises(CliError):
        export_bundle(concepts, tenant, out)
    assert not (tmp_path / "escape.md").exists()


def test_export_refuses_a_concept_that_collides_with_a_generated_file(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    concepts.create(tenant, Concept(path="index", type="Concept"), "test")

    with pytest.raises(CliError):
        export_bundle(concepts, tenant, tmp_path / "out")


def test_crlf_is_normalised_to_lf(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    """Accepted ceiling: a mixed-ending document is worse than a consistent one."""
    import_bundle(concepts, tenant, _bundle(tmp_path, DOC.replace("\n", "\r\n")))
    out = tmp_path / "out"
    export_bundle(concepts, tenant, out)

    exported = (out / "architecture" / "layers.md").read_text()
    assert "\r" not in exported
    assert exported == DOC


def test_a_frontmatter_comment_is_not_preserved(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    """Accepted ceiling: the mapping is loaded as a plain dict, comments and all."""
    commented = DOC.replace("type: Concept\n", "type: Concept\n# why this exists\n")
    import_bundle(concepts, tenant, _bundle(tmp_path, commented))
    out = tmp_path / "out"
    export_bundle(concepts, tenant, out)

    exported = (out / "architecture" / "layers.md").read_text()
    assert "# why this exists" not in exported
    assert exported == DOC


def test_an_unquoted_yaml_date_comes_back_as_a_string(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path
) -> None:
    """Accepted ceiling: jsonb has no date, so the type does not survive the store."""
    dated = DOC.replace("custom_vendor_field: keep-me", "created: 2026-01-01")
    import_bundle(concepts, tenant, _bundle(tmp_path, dated))
    out = tmp_path / "out"
    export_bundle(concepts, tenant, out)

    stored = concepts.read(tenant, "architecture/layers")
    assert stored is not None
    assert stored.frontmatter["created"] == "2026-01-01"
    exported = (out / "architecture" / "layers.md").read_text()
    assert "2026-01-01" in exported
    assert "created: 2026-01-01\n" not in exported


def test_validate_accepts_a_bundle_whose_links_resolve(tmp_path: Path) -> None:
    src = _bundle(tmp_path, DOC.replace("convention.", "convention. [see](./other.md)"))
    (src / "architecture" / "other.md").write_text("---\ntype: Concept\n---\nOther.\n")
    assert validate_bundle(src) == []


def test_validate_reports_a_dangling_link_and_a_missing_type(tmp_path: Path) -> None:
    src = _bundle(tmp_path, DOC.replace("convention.", "convention. [gone](./gone.md)"))
    (src / "architecture" / "typeless.md").write_text("---\ntitle: No type\n---\nx\n")

    errors = validate_bundle(src)
    assert any("architecture/gone" in e for e in errors)
    assert any("type is required" in e for e in errors)


def test_main_exports_a_bundle(
    concepts: ConceptStore, tenant: uuid.UUID, tmp_path: Path, pg_dsn: str
) -> None:
    import_bundle(concepts, tenant, _bundle(tmp_path))
    out = tmp_path / "out"

    assert main(["export", str(out), "--dsn", pg_dsn, "--tenant", str(tenant)]) == 0
    assert (out / "architecture" / "layers.md").read_text() == DOC


def test_export_skips_a_concept_that_vanished_mid_export(
    concepts: ConceptStore,
    tenant: uuid.UUID,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    """A concept deleted between the listing and the read loses its file, not the
    whole export."""
    import_bundle(concepts, tenant, _bundle(tmp_path))
    monkeypatch.setattr(concepts, "read", lambda *_: None)
    out = tmp_path / "out"

    assert export_bundle(concepts, tenant, out) == 0
    assert not (out / "architecture" / "layers.md").exists()
    assert (out / "index.md").exists()


def test_serve_hands_uvicorn_the_verified_app_and_the_parsed_port(
    tenant: uuid.UUID, pg_dsn: str, monkeypatch: pytest.MonkeyPatch
) -> None:
    served: dict[str, object] = {}
    monkeypatch.setattr(
        "keepsake.cli.uvicorn.run",
        lambda app, **kwargs: served.update(app=app, **kwargs),
    )

    code = main(["serve", "--dsn", pg_dsn, "--tenant", str(tenant), "--port", "9123"])

    assert code == 0
    assert served["port"] == 9123
    app = served["app"]
    assert isinstance(app, Starlette)
    app.state.store.close()


def test_serve_refuses_a_database_that_does_not_isolate(
    migrated: bool,
    admin_dsn: str,
    tenant: uuid.UUID,
    capsys: pytest.CaptureFixture[str],
) -> None:
    """The deployed pod crash-loops on this, so the message MUST reach the logs."""
    assert main(["serve", "--dsn", admin_dsn, "--tenant", str(tenant)]) == 1
    assert "must not connect as a superuser" in capsys.readouterr().err


def test_main_reports_a_bad_tenant_without_a_traceback(
    tmp_path: Path, pg_dsn: str, capsys: pytest.CaptureFixture[str]
) -> None:
    code = main(["export", str(tmp_path), "--dsn", pg_dsn, "--tenant", "not-a-uuid"])
    assert code == 1
    assert "not-a-uuid" in capsys.readouterr().err


def test_main_reports_a_missing_dsn_without_a_traceback(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.delenv("KEEPSAKE_DSN", raising=False)
    assert main(["export", str(tmp_path), "--tenant", str(uuid.uuid4())]) == 1
    assert "--dsn" in capsys.readouterr().err


def test_migrate_resolves_its_own_script_location(
    migrated: bool, owner_dsn: str
) -> None:
    """Idempotent, so running it over the fixture's schema only proves it resolves."""
    migrate(owner_dsn)
    with psycopg.connect(owner_dsn) as conn:
        row = conn.execute(
            sql.SQL("SELECT version_num FROM {}.alembic_version").format(
                sql.Identifier(SCHEMA)
            )
        ).fetchone()
    assert row is not None and row[0] == "0001"
