"""Rewrite admin_read so scoped reads keep the tenant index.

0003's bare GUC test, ORed with tenant_isolation, hid tenant_id from every index,
so each tenant-scoped read scanned every tenant's rows.

Revision ID: 0004
Revises: 0003
"""

from alembic import op

from keepsake.store import ADMIN_GUC, ADMIN_POLICY, SCHEMA

revision = "0004"
down_revision = "0003"
branch_labels = None
depends_on = None

TABLES = ("concept", "concept_revision")

# A range on tenant_id, so the planner can BitmapOr it with tenant_isolation's
# equality. With the GUC off the bound is NULL and the arm matches nothing.
QUAL = (
    f"tenant_id >= CASE WHEN current_setting('{ADMIN_GUC}', true) = 'on' "
    "THEN '00000000-0000-0000-0000-000000000000'::uuid END"
)


def upgrade() -> None:
    for table in TABLES:
        op.execute(f"ALTER POLICY {ADMIN_POLICY} ON {SCHEMA}.{table} USING ({QUAL})")


def downgrade() -> None:
    for table in TABLES:
        op.execute(
            f"ALTER POLICY {ADMIN_POLICY} ON {SCHEMA}.{table} "
            f"USING (current_setting('{ADMIN_GUC}', true) = 'on')"
        )
