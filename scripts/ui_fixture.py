"""A real stack for Playwright: testcontainers Postgres, `keepsake migrate`, a
seeded bundle imported into two tenants, then `keepsake serve`.

Run standalone (`uv run python3 scripts/ui_fixture.py`) or via
`scripts/ui-fixture.sh`, which builds the console bundle first. Prints
`PORT=<port>` once the server is ready, then blocks until killed. SIGTERM/SIGINT
reach this process directly (the shell wrapper `exec`s into it) so uvicorn's own
signal handling shuts the server down and the `with` block below stops the
container on the way out.
"""

from __future__ import annotations

import os
import socket
import tempfile
from pathlib import Path
from uuid import UUID

import psycopg
import uvicorn
from psycopg import sql
from testcontainers.community.postgres import PostgresContainer

from keepsake.cli import import_bundle, migrate
from keepsake.server.app import Config, build_app
from keepsake.store import SCHEMA
from keepsake.store.concepts import ConceptStore
from keepsake.store.pool import Store

OWNER_ROLE = "okf_owner"
OWNER_PASSWORD = "owner"
APP_ROLE = "okf_app"
APP_PASSWORD = "app"

# Also imported by the Playwright specs (frontend/tests/fixture-constants.ts) --
# keep the two definitions in sync by hand, there is no shared source of truth
# across the Python/TypeScript boundary.
TENANT_A = UUID("11111111-1111-1111-1111-111111111111")
TENANT_B = UUID("22222222-2222-2222-2222-222222222222")
ADMIN_PASSWORD = "ui-fixture-admin-password"

# path -> (type, title, description, body). Bodies carry the real markdown links
# that make up the corpus's link graph; `import_bundle` derives `links` from them.
# auth/login, billing/customer, ops/deploy and ops/oncall have no inbound links --
# the four orphans. "oncall" appears nowhere else, so it search-narrows to one hit.
_BUNDLE: dict[str, tuple[str, str, str, str]] = {
    "auth/login.md": (
        "guide",
        "Login",
        "How users sign in.",
        "Start a [Session](session.md) once credentials check out. High-risk "
        "accounts also require [MFA](security/mfa.md).\n",
    ),
    "auth/session.md": (
        "guide",
        "Session",
        "Session lifecycle after login.",
        "A session ends explicitly via [Logout](logout.md) or by expiring.\n",
    ),
    "auth/logout.md": (
        "guide",
        "Logout",
        "Ending a session.",
        "Revokes the session token and clears client state.\n",
    ),
    "auth/security/mfa.md": (
        "policy",
        "MFA",
        "Multi-factor enforcement.",
        "Every challenge is recorded to the [Audit log](audit-log.md).\n",
    ),
    "auth/security/audit-log.md": (
        "policy",
        "Audit log",
        "Security event trail.",
        "An append-only record of security-relevant events.\n",
    ),
    "billing/customer.md": (
        "entity",
        "Customer",
        "A billed account.",
        "Owns zero or more [Invoice](invoice.md)s under a [Plan](plan.md).\n",
    ),
    "billing/invoice.md": (
        "entity",
        "Invoice",
        "A billing statement.",
        "May carry a [Refund](refund.md) against it.\n",
    ),
    "billing/plan.md": (
        "entity",
        "Plan",
        "A pricing tier.",
        "Defines the rate a customer is billed at.\n",
    ),
    "billing/refund.md": (
        "process",
        "Refund",
        "Returning charged funds.",
        "Reverses part or all of an invoice's charge.\n",
    ),
    "ops/deploy.md": (
        "process",
        "Deploy",
        "Shipping a release.",
        "A bad deploy is undone with a [Rollback](rollback.md).\n",
    ),
    "ops/rollback.md": (
        "process",
        "Rollback",
        "Reverting a release.",
        "Restores the prior known-good deploy.\n",
    ),
    "ops/oncall.md": (
        "process",
        "Oncall",
        "Who owns production right now.",
        "The oncall engineer is paged first for any production incident.\n",
    ),
}

# Re-imported into TENANT_A only, after the base bundle: bumps exactly one path
# to version 2 so the Detail page's revision history has something to show.
_REVISED_LOGIN_BODY = (
    "Start a [Session](session.md) once credentials check out. High-risk "
    "accounts also require [MFA](security/mfa.md). Rate-limited after five "
    "failed attempts.\n"
)


def _write_bundle(root: Path, files: dict[str, tuple[str, str, str, str]]) -> None:
    for path, (type_, title, description, body) in files.items():
        target = root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(
            f"---\ntype: {type_}\ntitle: {title}\ndescription: {description}\n---\n{body}"
        )


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _dsn(pg: PostgresContainer, user: str, password: str) -> str:
    host = pg.get_container_host_ip()
    port = pg.get_exposed_port(5432)
    return f"postgresql://{user}:{password}@{host}:{port}/{pg.dbname}"


def _create_roles(admin_dsn: str, dbname: str) -> None:
    """Mirrors tests/conftest.py: an unprivileged app role is what makes RLS real."""
    create_role = sql.SQL("CREATE ROLE {} LOGIN PASSWORD {}")
    with psycopg.connect(admin_dsn, autocommit=True) as conn:
        conn.execute(create_role.format(sql.Identifier(OWNER_ROLE), sql.Literal(OWNER_PASSWORD)))
        conn.execute(create_role.format(sql.Identifier(APP_ROLE), sql.Literal(APP_PASSWORD)))
        conn.execute(
            sql.SQL("GRANT CREATE ON DATABASE {} TO {}").format(
                sql.Identifier(dbname), sql.Identifier(OWNER_ROLE)
            )
        )


def main() -> None:
    repo_root = Path(__file__).resolve().parent.parent
    port = int(os.environ.get("UI_FIXTURE_PORT", "0")) or _free_port()

    with PostgresContainer("postgres:17", driver=None) as pg:
        admin_dsn = _dsn(pg, pg.username, pg.password)
        _create_roles(admin_dsn, pg.dbname)
        owner_dsn = _dsn(pg, OWNER_ROLE, OWNER_PASSWORD)
        app_dsn = _dsn(pg, APP_ROLE, APP_PASSWORD)

        migrate(owner_dsn)

        store = Store(app_dsn, schema=SCHEMA)
        try:
            concepts = ConceptStore(store)
            with tempfile.TemporaryDirectory(prefix="ui-fixture-") as tmp:
                bundle_root = Path(tmp) / "bundle"
                _write_bundle(bundle_root, _BUNDLE)
                for tenant_id in (TENANT_A, TENANT_B):
                    import_bundle(concepts, tenant_id, bundle_root)

                revised_root = Path(tmp) / "revision"
                _write_bundle(
                    revised_root,
                    {
                        "auth/login.md": (
                            "guide",
                            "Login",
                            "How users sign in.",
                            _REVISED_LOGIN_BODY,
                        )
                    },
                )
                import_bundle(concepts, TENANT_A, revised_root)
        finally:
            store.close()

        os.environ["KEEPSAKE_ADMIN_PASSWORD"] = ADMIN_PASSWORD
        os.environ["KEEPSAKE_STATIC_DIR"] = str(repo_root / "frontend" / "dist")

        app = build_app(Config(dsn=app_dsn, tenant_id=TENANT_A, schema=SCHEMA))
        print(f"PORT={port}", flush=True)
        uvicorn.run(app, host="127.0.0.1", port=port)


if __name__ == "__main__":
    main()
