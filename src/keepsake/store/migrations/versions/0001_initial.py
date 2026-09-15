"""Initial schema: concepts, revisions, tenant isolation, purge.

Revision ID: 0001
Revises:
"""

from alembic import op

from keepsake.store import SCHEMA, TENANT_GUC

revision = "0001"
down_revision = None
branch_labels = None
depends_on = None

TABLES = ("concept", "concept_revision")
# The role the application connects as. Grants are skipped if it does not exist.
APP_ROLE = "okf_app"


def upgrade() -> None:
    op.execute(f"CREATE SCHEMA IF NOT EXISTS {SCHEMA}")
    # to_tsvector needs its regconfig argument: the one-argument form is only
    # STABLE, and Postgres rejects it in a generated column.
    op.execute(f"""
    CREATE TABLE {SCHEMA}.concept (
      tenant_id   uuid        NOT NULL,
      path        text        NOT NULL,
      type        text        NOT NULL,
      title       text        NOT NULL DEFAULT '',
      description text        NOT NULL DEFAULT '',
      body        text        NOT NULL DEFAULT '',
      frontmatter jsonb       NOT NULL DEFAULT '{{}}'::jsonb,
      links       text[]      NOT NULL DEFAULT '{{}}',
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
    op.execute(f"""
    CREATE TABLE {SCHEMA}.concept_revision (
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
    op.execute(f"CREATE INDEX ON {SCHEMA}.concept USING gin (search)")
    op.execute(f"CREATE INDEX ON {SCHEMA}.concept USING gin (links)")

    for table in TABLES:
        op.execute(f"ALTER TABLE {SCHEMA}.{table} ENABLE ROW LEVEL SECURITY")
        # FORCE subjects the table owner to the policy as well.
        op.execute(f"ALTER TABLE {SCHEMA}.{table} FORCE ROW LEVEL SECURITY")
        op.execute(f"""
            CREATE POLICY tenant_isolation ON {SCHEMA}.{table}
              USING      (tenant_id = current_setting('{TENANT_GUC}')::uuid)
              WITH CHECK (tenant_id = current_setting('{TENANT_GUC}')::uuid)
        """)

    # The function is subject to the policy like every other caller, so it can only
    # purge the scoped tenant. The returned count is of concepts, not of revisions.
    op.execute(f"""
    CREATE FUNCTION {SCHEMA}.purge_tenant(t uuid) RETURNS bigint
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
    DECLARE n bigint;
    BEGIN
      IF t <> current_setting('{TENANT_GUC}')::uuid THEN
        RAISE EXCEPTION 'purge_tenant(%) requires the session scoped to that tenant', t;
      END IF;
      DELETE FROM {SCHEMA}.concept_revision WHERE tenant_id = t;
      DELETE FROM {SCHEMA}.concept WHERE tenant_id = t;
      GET DIAGNOSTICS n = ROW_COUNT;
      RETURN n;
    END $$""")

    # Granted by name: a sweep would also grant whatever a later migration adds.
    op.execute(f"""
    DO $$
    BEGIN
      IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '{APP_ROLE}') THEN
        GRANT USAGE ON SCHEMA {SCHEMA} TO {APP_ROLE};
        GRANT SELECT, INSERT, UPDATE, DELETE
          ON {SCHEMA}.concept, {SCHEMA}.concept_revision TO {APP_ROLE};
      END IF;
    END $$""")


def downgrade() -> None:
    op.execute(f"DROP FUNCTION {SCHEMA}.purge_tenant(uuid)")
    for table in TABLES:
        op.execute(f"DROP TABLE {SCHEMA}.{table}")
