"""The operator commands: import, export, validate, migrate, serve.

A bundle of markdown files is how knowledge enters and leaves; Postgres is the
only store. The index and log a bundle carries are generated at export time and
are never stored as concepts.
"""

from __future__ import annotations

import argparse
import os
import sys
from collections import defaultdict
from collections.abc import Iterator, Sequence
from contextlib import contextmanager
from pathlib import Path
from uuid import UUID

import uvicorn
from alembic import command
from alembic.config import Config as AlembicConfig
from ruamel.yaml import YAMLError

from keepsake.server.app import Config as ServerConfig
from keepsake.server.app import build_app
from keepsake.store import SCHEMA
from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store
from keepsake.store.verify import MisconfiguredDatabase, verify
from okf_core import RESERVED_PATHS, Concept, parse, serialize, validate

_ACTOR = "cli"

# The log is a bundle file an operator reads, not an audit export.
_LOG_LIMIT = 1000

# Alembic scripts ship inside the package, so `migrate` needs no alembic.ini beside it.
_MIGRATIONS = Path(__file__).resolve().parent.parent / "store" / "migrations"

_TOP_LEVEL = "(top level)"

_DEFAULT_PORT = 8000


def _env_port() -> int:
    """The port $KEEPSAKE_PORT names, or 8000 when it names nothing usable.

    kubelet injects `tcp://10.96.0.1:8000` as KEEPSAKE_PORT into every pod in a
    namespace holding a Service named keepsake. Read lazily, and never by a
    subcommand that binds no port.
    """
    value = os.environ.get("KEEPSAKE_PORT", "")
    return int(value) if value.isdigit() else _DEFAULT_PORT


class CliError(RuntimeError):
    """An operator-facing failure. `main` prints it instead of a traceback."""


def _documents(root: Path, *, strict: bool = True) -> list[Concept]:
    """Every concept in the bundle, the generated files excluded.

    Parses the whole bundle before returning, so a malformed file refuses the
    command rather than aborting it half-applied. `strict=False` returns invalid
    concepts instead, for `validate` to report them all at once.
    """
    concepts = []
    for file in sorted(root.rglob("*.md")):
        path = file.relative_to(root).with_suffix("").as_posix()
        if path in RESERVED_PATHS:
            continue
        try:
            # OKF is UTF-8. The default encoding is the locale's, and a container
            # with no LANG set reads ASCII.
            concept = parse(file.read_text(encoding="utf-8"), path)
        except YAMLError as exc:
            # ruamel names the frontmatter it was handed, never the file it came from.
            raise CliError(f"{file}: {exc}") from None
        # Storing an invalid concept is worse than refusing it: a later okf_update
        # merges the stored empty type back in and fails on a field nobody touched.
        if strict and (errors := validate(concept)):
            raise CliError(f"{file}: {'; '.join(errors)}")
        concepts.append(concept)
    return concepts


def import_bundle(concepts: ConceptStore, tenant_id: UUID, root: Path) -> int:
    """Store every concept in the bundle. Returns how many were written."""
    count = 0
    for concept in _documents(root):
        if concepts.create(tenant_id, concept, _ACTOR) is None:
            # Last-write-wins: the bundle is the authority the operator is replaying,
            # so there is no version to compare and no conflict to resolve.
            try:
                concepts.update(tenant_id, concept, _ACTOR, None)
            except KeyError:
                raise CliError(
                    f"{concept.path} was removed while the bundle was importing"
                ) from None
        count += 1
    return count


def _target(root: Path, path: str) -> Path:
    """The file a concept is written to. Raises rather than leave the bundle."""
    if path in RESERVED_PATHS:
        raise CliError(
            f"the concept {path!r} collides with a generated file: the bundle root "
            f"reserves {' and '.join(f'{n}.md' for n in sorted(RESERVED_PATHS))}"
        )
    target = root / f"{path}.md"
    # A traversing path cannot be written through a tool, but whatever is stored, the
    # export MUST NOT write outside the directory the operator named.
    if not target.resolve().is_relative_to(root):
        raise CliError(f"refusing to write {path!r} outside the bundle")
    return target


