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
	"time"

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
	if err != nil || !created || version < 1 {
		t.Fatalf("first Create = %d, %v, %v, want a version, true, nil", version, created, err)
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
	one, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/c", Type: "Concept", Body: "v1"}, "agent")
	if err != nil {
		t.Fatal(err)
	}

	two, conflict, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/c", Type: "Concept", Body: "v2"}, "agent", &one)
	if err != nil || conflict != nil || two <= one {
		t.Fatalf("first update = %d, %v, %v, want a version above %d", two, conflict, err, one)
	}

	_, conflict, err = cs.Update(ctx, tenant, okf.Concept{Path: "a/c", Type: "Concept", Body: "v3"}, "agent", &one)
	if err != nil || conflict == nil {
		t.Fatalf("second update = %v, %v, want a conflict", conflict, err)
	}
	if conflict.CurrentVersion != two || conflict.CurrentBody != "v2" {
		t.Fatalf("conflict = %+v, want {%d v2}", conflict, two)
	}
}

func TestUpdateWithoutExpectedVersionIsLastWriteWins(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	one, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/d", Type: "Concept", Body: "v1"}, "agent")
	if err != nil {
		t.Fatal(err)
	}

	version, conflict, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/d", Type: "Concept", Body: "v2"}, "agent", nil)
	if err != nil || conflict != nil || version <= one {
		t.Fatalf("update = %d, %v, %v, want a version above %d", version, conflict, err, one)
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
	one, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/e", Type: "Concept", Body: "v1"}, "agent")
	if err != nil {
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
		one := one
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

	var winners, conflicts, won int
	for _, r := range []result{r1, r2} {
		if r.conflict == nil {
			winners++
			won = r.version
		}
	}
	for _, r := range []result{r1, r2} {
		if r.conflict != nil {
			conflicts++
			if r.conflict.CurrentVersion != won {
				t.Errorf("conflict.CurrentVersion = %d, want the winner's %d", r.conflict.CurrentVersion, won)
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
	one, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/f", Type: "Concept", Body: "v1"}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := cs.Update(ctx, tenant, okf.Concept{Path: "a/f", Type: "Concept", Body: "v2"}, "agent", &one)
	if err != nil {
		t.Fatal(err)
	}

	type row struct {
		version int
		op      string
		body    string
	}
	var rows []row
	err = s.Scope(ctx, tenant, func(tx pgx.Tx) error {
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
	want := []row{{one, "create", "v1"}, {two, "update", "v2"}}
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
// into the plain string "2026-01-01". It MUST still round-trip through jsonb
// byte-identically.
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
// transaction ImportMany runs the whole bundle in, so the first MUST NOT survive.
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
	want := []string{"a create 1 t", "a update 2 t", "a update 3 t", "b create b t"}
	if got := revisionLog(t, s, tenant); !reflect.DeepEqual(got, want) {
		t.Fatalf("revisions = %q, want %q", got, want)
	}
}

// revisionLog is each revision as "path op body", then whether its snapshot names its version, in version order per path.
func revisionLog(t *testing.T, s *store.Store, tenant uuid.UUID) []string {
	t.Helper()
	var log []string
	err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT concat_ws(' ', path, op, snapshot->>'body', (snapshot->>'version')::bigint = version) "+
			"FROM concept_revision ORDER BY path, version")
		if err != nil {
			return err
		}
		log, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return log
}

// Pushing the same bundle again MUST NOT bump versions, or every agent's expected_version goes stale.
func TestImportManySkipsAnUnchangedConcept(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	fm := okf.NewMap()
	fm.Set("tags", []any{"db"})
	fm.Set("owner", "sre")
	bundle := []okf.Concept{{Path: "a", Type: "Concept", Body: "same", Frontmatter: fm, Links: []string{"b"}}}
	if _, err := cs.ImportMany(ctx, tenant, bundle, "agent"); err != nil {
		t.Fatal(err)
	}
	before, err := cs.Read(ctx, tenant, "a")
	if err != nil {
		t.Fatal(err)
	}
	// The same frontmatter with its keys in another order is the same jsonb.
	reordered := okf.NewMap()
	reordered.Set("owner", "sre")
	reordered.Set("tags", []any{"db"})
	bundle[0].Frontmatter = reordered
	if n, err := cs.ImportMany(ctx, tenant, bundle, "agent"); err != nil || n != 0 {
		t.Fatalf("ImportMany = %d, %v, want 0 written", n, err)
	}
	if after, err := cs.Read(ctx, tenant, "a"); err != nil || after.Version != before.Version {
		t.Fatalf("Read(a) = %+v, %v, want version %d", after, err, before.Version)
	}
	if got := revisionLog(t, s, tenant); len(got) != 1 {
		t.Fatalf("revisions = %q, want only the create", got)
	}
}

func listPaths(t *testing.T, cs *store.ConceptStore, tenant uuid.UUID) []string {
	t.Helper()
	got, err := cs.List(ctx, tenant, "")
	if err != nil {
		t.Fatal(err)
	}
	return paths(got)
}

func TestReplacePrefixMakesThePrefixExactlyTheBundle(t *testing.T) {
	cs := store.NewConceptStore(openApp(t))
	tenant := uuid.New()
	seed := []okf.Concept{
		{Path: "docs", Type: "Doc"},
		{Path: "docs/keep", Type: "Doc", Body: "old"},
		{Path: "docs/gone", Type: "Doc"},
		{Path: "docsx/other", Type: "Doc"},
		{Path: "notes/agent", Type: "Note"},
	}
	if _, err := cs.ImportMany(ctx, tenant, seed, "agent"); err != nil {
		t.Fatal(err)
	}
	bundle := []okf.Concept{{Path: "docs/keep", Type: "Doc", Body: "new"}, {Path: "docs/sub/added", Type: "Doc"}}
	if written, deleted, err := cs.ReplacePrefix(ctx, tenant, "docs", bundle, "platform"); err != nil || written != 2 || deleted != 1 {
		t.Fatalf("ReplacePrefix = %d, %d, %v, want 2, 1, nil", written, deleted, err)
	}
	want := []string{"docs", "docs/keep", "docs/sub/added", "docsx/other", "notes/agent"}
	if got := listPaths(t, cs, tenant); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	if c, err := cs.Read(ctx, tenant, "docs/keep"); err != nil || c.Body != "new" {
		t.Fatalf("Read(docs/keep) = %+v, %v", c, err)
	}
}

// A re-created path MUST NOT reuse a version, or a stale expected_version would overwrite the new concept.
func TestADeletedPathReturnsAtANewVersionWithItsHistory(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	replace := func(path, body string) {
		t.Helper()
		if _, _, err := cs.ReplacePrefix(ctx, tenant, "docs", []okf.Concept{{Path: path, Type: "Doc", Body: body}}, "platform"); err != nil {
			t.Fatalf("ReplacePrefix(%s): %v", path, err)
		}
	}
	replace("docs/a", "old")
	stale, err := cs.Read(ctx, tenant, "docs/a")
	if err != nil {
		t.Fatal(err)
	}
	replace("docs/b", "")
	replace("docs/a", "new")

	_, conflict, err := cs.Update(ctx, tenant, okf.Concept{Path: "docs/a", Type: "Doc", Body: "from a stale read"}, "agent", &stale.Version)
	if err != nil || conflict == nil || conflict.CurrentBody != "new" {
		t.Fatalf("Update(stale) = %+v, %v, want a conflict against the new body", conflict, err)
	}
	want := []string{"docs/a create old t", "docs/a delete old t", "docs/a create new t", "docs/b create  t", "docs/b delete  t"}
	if got := revisionLog(t, s, tenant); !reflect.DeepEqual(got, want) {
		t.Fatalf("revisions = %q, want %q", got, want)
	}
}

func TestReplacePrefixIsOneTransaction(t *testing.T) {
	cs := store.NewConceptStore(openApp(t))
	tenant := uuid.New()
	if _, err := cs.ImportMany(ctx, tenant, []okf.Concept{{Path: "docs/old", Type: "Doc"}}, "agent"); err != nil {
		t.Fatal(err)
	}
	bundle := []okf.Concept{{Path: "docs/new", Type: "Doc", Body: "bad\x00body"}}
	if _, _, err := cs.ReplacePrefix(ctx, tenant, "docs", bundle, "platform"); err == nil {
		t.Fatal("ReplacePrefix = nil error, want one from the NUL byte")
	}
	if got := listPaths(t, cs, tenant); !reflect.DeepEqual(got, []string{"docs/old"}) {
		t.Fatalf("paths = %v, want the delete rolled back", got)
	}
}

func TestReplacePrefixRefusesAPathOutsideIt(t *testing.T) {
	cs := store.NewConceptStore(openApp(t))
	if _, _, err := cs.ReplacePrefix(ctx, uuid.New(), "docs", []okf.Concept{{Path: "docsx/a", Type: "Doc"}}, "platform"); err == nil {
		t.Fatal("ReplacePrefix = nil error")
	}
}

// Without a tenant-wide lock, a replace of docs and one of docs/sub each keep the path the other inserted.
func TestConcurrentReplacesOfNestedPrefixesLeaveOneBundle(t *testing.T) {
	cs := store.NewConceptStore(openApp(t))
	for range 10 {
		tenant := uuid.New()
		var wg sync.WaitGroup
		for _, r := range [][2]string{{"docs", "docs/sub/x"}, {"docs/sub", "docs/sub/y"}} {
			wg.Go(func() {
				if _, _, err := cs.ReplacePrefix(ctx, tenant, r[0], []okf.Concept{{Path: r[1], Type: "Doc"}}, "platform"); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if got := listPaths(t, cs, tenant); len(got) != 1 {
			t.Fatalf("paths = %v, want exactly one bundle", got)
		}
	}
}

// A delete revision MUST follow an update committed while ReplacePrefix waited on its row lock.
func TestReplacePrefixLogsItsDeleteAfterAnUpdateItWaitedFor(t *testing.T) {
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := uuid.New()
	if _, err := cs.ImportMany(ctx, tenant, []okf.Concept{{Path: "docs/gone", Type: "Doc"}}, "agent"); err != nil {
		t.Fatal(err)
	}

	locked, release := make(chan int), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- s.Scope(ctx, tenant, func(tx pgx.Tx) error {
			var pid int
			_, err := tx.Exec(ctx, "WITH w AS (UPDATE concept SET version = nextval('version_seq'), body = 'raced' WHERE path = 'docs/gone' RETURNING version) "+
				"INSERT INTO concept_revision (tenant_id, path, version, op, snapshot, updated_by) "+
				"SELECT $1, 'docs/gone', version, 'update', jsonb_build_object('version', version, 'body', 'raced'), 'agent' FROM w", tenant)
			if err == nil {
				err = tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
			}
			locked <- pid
			<-release
			return err
		})
	}()
	pid := <-locked
	if pid == 0 {
		close(release)
		t.Fatal(<-held)
	}

	replaced := make(chan error, 1)
	go func() {
		_, _, err := cs.ReplacePrefix(ctx, tenant, "docs", []okf.Concept{{Path: "docs/kept", Type: "Doc"}}, "platform")
		replaced <- err
	}()
	// Commit only once the replace is queued behind the held row lock.
	for blocked := false; !blocked; time.Sleep(10 * time.Millisecond) {
		select {
		case err := <-replaced:
			close(release)
			t.Fatalf("ReplacePrefix = %v before it waited on the row lock", err)
		default:
		}
		err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked)
		})
		if err != nil {
			close(release)
			t.Fatal(err)
		}
	}
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if err := <-replaced; err != nil {
		t.Fatal(err)
	}

	// The delete saw the raced update, so its snapshot holds the body that update wrote.
	want := []string{"docs/gone create  t", "docs/gone update raced t", "docs/gone delete raced t", "docs/kept create  t"}
	if got := revisionLog(t, s, tenant); !reflect.DeepEqual(got, want) {
		t.Fatalf("revisions = %q, want %q", got, want)
	}
}
