package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/store"
)

var db *pgtest.DB

// Ported from 2de90d2:tests/conftest.py's session-scoped fixtures: one container shared by
// every test in the package, migrated once, as the owner runs it in production.
func TestMain(m *testing.M) {
	ctx := context.Background()
	d, cleanup, err := pgtest.Start(ctx)
	if err != nil {
		panic(err)
	}
	if err := Up(ctx, d.OwnerDSN, "okf"); err != nil {
		cleanup()
		panic(err)
	}
	db = d
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// scratchSchema drops schema when the test ends, so -count=2 and the public-namespace
// check see only okf.
func scratchSchema(t *testing.T, schema string) string {
	t.Helper()
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), db.OwnerDSN)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	return schema
}

// appConn connects as the app role, enforced unprivileged: no test can take the
// DSN without the check, the way 2de90d2:tests/conftest.py's pg_dsn fixture is built.
func appConn(t *testing.T) *pgx.Conn {
	t.Helper()
	pgtest.AssertUnprivileged(t, db.AppDSN)
	return connect(t, db.AppDSN)
}

func scope(t *testing.T, tx pgx.Tx, tenant uuid.UUID) {
	t.Helper()
	if _, err := tx.Exec(context.Background(),
		"SELECT set_config('okf.current_tenant', $1, true)", tenant.String()); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, tx pgx.Tx, tenant uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := tx.Exec(ctx,
		"INSERT INTO okf.concept (tenant_id, path, type) VALUES ($1, 'a.md', 'note')", tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO okf.concept_revision (tenant_id, path, version, op, snapshot) "+
			"VALUES ($1, 'a.md', 1, 'create', '{}'::jsonb)", tenant); err != nil {
		t.Fatal(err)
	}
}

// Ported from 2de90d2:tests/test_migration.py::test_tables_have_rls_enabled_and_forced.
func TestTablesHaveRLSEnabledAndForced(t *testing.T) {
	conn := appConn(t)
	rows, err := conn.Query(context.Background(), `
		SELECT relname, relrowsecurity, relforcerowsecurity
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'okf' AND c.relkind = 'r' AND c.relname <> 'alembic_version'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		found = true
		var name string
		var enabled, forced bool
		if err := rows.Scan(&name, &enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled {
			t.Errorf("%s has RLS disabled", name)
		}
		if !forced {
			t.Errorf("%s does not FORCE RLS", name)
		}
	}
	if !found {
		t.Fatal("no tables created")
	}
}

// Ported from 2de90d2:tests/test_migration.py::test_policies_read_the_okf_guc.
func TestPoliciesReadTheOkfGUC(t *testing.T) {
	conn := appConn(t)
	rows, err := conn.Query(context.Background(),
		"SELECT tablename, policyname, cmd, qual, with_check FROM pg_policies WHERE schemaname = 'okf'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type policy struct {
		table, name, cmd string
		qual, withCheck  *string
	}
	var got []policy
	seen := map[[2]string]bool{}
	for rows.Next() {
		var p policy
		if err := rows.Scan(&p.table, &p.name, &p.cmd, &p.qual, &p.withCheck); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
		seen[[2]string{p.table, p.name}] = true
	}
	want := map[[2]string]bool{}
	for _, table := range []string{"concept", "concept_revision"} {
		for _, name := range []string{"tenant_isolation", "admin_read"} {
			want[[2]string{table, name}] = true
		}
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("policies = %v, want %v", seen, want)
	}

	for _, p := range got {
		if p.name == "admin_read" {
			// The one cross-tenant policy, so what keeps it off the write path is
			// that it applies to no other command.
			if p.cmd != "SELECT" {
				t.Errorf("%s.%s also applies to %s", p.table, p.name, p.cmd)
			}
			if p.qual == nil || !strings.Contains(*p.qual, "current_setting('okf.admin'") {
				t.Errorf("%s.%s does not read okf.admin: %v", p.table, p.name, p.qual)
			}
			continue
		}
		for _, clause := range []struct {
			name string
			expr *string
		}{{"USING", p.qual}, {"WITH CHECK", p.withCheck}} {
			where := fmt.Sprintf("%s.%s %s", p.table, p.name, clause.name)
			// A null expression is one Postgres does not apply: WITH CHECK absent is
			// WITH CHECK (true), which reads one tenant and writes any.
			if clause.expr == nil {
				t.Errorf("%s has no expression", where)
				continue
			}
			if !strings.Contains(*clause.expr, "current_setting('okf.current_tenant'") {
				t.Errorf("%s does not read okf.current_tenant: %s", where, *clause.expr)
			}
			if !strings.Contains(*clause.expr, "tenant_id") {
				t.Errorf("%s ignores tenant_id: %s", where, *clause.expr)
			}
		}
	}
}

// Ported from 2de90d2:tests/test_migration.py::test_the_app_role_owns_no_tables.
func TestTheAppRoleOwnsNoTables(t *testing.T) {
	conn := appConn(t)
	rows, err := conn.Query(context.Background(), `
		SELECT relname, pg_get_userbyid(relowner),
		       has_table_privilege('okf_app', c.oid, 'TRUNCATE')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'okf' AND c.relkind = 'r'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		found = true
		var name, owner string
		var truncatable bool
		if err := rows.Scan(&name, &owner, &truncatable); err != nil {
			t.Fatal(err)
		}
		if owner != "okf_owner" {
			t.Errorf("okf.%s is owned by %s", name, owner)
		}
		// TRUNCATE is not filtered by a policy, so it would empty every tenant.
		if truncatable {
			t.Errorf("okf_app may TRUNCATE okf.%s", name)
		}
	}
	if !found {
		t.Fatal("no tables created")
	}
}

