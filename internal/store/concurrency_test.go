// Ported from 2de90d2:tests/test_concurrency.py: concept writes and the compare-and-swap.
//
// package store_test, not store: reuses isolation_test.go's TestMain, db and ctx.
package store_test

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

func TestCreateReturnsNotCreatedWhenPathTaken(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	c := okf.Concept{Path: "a/b", Type: "Concept", Body: "one"}

	version, created, err := cs.Create(ctx, tenant, c, "agent")
	if err != nil || !created || version != 1 {
		t.Fatalf("first Create = %d, %v, %v, want 1, true, nil", version, created, err)
	}
	_, created, err = cs.Create(ctx, tenant, c, "agent")
	if err != nil || created {
		t.Fatalf("second Create = %v, %v, want false, nil", created, err)
	}
}

func TestUpdateWithStaleVersionReturnsCurrentContent(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/c", Type: "Concept", Body: "v1"}, "agent"); err != nil {
		t.Fatal(err)
	}

	one := 1
	version, conflict, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/c", Type: "Concept", Body: "v2"}, "agent", &one)
	if err != nil || conflict != nil || version != 2 {
		t.Fatalf("first update = %d, %v, %v, want 2, nil, nil", version, conflict, err)
	}

	_, conflict, err = cs.Update(ctx, tenant, okf.Concept{Path: "a/c", Type: "Concept", Body: "v3"}, "agent", &one)
	if err != nil || conflict == nil {
		t.Fatalf("second update = %v, %v, want a conflict", conflict, err)
	}
	if conflict.CurrentVersion != 2 || conflict.CurrentBody != "v2" {
		t.Fatalf("conflict = %+v, want {2 v2}", conflict)
	}
}

func TestUpdateWithoutExpectedVersionIsLastWriteWins(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/d", Type: "Concept", Body: "v1"}, "agent"); err != nil {
		t.Fatal(err)
	}

	version, conflict, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/d", Type: "Concept", Body: "v2"}, "agent", nil)
	if err != nil || conflict != nil || version != 2 {
		t.Fatalf("update = %d, %v, %v, want 2, nil, nil", version, conflict, err)
	}

	var body string
	err = s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT body FROM concept WHERE path = 'a/d'").Scan(&body)
	})
	if err != nil {
		t.Fatal(err)
	}
	if body != "v2" {
		t.Fatalf("body = %q, want v2", body)
	}
}

func TestUpdateOfAnAbsentPathReturnsErrNotFoundWithoutDisclosingBody(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()

	_, _, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/missing", Type: "Concept", Body: "secret"}, "agent", nil)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want it to wrap ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "a/missing") {
		t.Fatalf("err = %v, want it to name the path", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, must not disclose the body", err)
	}
}

// Two transactions open at once on two connections. The loser blocks on the
// winner's row lock and, under READ COMMITTED, re-checks the version predicate
// against the committed row, so it updates nothing. A barrier makes the race
// deterministic: both goroutines reach it before either issues its UPDATE.
func TestExactlyOneOfTwoConcurrentUpdatesWins(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/e", Type: "Concept", Body: "v1"}, "agent"); err != nil {
		t.Fatal(err)
	}

	type result struct {
		version  int
		conflict *store.Conflict
		err      error
	}
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	results := make(chan result, 2)

	race := func(actor, body string) {
		one := 1
		ready.Done()
		<-start
		version, conflict, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/e", Type: "Concept", Body: body}, actor, &one)
		results <- result{version, conflict, err}
	}
	go race("a1", "x")
	go race("a2", "y")
	ready.Wait()
	close(start)

	r1, r2 := <-results, <-results
	for _, r := range []result{r1, r2} {
		if r.err != nil {
			t.Fatal(r.err)
		}
	}

	var winners, conflicts int
	for _, r := range []result{r1, r2} {
		switch {
		case r.conflict == nil:
			winners++
			if r.version != 2 {
				t.Errorf("winner version = %d, want 2", r.version)
			}
		default:
			conflicts++
			if r.conflict.CurrentVersion != 2 {
				t.Errorf("conflict.CurrentVersion = %d, want 2", r.conflict.CurrentVersion)
			}
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners = %d, conflicts = %d, want exactly one of each", winners, conflicts)
	}
}

func TestEveryWriteAppendsARevision(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/f", Type: "Concept", Body: "v1"}, "agent"); err != nil {
		t.Fatal(err)
	}
	one := 1
	if _, _, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/f", Type: "Concept", Body: "v2"}, "agent", &one); err != nil {
		t.Fatal(err)
	}

	type row struct {
		version int
		op      string
		body    string
	}
	var rows []row
	err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx,
			"SELECT version, op, snapshot->>'body' FROM concept_revision WHERE path = 'a/f' ORDER BY version")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var rr row
			if err := r.Scan(&rr.version, &rr.op, &rr.body); err != nil {
				return err
			}
			rows = append(rows, rr)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []row{{1, "create", "v1"}, {2, "update", "v2"}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
}

