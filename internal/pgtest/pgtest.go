// Package pgtest starts a disposable Postgres container for tests, with the same
// three roles tests/conftest.py creates: an admin (the container superuser), an
// owner (migrations run as this role) and an app role (RLS applies to this one).
package pgtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

type DB struct{ AdminDSN, OwnerDSN, AppDSN string }

func Start(ctx context.Context) (*DB, func(), error) {
	c, err := postgres.Run(ctx, "postgres:17",
		postgres.WithDatabase("keepsake"), postgres.WithUsername("postgres"), postgres.WithPassword("postgres"),
		postgres.BasicWaitStrategies())
	if err != nil {
		return nil, nil, err
	}
	admin, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, nil, err
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(ctx)
	for _, s := range []string{
		`CREATE ROLE okf_owner LOGIN PASSWORD 'owner'`,
		`CREATE ROLE okf_app LOGIN PASSWORD 'app'`,
		`GRANT CREATE ON DATABASE keepsake TO okf_owner`,
	} {
		if _, err := conn.Exec(ctx, s); err != nil {
			return nil, nil, err
		}
	}
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "5432/tcp")
	dsn := func(u, p string) string {
		return fmt.Sprintf("postgres://%s:%s@%s:%s/keepsake?sslmode=disable", u, p, host, port.Port())
	}
	db := &DB{AdminDSN: admin, OwnerDSN: dsn("okf_owner", "owner"), AppDSN: dsn("okf_app", "app")}
	return db, func() { _ = c.Terminate(context.Background()) }, nil
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
