package cli

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPruneDeletesReadsPastTheirRetentionAsTheAppRole(t *testing.T) {
	t.Setenv("KEEPSAKE_READ_RETENTION_DAYS", "30")
	admin, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	// A tenant with reads and no concepts, as one is after its concepts are deleted.
	tenant := uuid.New()
	if _, err := admin.Exec(ctx, "INSERT INTO okf.concept_read (tenant_id, created_at) VALUES "+
		"($1, now() - interval '31 days'), ($1, now() - interval '29 days')", tenant); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run(t, "prune", "--dsn", db.AppDSN)
	if code != 0 || !strings.HasPrefix(stdout, "pruned ") {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	var left int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM okf.concept_read WHERE tenant_id = $1", tenant).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("%d reads left, want the 29-day-old one", left)
	}
}

func TestPruneRefusesABadRetention(t *testing.T) {
	for name, value := range map[string]string{
		"KEEPSAKE_READ_RETENTION_DAYS":     "0",
		"KEEPSAKE_REVISION_RETENTION_DAYS": "-1",
		"KEEPSAKE_REVISION_KEEP":           "ten",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			code, _, stderr := run(t, "prune", "--dsn", db.AppDSN)
			if code != 1 || !strings.Contains(stderr, name+" must be an integer") {
				t.Fatalf("exit %d: %s", code, stderr)
			}
		})
	}
}
