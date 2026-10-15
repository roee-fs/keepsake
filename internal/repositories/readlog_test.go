package repositories_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/pgtest"
)

func readsOf(t *testing.T, tenant uuid.UUID) int {
	t.Helper()
	return adminInts(t, fmt.Sprintf("SELECT count(*)::int FROM okf.concept_read WHERE tenant_id = '%s'", tenant))[0]
}

// Each agent read logs one row in its own transaction; the console's reads log none.
func TestWithReadLogLogsEachAgentReadOnce(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	logged := cs.WithReadLog()
	if _, _, err := logged.ReadWithBacklinks(ctx, tenant, seedData[0].path); err != nil {
		t.Fatal(err)
	}
	if _, err := logged.Search(ctx, tenant, "tenant", 5, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := logged.Grep(ctx, tenant, "tenant", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := logged.List(ctx, tenant, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Search(ctx, tenant, "tenant", 5, nil); err != nil {
		t.Fatal(err)
	}
	if n := readsOf(t, tenant); n != 4 {
		t.Fatalf("%d reads logged, want 4", n)
	}
}

// The savepoint keeps a refused insert from aborting the read it follows.
func TestAFailedReadLogStillReturnsTheRead(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	pgtest.Exec(t, db.AdminDSN, "REVOKE INSERT ON okf.concept_read FROM keepsake_app")
	t.Cleanup(func() { pgtest.Exec(t, db.AdminDSN, "GRANT INSERT ON okf.concept_read TO keepsake_app") })
	c, _, err := cs.WithReadLog().ReadWithBacklinks(ctx, tenant, seedData[0].path)
	if err != nil || c == nil {
		t.Fatalf("ReadWithBacklinks = %v, %v, want the concept", c, err)
	}
	if n := readsOf(t, tenant); n != 0 {
		t.Fatalf("%d reads logged, want 0", n)
	}
}
