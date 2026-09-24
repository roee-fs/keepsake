// Ported from 8f2af2e:tests/test_isolation.py: tenant isolation as seen through Store, the
// only place the GUC is set. 2de90d2:tests/test_migration.py proves the policies; these prove
// the connection handling above them scopes every statement and leaves no scope
// behind on a pooled connection.
//
// package store_test, not store: TestMain MUST migrate a database before any test
// runs, and internal/migrate imports internal/store, so an internal test file here
// importing internal/migrate would be an import cycle.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/roee-fs/keepsake/internal/migrate"
	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

var (
	db  *pgtest.DB
	ctx = context.Background()
)

// probeAttempts is enough acquisitions to cycle any plausible pool; exceeding it
// fails the probe.
const probeAttempts = 64

func TestMain(m *testing.M) {
	pgtest.Main(m, func(d *pgtest.DB) error {
		db = d
		return migrate.Up(ctx, d.OwnerDSN, "okf")
	})
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
	// Fresh every run, not a package-level fixed pair: TestMain's database is
	// never reset between `go test -count=N` runs, so a shared A would collide
	// with the literal path a repeat run inserts under it.
	A, B := uuid.New(), uuid.New()
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
	A, B := uuid.New(), uuid.New()
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
	// The transaction aborted, so the scope MUST be gone from that connection too.
	probeUntilScopedConnectionReturns(t, s, scoped)
}

// TestIsolationHoldsForEveryReadShape ports
// 8f2af2e:tests/test_isolation.py::test_isolation_holds_for_every_read_shape: a policy can
// be right for one query shape and wrong for another.
func TestIsolationHoldsForEveryReadShape(t *testing.T) {
	A, B := uuid.New(), uuid.New()
	s := openApp(t)
	cs := store.NewConceptStore(s)
	if _, _, err := cs.Create(ctx, A, okf.Concept{
		Path: "x/y", Type: "Concept", Title: "secret", Body: "tenant a only",
		Links: []string{"t/z"},
	}, "seed"); err != nil {
		t.Fatal(err)
	}
	// The link target exists in both tenants, so B has a concept to read backlinks of.
	for _, tenant := range []uuid.UUID{A, B} {
		if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "t/z", Type: "Concept"}, "seed"); err != nil {
			t.Fatal(err)
		}
	}

	// The positive control: every B assertion below also holds if nothing was written.
	if c, err := cs.Read(ctx, A, "x/y"); err != nil || c == nil {
		t.Fatalf("Read(A, x/y) = %v, %v, want a concept", c, err)
	}
	if list, err := cs.List(ctx, A, "x/"); err != nil || !reflect.DeepEqual(list, []store.PathType{{Path: "x/y", Type: "Concept"}}) {
		t.Fatalf("List(A, x/) = %v, %v", list, err)
	}
	if hits, err := cs.Search(ctx, A, "secret", 10, nil); err != nil || len(hits) != 1 || hits[0].Path != "x/y" {
		t.Fatalf("Search(A, secret) = %v, %v, want just x/y", hits, err)
	}
	if hits, err := cs.Grep(ctx, A, "tenant a only", 10); err != nil || len(hits) != 1 || hits[0].Path != "x/y" {
		t.Fatalf("Grep(A, ...) = %v, %v, want just x/y", hits, err)
	}
	if _, bl, err := cs.ReadWithBacklinks(ctx, A, "t/z"); err != nil || !reflect.DeepEqual(bl, []string{"x/y"}) {
		t.Fatalf("ReadWithBacklinks(A, t/z) = %v, %v, want [x/y]", bl, err)
	}

	if c, err := cs.Read(ctx, B, "x/y"); err != nil || c != nil {
		t.Fatalf("Read(B, x/y) = %v, %v, want nil", c, err)
	}
	if list, err := cs.List(ctx, B, "x/"); err != nil || len(list) != 0 {
		t.Fatalf("List(B, x/) = %v, %v, want empty", list, err)
	}
	if hits, err := cs.Search(ctx, B, "secret", 10, nil); err != nil || len(hits) != 0 {
		t.Fatalf("Search(B, secret) = %v, %v, want empty", hits, err)
	}
	if hits, err := cs.Grep(ctx, B, "tenant a only", 10); err != nil || len(hits) != 0 {
		t.Fatalf("Grep(B, ...) = %v, %v, want empty", hits, err)
	}
	if c, bl, err := cs.ReadWithBacklinks(ctx, B, "t/z"); err != nil || c == nil || len(bl) != 0 {
		t.Fatalf("ReadWithBacklinks(B, t/z) = %v, %v, %v, want no backlinks", c, bl, err)
	}
}

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
	probeUntilScopedConnectionReturns(t, s, scoped)
}

// TestIsUnavailable is new: no Python test kills backends. It also locks okf_app
// out of new connections, not just existing ones: with login still allowed, the
// pool's ping-on-acquire silently discards a killed connection and dials a fresh
// one (see TestScopeSurvivesATerminatedBackendWhenLoginIsStillAllowed), so a real
// failure needs both the existing connection dead and no replacement possible.
func TestIsUnavailable(t *testing.T) {
	s, err := store.Open(ctx, db.AppDSN, "okf")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Force at least one physical connection to exist before locking it out.
	if err := s.Raw(ctx, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}

	restore := db.LockOut(t)

	scopeErr := s.Scope(ctx, uuid.New(), func(pgx.Tx) error { return nil })
	if scopeErr == nil {
		t.Fatal("Scope succeeded after okf_app was locked out and its backends terminated")
	}
	if !store.IsUnavailable(scopeErr) {
		t.Errorf("IsUnavailable(%v) = false, want true", scopeErr)
	}
	// Restored now, not just in Cleanup: the unique-violation check below needs a
	// working connection of its own.
	restore()

	s2 := openApp(t)
	dup := uuid.New()
	dupErr := s2.Scope(ctx, dup, func(tx pgx.Tx) error {
		if err := insert(ctx, tx, dup, "dup/path"); err != nil {
			return err
		}
		return insert(ctx, tx, dup, "dup/path")
	})
	if code := pgErrCode(t, dupErr); code != "23505" {
		t.Fatalf("code = %s, want unique_violation (23505)", code)
	}
	if store.IsUnavailable(dupErr) {
		t.Errorf("IsUnavailable(%v) = true, want false for a unique violation", dupErr)
	}
}