def export_bundle(concepts: ConceptStore, tenant_id: UUID, root: Path) -> int:
    """Write the corpus out as a bundle, index and log included."""
    root = root.resolve()
    root.mkdir(parents=True, exist_ok=True)
    # Every path is checked before the first file is written: a bundle that is half
    # written looks exactly like a complete one.
    targets = [(p, _target(root, p)) for p, _ in concepts.list_(tenant_id, "")]
    written: list[str] = []
    for path, target in targets:
        concept = concepts.read(tenant_id, path)
        if concept is None:
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(serialize(concept), encoding="utf-8")
        written.append(path)
    # From what reached the disk, not from the listing: the index MUST NOT link to a
    # file that was never written.
    (root / "index.md").write_text(_render_index(written), encoding="utf-8")
    (root / "log.md").write_text(_render_log(concepts, tenant_id), encoding="utf-8")
    return len(written)


def validate_bundle(root: Path) -> list[str]:
    """Every rule the bundle breaks: per-concept errors plus links to nothing."""
    concepts = _documents(root, strict=False)
    known = {c.path for c in concepts}
    errors: list[str] = []
    for concept in concepts:
        errors += [f"{concept.path}: {e}" for e in validate(concept)]
        errors += [
            f"{concept.path}: link to unknown concept {target}"
            for target in concept.links
            if target not in known
        ]
    return errors


def migrate(dsn: str) -> None:
    """Bring the schema to head. Run as the owner role, never as the app role."""
    config = AlembicConfig()
    config.set_main_option("script_location", str(_MIGRATIONS))
    config.set_main_option("sqlalchemy.url", dsn)
    command.upgrade(config, "head")


def _render_index(paths: Sequence[str]) -> str:
    """Group the corpus by its first path segment."""
    groups: defaultdict[str, list[str]] = defaultdict(list)
    for path in paths:
        head, separator, _ = path.partition("/")
        groups[head if separator else _TOP_LEVEL].append(path)
    lines = ["# Index", ""]
    for group in sorted(groups):
        lines += [f"## {group}", ""]
        lines += [f"- [{path}]({path}.md)" for path in groups[group]]
        lines.append("")
    return "\n".join(lines)


def _render_log(
    concepts: ConceptStore, tenant_id: UUID, limit: int = _LOG_LIMIT
) -> str:
    """Render the revision log oldest-first under date headings."""
    revisions = concepts.revisions(tenant_id, limit)
    lines = ["# Log", ""]
    day = ""
    for revision in revisions:
        stamp = revision.created_at.date().isoformat()
        if stamp != day:
            day = stamp
            lines += [f"## {stamp}", ""]
        lines.append(
            f"- `{revision.path}` v{revision.version} "
            f"{revision.op} by {revision.updated_by}"
        )
    if len(revisions) == limit:
        lines += ["", f"Older revisions omitted: the log renders at most {limit}."]
    lines.append("")
    return "\n".join(lines)


def _required(value: str | None, flag: str, env: str) -> str:
    if not value:
        raise CliError(f"{flag} is required, or set {env}")
    return value


def _tenant(args: argparse.Namespace) -> UUID:
    value = _required(args.tenant, "--tenant", "KEEPSAKE_TENANT_ID")
    try:
        return UUID(value)
    except ValueError:
        raise CliError(f"not a tenant uuid: {value!r}") from None


@contextmanager
def _bound(args: argparse.Namespace) -> Iterator[tuple[ConceptStore, UUID]]:
    """A store and a tenant for one command, closed again on the way out."""
    tenant_id = _tenant(args)
    store = Store(_required(args.dsn, "--dsn", "KEEPSAKE_DSN"), schema=SCHEMA)
    try:
        # Nothing here filters by tenant: RLS is the only thing keeping one tenant's
        # concepts out of another's bundle. A privileged DSN — the same one `migrate`
        # needs — would export every tenant at once, silently.
        verify(store, SCHEMA)
        yield ConceptStore(store), tenant_id
    finally:
        store.close()


