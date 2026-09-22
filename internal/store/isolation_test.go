// Ported from tests/test_isolation.py: tenant isolation as seen through Store, the
// only place the GUC is set. tests/test_migration.py proves the policies; these prove
// the connection handling above them scopes every statement and leaves no scope
// behind on a pooled connection.
//
// package store_test, not store: TestMain must migrate a database before any test
// runs, and internal/migrate imports internal/store, so an internal test file here
// importing internal/migrate would be an import cycle.
package store_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/roee-fs/keepsake/internal/migrate"
	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/store"
)

var (
	db   *pgtest.DB
	A, B = uuid.New(), uuid.New()
	ctx  = context.Background()
)

// probeAttempts is enough acquisitions to cycle any plausible pool; exceeding it
// fails the probe.
const probeAttempts = 64

func TestMain(m *testing.M) {
	d, cleanup, err := pgtest.Start(ctx)
	if err != nil {
		panic(err)
	}
	if err := migrate.Up(ctx, d.OwnerDSN, "okf"); err != nil {
		cleanup()
		panic(err)
	}
	db = d
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func openApp(t *testing.T) *store.Store {
	t.Helper()
	pgtest.AssertUnprivileged(t, db.AppDSN)
	s, err := store.Open(ctx, db.AppDSN, "okf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func insert(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, path string) error {
	_, err := tx.Exec(ctx,
		"INSERT INTO okf.concept (tenant_id, path, type) VALUES ($1, $2, 'Concept')", tenant, path)
	return err
}

// owners is the tenants owning paths that this connection can actually see.
func owners(ctx context.Context, tx pgx.Tx, paths []string) (map[uuid.UUID]bool, error) {
	rows, err := tx.Query(ctx, "SELECT tenant_id FROM okf.concept WHERE path = ANY($1)", paths)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var tenant uuid.UUID
		if err := rows.Scan(&tenant); err != nil {
			return nil, err
		}
		out[tenant] = true
	}
	return out, rows.Err()
}

func pgErrCode(t *testing.T, err error) string {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want a *pgconn.PgError", err)
	}
	return pgErr.Code
}

// probeUntilScopedConnectionReturns takes raw connections until one of scoped is
// re-handed, checking each: self-verifying rather than pool-geometry-dependent, so
// a pool too large to cycle fails here instead of silently ceasing to test the leak
// path. Both GUCs are checked because okf.admin riding a recycled connection to the
// next caller is a cross-tenant read exactly as okf.current_tenant leaking is.
func probeUntilScopedConnectionReturns(t *testing.T, s *store.Store, scoped map[uint32]bool) {
	t.Helper()
	for range probeAttempts {
		var pid uint32
		var tenant, admin *string
		err := s.Raw(ctx, func(tx pgx.Tx) error {
			pid = tx.Conn().PgConn().PID()
			return tx.QueryRow(ctx,
				"SELECT current_setting('okf.current_tenant', true), current_setting('okf.admin', true)",
			).Scan(&tenant, &admin)
		})
		if err != nil {
			t.Fatal(err)
		}
		if (tenant != nil && *tenant != "") || (admin != nil && *admin != "") {
			t.Fatalf("scope outlived its transaction: %v %v", tenant, admin)
		}
		if scoped[pid] {
			return
		}
	}
	t.Fatal("no scoped connection came back; the leak path went untested")
}