// The opposite of TestIsUnavailable: with login still allowed, the pool's
// ping-on-acquire discards a killed connection and dials a fresh one instead of
// failing the caller.
func TestScopeSurvivesATerminatedBackendWhenLoginIsStillAllowed(t *testing.T) {
	s, err := store.Open(ctx, db.AppDSN, "okf")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Raw(ctx, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}

	db.TerminateApp(t)

	if err := s.Scope(ctx, uuid.New(), func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("Scope after a terminated backend with login still allowed: %v", err)
	}
}

// A regression test for the acquire timeout: it MUST bound only the wait for a
// pooled connection, not the transaction that connection then runs. Before the
// fix, BeginTxFunc reused the acquire's timed context for COMMIT too, so a slow
// fn failed at commit instead of succeeding.
func TestScopeCommitsAfterTheAcquireTimeoutElapses(t *testing.T) {
	s := openApp(t)
	warm(t, s)
	restore := store.SetAcquireTimeout(100 * time.Millisecond)
	defer restore()

	tenant := uuid.New()
	err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		time.Sleep(300 * time.Millisecond)
		return insert(ctx, tx, tenant, "slow/one")
	})
	if err != nil {
		t.Fatalf("Scope with an fn slower than the acquire timeout: %v", err)
	}

	err = s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM okf.concept WHERE path = 'slow/one'").Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("count = %d, want 1: the slow transaction did not commit", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A real acquire timeout, unlike a query-level one, is unavailability: the pool
// itself could not produce a connection.
func TestIsUnavailableClassifiesAnAcquireTimeout(t *testing.T) {
	t.Setenv("KEEPSAKE_POOL_SIZE", "1")
	s, err := store.Open(ctx, db.AppDSN, "okf")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restore := store.SetAcquireTimeout(20 * time.Millisecond)
	defer restore()

	holding := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Scope(ctx, uuid.New(), func(pgx.Tx) error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Errorf("the connection-holding Scope call: %v", err)
		}
	}()

	err = s.Scope(ctx, uuid.New(), func(pgx.Tx) error { return nil })
	if err == nil {
		t.Fatal("Scope succeeded while the pool's only connection was held")
	}
	if !store.IsUnavailable(err) {
		t.Errorf("IsUnavailable(%v) = false, want true for an acquire timeout", err)
	}
}

// A deadline the caller's own fn hit, rather than the acquire, is not
// unavailability: context.DeadlineExceeded also satisfies net.Error, which is
// exactly what would misclassify this without the acquire-only guard.
func TestIsUnavailableDoesNotClassifyAnUnrelatedDeadline(t *testing.T) {
	s := openApp(t)
	warm(t, s)
	shortCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err := s.Scope(shortCtx, uuid.New(), func(tx pgx.Tx) error {
		_, err := tx.Exec(shortCtx, "SELECT pg_sleep(1)")
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if store.IsUnavailable(err) {
		t.Errorf("IsUnavailable(%v) = true, want false for a query-level deadline", err)
	}
}

// A server that dies mid-query sends no error; pgconn wraps the read's io.ErrUnexpectedEOF.
func TestIsUnavailableClassifiesADroppedConnection(t *testing.T) {
	err := fmt.Errorf("failed to receive message: %w", io.ErrUnexpectedEOF)
	if !store.IsUnavailable(err) {
		t.Errorf("IsUnavailable(%v) = false, want true", err)
	}
}

// A role, database or DSN default of okf.admin=on would open admin_read to /mcp.
func TestASessionDefaultAdminGUCDoesNotWidenAScopedRead(t *testing.T) {
	s := openApp(t)
	one, two := uuid.New(), uuid.New()
	paths := []string{"ambient/" + one.String(), "ambient/" + two.String()}
	for i, tenant := range []uuid.UUID{one, two} {
		path := paths[i]
		if err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
			return insert(ctx, tx, tenant, path)
		}); err != nil {
			t.Fatal(err)
		}
	}

	ambient, err := store.Open(ctx, db.AppDSN+"&options=-c%20okf.admin%3Don", "okf")
	if err != nil {
		t.Fatal(err)
	}
	defer ambient.Close()
	var seen map[uuid.UUID]bool
	if err := ambient.Scope(ctx, one, func(tx pgx.Tx) error {
		seen, err = owners(ctx, tx, paths)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := map[uuid.UUID]bool{one: true}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("owners = %v, want %v", seen, want)
	}
}

// admin_read is ORed into every SELECT; a bare GUC test there hid tenant_id.
func TestAScopedPointReadKeepsTheTenantIndex(t *testing.T) {
	s := openApp(t)
	var plan []string
	err := s.Scope(ctx, uuid.New(), func(tx pgx.Tx) error {
		// A table this small would otherwise be scanned whatever the policy says.
		if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "EXPLAIN SELECT path FROM okf.concept WHERE path = 'x'")
		if err != nil {
			return err
		}
		plan, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "Index Cond: ((tenant_id = ") {
		t.Fatal(joined)
	}
}

// warm opens the pool's connection first, so a short timeout bounds the step under test, not the dial.
func warm(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.Scope(ctx, uuid.New(), func(pgx.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
