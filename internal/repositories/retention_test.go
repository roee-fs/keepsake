package repositories_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/repositories"
	"github.com/roee-fs/keepsake/okf"
)

var cutoff = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	atCutoff     = "'2026-01-01 00:00:00+00'"
	beforeCutoff = "'2025-12-31 23:59:59.999999+00'"
)

// adminInts runs sql as the superuser, past RLS, and returns its one int column.
func adminInts(t *testing.T, sql string) []int {
	t.Helper()
	conn, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, _ := conn.Query(ctx, sql)
	out, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func logReadAt(t *testing.T, tenant uuid.UUID, at string, n int) {
	t.Helper()
	pgtest.Exec(t, db.AdminDSN, fmt.Sprintf(
		"INSERT INTO okf.concept_read (tenant_id, created_at) SELECT '%s', %s FROM generate_series(1, %d)", tenant, at, n))
}

// A read at the cutoff stays, one a microsecond older goes, in batches, and another tenant's stay.
func TestPruneReadsDeletesOnlyOlderReadsOfOneTenant(t *testing.T) {
	defer repositories.SetPruneBatch(2)()
	cs, tenant := fixture(t)
	other := uuid.New()
	logReadAt(t, tenant, atCutoff, 1)
	logReadAt(t, tenant, beforeCutoff, 5)
	logReadAt(t, other, beforeCutoff, 1)

	n, err := cs.PruneReads(ctx, tenant, cutoff)
	if err != nil || n != 5 {
		t.Fatalf("PruneReads = %d, %v, want 5", n, err)
	}
	left := adminInts(t, fmt.Sprintf(
		"SELECT count(*)::int FROM okf.concept_read WHERE tenant_id IN ('%s', '%s') GROUP BY tenant_id = '%s' ORDER BY 1",
		tenant, other, tenant))
	if !slices.Equal(left, []int{1, 1}) {
		t.Fatalf("reads left = %v, want one per tenant", left)
	}
}

// Each path keeps its newest keep revisions at any age; older ones past the cutoff go.
func TestPruneRevisionsKeepsEachPathsNewest(t *testing.T) {
	defer repositories.SetPruneBatch(2)()
	cs, tenant := fixture(t)
	for _, p := range []string{"busy", "quiet"} {
		create(t, okf.Concept{Path: p, Type: "Concept"})
	}
	for i := range 4 {
		c := okf.Concept{Path: "busy", Type: "Concept", Body: fmt.Sprint(i)}
		if _, _, err := cs.Update(ctx, tenant, c, "seed", nil); err != nil {
			t.Fatal(err)
		}
	}
	pgtest.Exec(t, db.AdminDSN, fmt.Sprintf(
		"UPDATE okf.concept_revision SET created_at = %s WHERE tenant_id = '%s'", beforeCutoff, tenant))

	n, err := cs.PruneRevisions(ctx, tenant, cutoff, 2)
	if err != nil || n != 3 {
		t.Fatalf("PruneRevisions = %d, %v, want 3", n, err)
	}
	left := adminInts(t, fmt.Sprintf(
		"SELECT count(*)::int FROM okf.concept_revision WHERE tenant_id = '%s' GROUP BY path ORDER BY path", tenant))
	if !slices.Equal(left, []int{2, 1}) {
		t.Fatalf("revisions left per path = %v, want busy 2 and quiet 1", left)
	}
}

func TestPruneRevisionsKeepsWhatIsNewerThanTheCutoff(t *testing.T) {
	cs, tenant := fixture(t)
	create(t, okf.Concept{Path: "a", Type: "Concept"})
	create(t, okf.Concept{Path: "b", Type: "Concept"})
	pgtest.Exec(t, db.AdminDSN, fmt.Sprintf(
		"UPDATE okf.concept_revision SET created_at = %s WHERE tenant_id = '%s'", atCutoff, tenant))
	if n, err := cs.PruneRevisions(ctx, tenant, cutoff, 1); err != nil || n != 0 {
		t.Fatalf("PruneRevisions = %d, %v, want 0", n, err)
	}
}

// A tenant whose concepts are all gone still has a log to prune.
func TestLogTenantsListsATenantWithReadsAlone(t *testing.T) {
	cs, _ := fixture(t)
	only := uuid.New()
	logReadAt(t, only, "now()", 1)
	tenants, err := cs.LogTenants(ctx)
	if err != nil || !slices.Contains(tenants, only) {
		t.Fatalf("LogTenants = %v, %v, want %s among them", tenants, err, only)
	}
}
