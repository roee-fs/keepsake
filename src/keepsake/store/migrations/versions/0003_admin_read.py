"""Let a connection that declares itself an admin read every tenant.

A recorded exception to "no caller-supplied scope identifiers, in any mode, ever".
That rule governs the agent surface and still holds there: on /mcp an agent reads
only the tenant bound to its session. The authenticated admin console is a new actor
class, and its tenant switcher is a caller-supplied scope identifier by design. What
contains the exception is that the policy below is FOR SELECT, so it never reaches a
write.

Revision ID: 0003
Revises: 0002
"""

from alembic import op

from keepsake.store import ADMIN_GUC, ADMIN_POLICY, SCHEMA

revision = "0003"
down_revision = "0002"
branch_labels = None
depends_on = None

# Restated rather than imported: a module named 0001_initial is not importable.
TABLES = ("concept", "concept_revision")


def upgrade() -> None:
    for table in TABLES:
        # FOR SELECT, so UPDATE and DELETE consult tenant_isolation alone and stay
        # tenant-scoped even for an admin. Permissive policies OR together, so a
        # SELECT matches on the tenant or on the admin GUC.
        #
        # No TO clause. 0001 grants by name only when okf_app exists, because an
        # operator in postgres.mode: existing wires their own role by hand, and a
        # policy pinned to a role name would never apply to them.
        op.execute(f"""
            CREATE POLICY {ADMIN_POLICY} ON {SCHEMA}.{table} FOR SELECT
              USING (current_setting('{ADMIN_GUC}', true) = 'on')
        """)


def downgrade() -> None:
    for table in TABLES:
        op.execute(f"DROP POLICY {ADMIN_POLICY} ON {SCHEMA}.{table}")