func TestAConflictingUpdateAppendsNoRevision(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/g", Type: "Concept", Body: "v1"}, "agent"); err != nil {
		t.Fatal(err)
	}

	stale := 99
	_, conflict, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/g", Type: "Concept", Body: "v2"}, "agent", &stale)
	if err != nil || conflict == nil {
		t.Fatalf("update = %v, %v, want a conflict", conflict, err)
	}

	var count int
	err = s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM concept_revision WHERE path = 'a/g'").Scan(&count)
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
}

// YAML loads an unquoted 2026-01-01 as a date, which okf's parser already turns
// into the plain string "2026-01-01" (Task 3/4's divergence 1). It must still
// round-trip through jsonb byte-identically.
func TestFrontmatterCarryingADateSurvivesAWrite(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()

	c, err := okf.Parse("---\ntype: Concept\nreviewed: 2026-01-01\n---\nbody\n", "a/h")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := c.Frontmatter.Get("reviewed"); !ok || v != "2026-01-01" {
		t.Fatalf("frontmatter[reviewed] = %v, %v, want \"2026-01-01\", true", v, ok)
	}
	if _, _, err := cs.Create(ctx, tenant, c, "agent"); err != nil {
		t.Fatal(err)
	}

	var frontmatter, snapshotFrontmatter map[string]any
	err = s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT frontmatter, snapshot->'frontmatter' FROM concept "+
				"JOIN concept_revision USING (tenant_id, path, version) WHERE path = 'a/h'",
		).Scan(&frontmatter, &snapshotFrontmatter)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"reviewed": "2026-01-01"}
	if !reflect.DeepEqual(frontmatter, want) || !reflect.DeepEqual(snapshotFrontmatter, want) {
		t.Fatalf("frontmatter = %v, snapshot = %v, want both %v", frontmatter, snapshotFrontmatter, want)
	}
}

func TestRevisionSnapshotCarriesEveryFieldAndTheNewVersion(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	fm := okf.NewMap()
	fm.Set("reviewed", "2026-01-01")
	c := okf.Concept{
		Path:        "a/snap",
		Type:        "Concept",
		Title:       "Title",
		Description: "Desc",
		Body:        "body text",
		Frontmatter: fm,
		Links:       []string{"a/other"},
	}

	version, created, err := cs.Create(ctx, tenant, c, "agent")
	if err != nil || !created {
		t.Fatalf("Create = %d, %v, %v", version, created, err)
	}

	var snapshot map[string]any
	err = s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT snapshot FROM concept_revision WHERE path = 'a/snap' AND version = $1", version,
		).Scan(&snapshot)
	})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"path":        "a/snap",
		"type":        "Concept",
		"title":       "Title",
		"description": "Desc",
		"body":        "body text",
		"frontmatter": map[string]any{"reviewed": "2026-01-01"},
		"links":       []any{"a/other"},
		"version":     float64(version),
	}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %#v, want %#v", snapshot, want)
	}
}

// The second concept's NUL byte fails at the database, inside the same
// transaction ImportMany runs the whole bundle in, so the first must not survive.
func TestImportManyIsOneTransaction(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	bundle := []okf.Concept{
		{Path: "a/import1", Type: "Concept", Body: "ok"},
		{Path: "a/import2", Type: "Concept", Body: "bad\x00body"},
	}

	if _, err := cs.ImportMany(ctx, tenant, bundle, "agent"); err == nil {
		t.Fatal("ImportMany = nil error, want one from the NUL byte")
	}

	var count int
	err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM concept WHERE path = 'a/import1'").Scan(&count)
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0: the first concept must not survive the second's failure", count)
	}
}

// A re-import overwrites a taken path and logs it, in bundle order, even when the bundle names it twice.
func TestImportManyOverwritesTakenPathsInBundleOrder(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	if _, err := cs.ImportMany(ctx, tenant, []okf.Concept{{Path: "a", Type: "Concept", Body: "1"}}, "agent"); err != nil {
		t.Fatal(err)
	}
	bundle := []okf.Concept{
		{Path: "a", Type: "Concept", Body: "2"},
		{Path: "b", Type: "Concept", Body: "b"},
		{Path: "a", Type: "Concept", Body: "3"},
	}
	if n, err := cs.ImportMany(ctx, tenant, bundle, "agent"); err != nil || n != 3 {
		t.Fatalf("ImportMany = %d, %v", n, err)
	}
	if c, err := cs.Read(ctx, tenant, "a"); err != nil || c.Version != 3 || c.Body != "3" {
		t.Fatalf("Read(a) = %+v, %v, want version 3 with body 3", c, err)
	}
	var log []string
	err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT concat_ws(' ', path, version, op, snapshot->>'body', snapshot->>'version') "+
			"FROM concept_revision ORDER BY path, version")
		if err != nil {
			return err
		}
		log, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if want := []string{"a 1 create 1 1", "a 2 update 2 2", "a 3 update 3 3", "b 1 create b 1"}; err != nil || !reflect.DeepEqual(log, want) {
		t.Fatalf("revisions = %q, %v, want %q", log, err, want)
	}
}