func TestReadIsolation(t *testing.T) {
	s := openApp(t)
	err := s.Scope(ctx, A, func(tx pgx.Tx) error {
		if err := insert(ctx, tx, A, "a/one"); err != nil {
			return err
		}
		var count int
		// The positive control: a policy matching nothing also returns 0 below.
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM okf.concept WHERE path = 'a/one'").Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.Scope(ctx, B, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM okf.concept").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Errorf("count = %d, want 0", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Catches a WITH CHECK that does not constrain tenant_id. Omitting WITH CHECK
// entirely is not that defect: Postgres then applies the USING expression to new
// rows, which is the same constraint.
func TestCannotInsertForAnotherTenant(t *testing.T) {
	s := openApp(t)
	scoped := map[uint32]bool{}
	err := s.Scope(ctx, A, func(tx pgx.Tx) error {
		// Recorded before the insert, which raises and skips the rest of fn.
		scoped[tx.Conn().PgConn().PID()] = true
		return insert(ctx, tx, B, "a/two")
	})
	if code := pgErrCode(t, err); code != "42501" {
		t.Fatalf("code = %s, want InsufficientPrivilege (42501)", code)
	}
	// The transaction aborted, so the scope must be gone from that connection too.
	probeUntilScopedConnectionReturns(t, s, scoped)
}

// TestIsolationHoldsForEveryReadShape is deferred: it needs ConceptStore's search,
// grep and backlinks queries (tests/test_isolation.py::test_isolation_holds_for_every_read_shape),
// which do not exist in Go yet. Owned by Tasks 8-9.

func TestUnsetScopeRaisesRatherThanReturningEverything(t *testing.T) {
	s := openApp(t)
	err := s.Raw(ctx, func(tx pgx.Tx) error {
		var count int
		return tx.QueryRow(ctx, "SELECT count(*) FROM okf.concept").Scan(&count)
	})
	// Either SQLSTATE is correct: a never-assigned GUC is undefined (42704), a
	// reset one fails the ::uuid cast on '' (22P02).
	if code := pgErrCode(t, err); code != "42704" && code != "22P02" {
		t.Fatalf("code = %s, want undefined_object (42704) or invalid_text_representation (22P02)", code)
	}
}

// raw() is the one unscoped connection in the codebase, so a write through it would
// reach every tenant's rows at once. The statement is DDL, because a write to
// concept is refused by the policy first and would fail either way.
func TestRawIsReadOnlyAndNotMerelyDocumented(t *testing.T) {
	s := openApp(t)
	err := s.Raw(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "CREATE TABLE okf.should_never_exist (x int)")
		return err
	})
	if code := pgErrCode(t, err); code != "25006" {
		t.Fatalf("code = %s, want ReadOnlySqlTransaction (25006)", code)
	}
}

func TestAdminScopeReadsEveryTenant(t *testing.T) {
	s := openApp(t)
	one, two := uuid.New(), uuid.New()
	paths := []string{"admin/" + one.String(), "admin/" + two.String()}
	for i, tenant := range []uuid.UUID{one, two} {
		path := paths[i]
		if err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
			return insert(ctx, tx, tenant, path)
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The positive control: the same query under a tenant scope sees one of the two.
	err := s.Scope(ctx, one, func(tx pgx.Tx) error {
		got, err := owners(ctx, tx, paths)
		if err != nil {
			return err
		}
		if want := (map[uuid.UUID]bool{one: true}); !reflect.DeepEqual(got, want) {
			t.Errorf("owners = %v, want %v", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	scoped := map[uint32]bool{}
	err = s.AdminScope(ctx, func(tx pgx.Tx) error {
		scoped[tx.Conn().PgConn().PID()] = true
		got, err := owners(ctx, tx, paths)
		if err != nil {
			return err
		}
		if want := (map[uuid.UUID]bool{one: true, two: true}); !reflect.DeepEqual(got, want) {
			t.Errorf("owners = %v, want %v", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both GUCs are SET LOCAL, so neither may ride this connection back out.
	probeUntilScopedConnectionReturns(t, s, scoped)
}

// The policy is FOR SELECT, so an admin UPDATE stays scoped to the nil tenant and
// matches no rows silently, which reads as a write that changed nothing. The
// read-only transaction is what raises instead.
func TestAdminScopeCannotWrite(t *testing.T) {
	s := openApp(t)
	err := s.AdminScope(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE okf.concept SET title = 'x'")
		return err
	})
	if code := pgErrCode(t, err); code != "25006" {
		t.Fatalf("code = %s, want ReadOnlySqlTransaction (25006)", code)
	}
}

// Pins the FOR SELECT half, which the test above cannot reach: this sets the admin
// GUC on an ordinary writable connection, where only the policy's command scope
// stops the UPDATE.
func TestTheAdminGUCDoesNotWidenAWrite(t *testing.T) {
	s := openApp(t)
	one, two := uuid.New(), uuid.New()
	paths := []string{"write/" + one.String(), "write/" + two.String()}
	for i, tenant := range []uuid.UUID{one, two} {
		path := paths[i]
		if err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
			return insert(ctx, tx, tenant, path)
		}); err != nil {
			t.Fatal(err)
		}
	}

	var updated int64
	err := s.Scope(ctx, one, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('okf.admin', 'on', true)"); err != nil {
			return err
		}
		// No WHERE, so the count is exactly the rows the policies let it reach.
		tag, err := tx.Exec(ctx, "UPDATE okf.concept SET title = 'claimed'")
		if err != nil {
			return err
		}
		updated = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated != 1 {
		t.Fatalf("the admin GUC widened an UPDATE to %d rows", updated)
	}

	titles := map[string]string{}
	err = s.AdminScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT path, title FROM okf.concept WHERE path = ANY($1)", paths)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var path, title string
			if err := rows.Scan(&path, &title); err != nil {
				return err
			}
			titles[path] = title
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{paths[0]: "claimed", paths[1]: ""}; !reflect.DeepEqual(titles, want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
}

// Catches SET instead of SET LOCAL.
func TestScopeDoesNotLeakAcrossPooledConnections(t *testing.T) {
	s := openApp(t)
	scoped := map[uint32]bool{}
	for range 4 {
		_ = s.Scope(ctx, uuid.New(), func(tx pgx.Tx) error {
			scoped[tx.Conn().PgConn().PID()] = true
			return nil
		})
	}
	for range 64 {
		var pid uint32
		var tenant, admin *string
		_ = s.Raw(ctx, func(tx pgx.Tx) error {
			pid = tx.Conn().PgConn().PID()
			return tx.QueryRow(ctx, `SELECT current_setting('okf.current_tenant', true), current_setting('okf.admin', true)`).Scan(&tenant, &admin)
		})
		if (tenant != nil && *tenant != "") || (admin != nil && *admin != "") {
			t.Fatalf("scope outlived its transaction: %v %v", tenant, admin)
		}
		if scoped[pid] {
			return
		}
	}
	t.Fatal("never re-drew a scoped connection: the probe proved nothing")
}

// TestIsUnavailable is new: no Python test kills backends. It opens its own store so
// killing every okf_app backend cannot poison other tests running in this package.
func TestIsUnavailable(t *testing.T) {
	s, err := store.Open(ctx, db.AppDSN, "okf")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Force at least one physical connection to exist before killing it.
	if err := s.Raw(ctx, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}

	admin, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = 'okf_app'"); err != nil {
		t.Fatal(err)
	}

	scopeErr := s.Scope(ctx, uuid.New(), func(pgx.Tx) error { return nil })
	if scopeErr == nil {
		t.Fatal("Scope succeeded after every okf_app backend was terminated")
	}
	if !store.IsUnavailable(scopeErr) {
		t.Errorf("IsUnavailable(%v) = false, want true", scopeErr)
	}

	s2 := openApp(t)
	dupErr := s2.Scope(ctx, A, func(tx pgx.Tx) error {
		if err := insert(ctx, tx, A, "dup/path"); err != nil {
			return err
		}
		return insert(ctx, tx, A, "dup/path")
	})
	if code := pgErrCode(t, dupErr); code != "23505" {
		t.Fatalf("code = %s, want unique_violation (23505)", code)
	}
	if store.IsUnavailable(dupErr) {
		t.Errorf("IsUnavailable(%v) = true, want false for a unique violation", dupErr)
	}
}
