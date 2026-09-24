// Package pgtest shares one Postgres container across a test run, one database per package,
// with the admin, owner and app roles of 2de90d2:tests/conftest.py.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/roee-fs/keepsake/internal/store"
)

type DB struct{ AdminDSN, OwnerDSN, AppDSN string }

// Main runs the tests on the package's own database after setup, which migrates without an import cycle.
func Main(m *testing.M, setup func(*DB) error) {
	db, drop, err := start(context.Background())
	if err != nil {
		panic(err)
	}
	if err := setup(db); err != nil {
		drop()
		panic(err)
	}
	code := m.Run()
	drop()
	os.Exit(code)
}

// start joins or starts the session's container, which Ryuk reaps, and creates the package's database.
func start(ctx context.Context) (*DB, func(), error) {
	c, err := postgres.Run(ctx, "postgres:17",
		postgres.WithDatabase("keepsake"), postgres.WithUsername("postgres"), postgres.WithPassword("postgres"),
		// Every package's pools share this one server.
		testcontainers.WithCmdArgs("-c", "max_connections=1000"),
		testcontainers.WithReuseByName("keepsake-pgtest-"+testcontainers.SessionID()[:16]),
		postgres.BasicWaitStrategies())
	if err != nil {
		return nil, nil, err
	}
	server, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, nil, err
	}
	conn, err := pgx.Connect(ctx, server)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(ctx)
	name := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(filepath.Base(os.Args[0]), "_") + fmt.Sprint("_", os.Getpid())
	// Serialised across packages: roles are cluster-wide.
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('keepsake-pgtest'));
			DO $$ BEGIN
			  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'okf_owner') THEN
			    CREATE ROLE okf_owner LOGIN PASSWORD 'owner';
			    CREATE ROLE okf_app LOGIN PASSWORD 'app';
			  END IF;
			END $$`)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	for _, s := range []string{
		"CREATE DATABASE " + name + " TEMPLATE template1",
		"GRANT CREATE ON DATABASE " + name + " TO okf_owner",
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			return nil, nil, err
		}
	}
	dsn := func(user, password string) string {
		u, _ := url.Parse(server)
		u.User = url.UserPassword(user, password)
		u.Path = "/" + name
		return u.String()
	}
	drop := func() {
		ctx := context.Background()
		if conn, err := pgx.Connect(ctx, server); err == nil {
			conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
			conn.Close(ctx)
		}
	}
	return &DB{AdminDSN: dsn("postgres", "postgres"), OwnerDSN: dsn("okf_owner", "owner"), AppDSN: dsn("okf_app", "app")}, drop, nil
}

// Exec runs each statement on its own, autocommitted, as dsn's role.
func Exec(t testing.TB, dsn string, statements ...string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, s := range statements {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

// LockOut makes db's database look down to the app role, until the returned func or t's end.
func (db *DB) LockOut(t testing.TB) (restore func()) {
	t.Helper()
	restore = func() {
		Exec(t, db.AdminDSN, `DO $$ BEGIN EXECUTE format('GRANT CONNECT ON DATABASE %I TO PUBLIC', current_database()); END $$`)
	}
	Exec(t, db.AdminDSN, `DO $$ BEGIN EXECUTE format('REVOKE CONNECT ON DATABASE %I FROM PUBLIC', current_database()); END $$`)
	t.Cleanup(restore)
	db.TerminateApp(t)
	return restore
}

// TerminateApp ends every app-role backend on db's database, as a restart or failover would.
func (db *DB) TerminateApp(t testing.TB) {
	t.Helper()
	Exec(t, db.AdminDSN, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "+
		"WHERE usename = 'okf_app' AND datname = current_database()")
}

// ConceptStore opens a store on dsn's okf schema, closed when t ends.
func ConceptStore(t testing.TB, dsn string) *store.ConceptStore {
	t.Helper()
	s, err := store.Open(context.Background(), dsn, "okf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return store.NewConceptStore(s)
}

// AssertUnprivileged fails t if dsn logs in as a role exempt from row-level security.
func AssertUnprivileged(t testing.TB, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var name string
	var super, bypass bool
	err = conn.QueryRow(ctx, `SELECT usename, usesuper, usebypassrls FROM pg_user WHERE usename = current_user`).Scan(&name, &super, &bypass)
	if err != nil {
		t.Fatal(err)
	}
	// The DSN carries a password, so name the role instead.
	if super || bypass {
		t.Fatalf("role %q is exempt from RLS: superuser=%v bypassrls=%v", name, super, bypass)
	}
}
