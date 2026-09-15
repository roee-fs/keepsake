"""Initial okf schema: concepts, revisions, tenant isolation, purge.

Revision ID: 0001
Revises:
"""

from alembic import op

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None

TABLES = ("concept", "concept_revision")


def upgrade() -> None:
    op.execute("CREATE SCHEMA IF NOT EXISTS okf")
    # to_tsvector needs its regconfig argument: the one-argument form is only
    # STABLE, and Postgres rejects it in a generated column.
    op.execute("""
    CREATE TABLE okf.concept (
      tenant_id   uuid        NOT NULL,
      path        text        NOT NULL,
      type        text        NOT NULL,
      title       text        NOT NULL DEFAULT '',
      description text        NOT NULL DEFAULT '',
      body        text        NOT NULL DEFAULT '',
      frontmatter jsonb       NOT NULL DEFAULT '{}'::jsonb,
      links       text[]      NOT NULL DEFAULT '{}',
      search      tsvector GENERATED ALWAYS AS (
                    setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
                    setweight(to_tsvector('english', coalesce(description, '')), 'B') ||
                    setweight(to_tsvector('english', coalesce(body, '')), 'C')
                  ) STORED,
      version     int         NOT NULL DEFAULT 1,
      updated_by  text,
      created_at  timestamptz NOT NULL DEFAULT now(),
      updated_at  timestamptz NOT NULL DEFAULT now(),
      PRIMARY KEY (tenant_id, path)
    )""")
    op.execute("""
    CREATE TABLE okf.concept_revision (
      id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
      tenant_id   uuid        NOT NULL,
      path        text        NOT NULL,
      version     int         NOT NULL,
      op          text        NOT NULL,
      snapshot    jsonb       NOT NULL,
      updated_by  text,
      created_at  timestamptz NOT NULL DEFAULT now(),
      UNIQUE (tenant_id, path, version)
    )""")
    op.execute("CREATE INDEX ON okf.concept USING gin (search)")
    op.execute("CREATE INDEX ON okf.concept USING gin (links)")

    for table in TABLES:
        op.execute(f"ALTER TABLE okf.{table} ENABLE ROW LEVEL SECURITY")
        # FORCE subjects the table owner to the policy as well.
        op.execute(f"ALTER TABLE okf.{table} FORCE ROW LEVEL SECURITY")
        op.execute(f"""
            CREATE POLICY tenant_isolation ON okf.{table}
              USING      (tenant_id = current_setting('okf.current_tenant')::uuid)
              WITH CHECK (tenant_id = current_setting('okf.current_tenant')::uuid)
        """)

    op.execute("""
    CREATE FUNCTION okf.purge_tenant(t uuid) RETURNS void
    LANGUAGE sql SECURITY DEFINER SET search_path = okf, pg_catalog AS $$
      DELETE FROM okf.concept_revision WHERE tenant_id = t;
      DELETE FROM okf.concept          WHERE tenant_id = t;
    $$""")


def downgrade() -> None:
    op.execute("DROP FUNCTION okf.purge_tenant(uuid)")
    for table in TABLES:
        op.execute(f"DROP TABLE okf.{table}")