// Ported from 2de90d2:tests/test_migration.py::test_alembic_version_table_is_not_in_public.
func TestAlembicVersionTableIsNotInPublic(t *testing.T) {
	conn := appConn(t)
	rows, err := conn.Query(context.Background(), `
		SELECT n.nspname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = 'alembic_version'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			t.Fatal(err)
		}
		got = append(got, ns)
	}
	if !reflect.DeepEqual(got, []string{"okf"}) {
		t.Fatalf("namespaces = %v, want [okf]", got)
	}
}

// Ported from 2de90d2:tests/test_migration.py::test_purge_tenant_returns_the_count_and_empties_both_tables.
func TestPurgeTenantReturnsTheCountAndEmptiesBothTables(t *testing.T) {
	ctx := context.Background()
	conn := appConn(t)
	tenant := uuid.New()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	scope(t, tx, tenant)
	seed(t, tx, tenant)

	var purged int64
	if err := tx.QueryRow(ctx, "SELECT okf.purge_tenant($1)", tenant).Scan(&purged); err != nil {
		t.Fatal(err)
	}
	var concepts, revisions int64
	if err := tx.QueryRow(ctx,
		"SELECT (SELECT count(*) FROM okf.concept), (SELECT count(*) FROM okf.concept_revision)",
	).Scan(&concepts, &revisions); err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Errorf("purged = %d, want 1", purged)
	}
	if concepts != 0 || revisions != 0 {
		t.Errorf("remaining = (%d, %d), want (0, 0)", concepts, revisions)
	}
}

// Ported from 2de90d2:tests/test_migration.py::test_purge_tenant_refuses_a_tenant_the_session_is_not_scoped_to.
func TestPurgeTenantRefusesATenantTheSessionIsNotScopedTo(t *testing.T) {
	ctx := context.Background()
	conn := appConn(t)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	scoped, other := uuid.New(), uuid.New()
	scope(t, tx, scoped)

	_, err = tx.Exec(ctx, "SELECT okf.purge_tenant($1)", other)
	var pgErr *pgconn.PgError
	// A silent no-op reads to the operator as a completed purge.
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("err = %v, want a RAISE EXCEPTION (P0001)", err)
	}
}

// Ported from 2de90d2:tests/test_migration.py::test_a_tenant_cannot_read_or_write_another_tenants_rows.
func TestATenantCannotReadOrWriteAnotherTenantsRows(t *testing.T) {
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()

	tx1, err := appConn(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope(t, tx1, a)
	seed(t, tx1, a)
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tx2, err := appConn(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope(t, tx2, b)
	var count int64
	if err := tx2.QueryRow(ctx, "SELECT count(*) FROM okf.concept").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	// A WITH CHECK violation is 42501, not a check-constraint violation.
	_, err = tx2.Exec(ctx,
		"INSERT INTO okf.concept (tenant_id, path, type) VALUES ($1, 'planted.md', 'note')", a)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("err = %v, want InsufficientPrivilege (42501)", err)
	}
	tx2.Rollback(ctx)

	tx3, err := appConn(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope(t, tx3, a)
	var purged int64
	if err := tx3.QueryRow(ctx, "SELECT okf.purge_tenant($1)", a).Scan(&purged); err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Errorf("purged = %d, want 1", purged)
	}
	tx3.Commit(ctx)
}

// schema.go's own comment explains why: the name is formatted into DDL, never bound.
func TestUpRejectsAnInvalidSchemaName(t *testing.T) {
	err := Up(context.Background(), "postgres://unreachable-host:5432/nope", "okf; DROP TABLE x")
	if err == nil {
		t.Fatal("Up did not reject an invalid schema name")
	}
	want := "not a usable schema name: 'okf; DROP TABLE x'"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q (Up must validate before it ever connects)", err.Error(), want)
	}
}

// Ported from 2de90d2:tests/test_schema_name.py::test_a_non_default_schema_migrates_and_serves, the
// migration half: the ConceptStore/Store half has no Go port yet (a later task).
func TestANonDefaultSchemaMigrates(t *testing.T) {
	ctx := context.Background()
	if err := Up(ctx, db.OwnerDSN, scratchSchema(t, "okf_elsewhere")); err != nil {
		t.Fatal(err)
	}
	conn := connect(t, db.OwnerDSN)
	rows, err := conn.Query(ctx, `
		SELECT relname FROM pg_class
		WHERE relnamespace = $1::regnamespace AND relkind = 'r'
		AND relrowsecurity AND relforcerowsecurity ORDER BY relname`, "okf_elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	want := []string{"concept", "concept_revision"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("guarded tables = %v, want %v", got, want)
	}
}

// A fresh database at each revision an older Python release left it at. The fixture
// is built from the same embedded templates as Up itself.
func TestUpgradesADatabaseAlembicMigrated(t *testing.T) {
	for _, left := range []string{"0002", "0003"} {
		t.Run(left, func(t *testing.T) { upgradeFrom(t, left) })
	}
}

func upgradeFrom(t *testing.T, left string) {
	ctx := context.Background()
	schema := scratchSchema(t, "okf_from_"+left)
	conn := connect(t, db.OwnerDSN)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := fields{
		SCHEMA: schema, TENANT_GUC: store.TenantGUC, ADMIN_GUC: store.AdminGUC,
		ADMIN_POLICY: store.AdminPolicy, APP_ROLE: appRole,
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.alembic_version (
		version_num varchar(32) NOT NULL,
		CONSTRAINT alembic_version_pkc PRIMARY KEY (version_num)
	)`, schema)); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.revision > left {
			break
		}
		var sql strings.Builder
		if err := m.tmpl.Execute(&sql, f); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, sql.String()); err != nil {
			t.Fatalf("migration %s: %v", m.revision, err)
		}
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		"INSERT INTO %s.alembic_version (version_num) VALUES ($1)", schema), left); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := Up(ctx, db.OwnerDSN, schema); err != nil {
		t.Fatal(err)
	}

	var version string
	if err := conn.QueryRow(ctx,
		fmt.Sprintf("SELECT version_num FROM %s.alembic_version", schema)).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "0004" {
		t.Fatalf("version_num = %s, want 0004", version)
	}

	// The startup check pins admin_read to 0004's exact expression.
	s, err := store.Open(ctx, db.AppDSN, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := store.Verify(ctx, s, schema); err != nil {
		t.Fatalf("the upgraded schema fails the startup check: %v", err)
	}
}