def _run_import(args: argparse.Namespace) -> int:
    with _bound(args) as (concepts, tenant_id):
        count = import_bundle(concepts, tenant_id, args.directory)
    print(f"imported {count} concepts")
    return 0


def _run_export(args: argparse.Namespace) -> int:
    with _bound(args) as (concepts, tenant_id):
        count = export_bundle(concepts, tenant_id, args.directory)
    print(f"exported {count} concepts")
    return 0


def _run_validate(args: argparse.Namespace) -> int:
    errors = validate_bundle(args.directory)
    for error in errors:
        print(error, file=sys.stderr)
    return 1 if errors else 0


def _run_migrate(args: argparse.Namespace) -> int:
    migrate(_required(args.dsn, "--dsn", "KEEPSAKE_DSN"))
    return 0


def _run_serve(args: argparse.Namespace) -> int:
    config = ServerConfig(
        dsn=_required(args.dsn, "--dsn", "KEEPSAKE_DSN"),
        tenant_id=_tenant(args),
        schema=SCHEMA,
    )
    port = _env_port() if args.port is None else args.port
    uvicorn.run(build_app(config), host=args.host, port=port)
    return 0


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="keepsake",
        description=(
            "Operate an OKF knowledge store. Import and export move a bundle of "
            "markdown files in and out of Postgres; serve exposes the MCP tools."
        ),
    )
    sub = parser.add_subparsers(required=True)

    def _with_dsn(name: str, help_text: str) -> argparse.ArgumentParser:
        command_parser = sub.add_parser(name, help=help_text, description=help_text)
        command_parser.add_argument(
            "--dsn",
            default=os.environ.get("KEEPSAKE_DSN"),
            help="PostgreSQL connection string. Defaults to $KEEPSAKE_DSN.",
        )
        return command_parser

    def _with_tenant(name: str, help_text: str) -> argparse.ArgumentParser:
        command_parser = _with_dsn(name, help_text)
        command_parser.add_argument(
            "--tenant",
            default=os.environ.get("KEEPSAKE_TENANT_ID"),
            help="The tenant to read and write. Defaults to $KEEPSAKE_TENANT_ID.",
        )
        return command_parser

    importer = _with_tenant("import", "Load a bundle of markdown files into the store.")
    importer.add_argument("directory", type=Path, help="The bundle to read.")
    importer.set_defaults(run=_run_import)

    exporter = _with_tenant("export", "Write the stored concepts out as a bundle.")
    exporter.add_argument("directory", type=Path, help="The directory to write.")
    exporter.set_defaults(run=_run_export)

    validator = sub.add_parser(
        "validate",
        help="Check a bundle on disk. Reads no database.",
        description="Check a bundle on disk. Reads no database.",
    )
    validator.add_argument("directory", type=Path, help="The bundle to check.")
    validator.set_defaults(run=_run_validate)

    migrator = _with_dsn("migrate", "Bring the database schema to head.")
    migrator.set_defaults(run=_run_migrate)

    server = _with_tenant("serve", "Serve the MCP tools over HTTP.")
    server.add_argument(
        "--host",
        default=os.environ.get("KEEPSAKE_HOST", "0.0.0.0"),
        help="The address to bind. Defaults to $KEEPSAKE_HOST, then every interface.",
    )
    server.add_argument(
        "--port",
        type=int,
        default=None,
        help="The port to bind. Defaults to $KEEPSAKE_PORT, then 8000.",
    )
    server.set_defaults(run=_run_serve)

    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        exit_code: int = args.run(args)
    except (CliError, MisconfiguredDatabase) as exc:
        print(f"keepsake: {exc}", file=sys.stderr)
        return 1
    return exit_code
