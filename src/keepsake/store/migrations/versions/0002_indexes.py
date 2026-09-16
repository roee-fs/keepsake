"""Drop the two unreachable GIN indexes, constrain `op`, index the revision log.

Revision ID: 0002
Revises: 0001
"""

from alembic import op

from keepsake.store import SCHEMA

revision = "0002"
down_revision = "0001"
branch_labels = None
depends_on = None

# The two operations a revision can record. `purge_tenant` deletes rather than
# logging, so there is no third.
OPS = ("create", "update")


def upgrade() -> None:
    # Neither GIN index can ever be used. `ts_match_vq` and `arraycontains` are not
    # leakproof, so under FORCE ROW LEVEL SECURITY the planner refuses to promote
    # either to an index condition — measured on postgres 17 at 10,000 rows, the app
    # role gets a sequential scan every time while `row_security = off` gets a bitmap
    # index scan. They only buy write amplification (~11% on a 2000-row insert) and
    # disk. They come back if the isolation model ever changes.
    op.execute(f"DROP INDEX {SCHEMA}.concept_search_idx")
    op.execute(f"DROP INDEX {SCHEMA}.concept_links_idx")

    # `op` is written by this codebase alone, so the check is a guard against a
    # future writer, not against a caller.
    ops = ", ".join(f"'{name}'" for name in OPS)
    op.execute(
        f"ALTER TABLE {SCHEMA}.concept_revision "
        f"ADD CONSTRAINT concept_revision_op_check CHECK (op IN ({ops}))"
    )

    # What `revisions()` orders by. Leading with tenant_id because the policy adds
    # that predicate to every query; the rest matches the ORDER BY exactly, and a
    # btree scans backwards, so it serves the newest-first read too.
    op.execute(
        f"CREATE INDEX concept_revision_log_idx ON {SCHEMA}.concept_revision "
        "(tenant_id, created_at, path, version)"
    )


def downgrade() -> None:
    op.execute(f"DROP INDEX {SCHEMA}.concept_revision_log_idx")
    op.execute(
        f"ALTER TABLE {SCHEMA}.concept_revision DROP CONSTRAINT concept_revision_op_check"
    )
    op.execute(f"CREATE INDEX ON {SCHEMA}.concept USING gin (links)")
    op.execute(f"CREATE INDEX ON {SCHEMA}.concept USING gin (search)")