// helm upgrade re-runs the hook against a database already at head. MUST succeed and
// change nothing.
func TestUpIsIdempotentAtHead(t *testing.T) {
	ctx := context.Background()
	schema := scratchSchema(t, "okf_idempotent")
	if err := Up(ctx, db.OwnerDSN, schema); err != nil {
		t.Fatal(err)
	}
	if err := Up(ctx, db.OwnerDSN, schema); err != nil {
		t.Fatalf("second Up at head: %v", err)
	}
	conn := connect(t, db.OwnerDSN)
	var version string
	if err := conn.QueryRow(ctx,
		fmt.Sprintf("SELECT version_num FROM %s.alembic_version", schema)).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != Head() {
		t.Fatalf("version_num = %s, want %s", version, Head())
	}
}

// A database a newer release migrated is refused, as Alembic refuses it.
func TestUpRefusesARevisionItDoesNotKnow(t *testing.T) {
	ctx := context.Background()
	schema := scratchSchema(t, "okf_newer")
	if err := Up(ctx, db.OwnerDSN, schema); err != nil {
		t.Fatal(err)
	}
	conn := connect(t, db.OwnerDSN)
	if _, err := conn.Exec(ctx, fmt.Sprintf("UPDATE %s.alembic_version SET version_num = '0099'", schema)); err != nil {
		t.Fatal(err)
	}
	err := Up(ctx, db.OwnerDSN, schema)
	if want := "Can't locate revision identified by '0099'"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}
