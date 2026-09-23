// Package migrate replays the Alembic migrations under
// 8f2af2e:src/keepsake/store/migrations/versions/ in place, without Python or Alembic
// installed. It reads and writes the same <schema>.alembic_version table Alembic
// does, so a database Alembic migrated upgrades from wherever it left off.
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"text/template"

	"github.com/jackc/pgx/v5"

	"github.com/roee-fs/keepsake/internal/store"
)

//go:embed sql/*.sql.tmpl
var sqlFS embed.FS

// appRole is the role migration 0001 grants by name when it exists. Restated
// rather than imported, matching 2de90d2:src/keepsake/store/migrations/versions/0001_initial.py.
const appRole = "okf_app"

type fields struct {
	SCHEMA       string
	TENANT_GUC   string
	ADMIN_GUC    string
	ADMIN_POLICY string
	APP_ROLE     string
}

type migration struct {
	revision string
	tmpl     *template.Template
}

var migrations = loadMigrations()

func loadMigrations() []migration {
	entries, err := sqlFS.ReadDir("sql")
	if err != nil {
		panic(err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		revision := strings.TrimSuffix(e.Name(), ".sql.tmpl")
		b, err := sqlFS.ReadFile("sql/" + e.Name())
		if err != nil {
			panic(err)
		}
		tmpl, err := template.New(revision).Parse(string(b))
		if err != nil {
			panic(err)
		}
		out = append(out, migration{revision: revision, tmpl: tmpl})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].revision < out[j].revision })
	return out
}

// Head returns the id of the last embedded migration.
func Head() string {
	return migrations[len(migrations)-1].revision
}

// Up applies every migration after schema's current alembic_version, in a single
// transaction, the way Alembic applies them. Run again at head, it changes nothing.
func Up(ctx context.Context, dsn, schema string) error {
	schema, err := store.ValidatedSchema(schema)
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('keepsake-migrate'))`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)); err != nil {
		return err
	}
	// Alembic's own DDL for its bookkeeping table.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.alembic_version (
		version_num varchar(32) NOT NULL,
		CONSTRAINT alembic_version_pkc PRIMARY KEY (version_num)
	)`, schema)); err != nil {
		return err
	}

	var current string
	err = tx.QueryRow(ctx, fmt.Sprintf("SELECT version_num FROM %s.alembic_version", schema)).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if current != "" && !slices.ContainsFunc(migrations, func(m migration) bool { return m.revision == current }) {
		return fmt.Errorf("Can't locate revision identified by '%s'", current)
	}

	f := fields{
		SCHEMA:       schema,
		TENANT_GUC:   store.TenantGUC,
		ADMIN_GUC:    store.AdminGUC,
		ADMIN_POLICY: store.AdminPolicy,
		APP_ROLE:     appRole,
	}
	for _, m := range migrations {
		if m.revision <= current {
			continue
		}
		var sql strings.Builder
		if err := m.tmpl.Execute(&sql, f); err != nil {
			return fmt.Errorf("migration %s: %w", m.revision, err)
		}
		if _, err := tx.Exec(ctx, sql.String()); err != nil {
			return fmt.Errorf("migration %s: %w", m.revision, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s.alembic_version", schema)); err != nil {
			return fmt.Errorf("migration %s: %w", m.revision, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"INSERT INTO %s.alembic_version (version_num) VALUES ($1)", schema), m.revision); err != nil {
			return fmt.Errorf("migration %s: %w", m.revision, err)
		}
		current = m.revision
	}

	return tx.Commit(ctx)
}
