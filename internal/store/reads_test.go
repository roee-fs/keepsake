// Ported from 2de90d2:tests/test_reads.py: the read paths an agent and the admin console
// use to find and browse knowledge. search returns cards, never bodies.
//
// package store_test, not store: shares isolation_test.go's TestMain, db and ctx.
package store_test

import (
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

var tenantCache sync.Map // *testing.T -> uuid.UUID

// testTenant is a fresh uuid, generated once per test and cached, so create/read
// (below) can share one tenant across the many calls one test makes without
// threading it through every call, the way pytest's function-scoped tenant
// fixture does. It must be random, not derived from t.Name(): TestMain starts one
// database for the whole test binary and never resets it between tests, so under
// `go test -count=2` a name-derived tenant would be the same UUID both runs and
// see the first run's rows.
func testTenant(t *testing.T) uuid.UUID {
	v, _ := tenantCache.LoadOrStore(t, uuid.New())
	return v.(uuid.UUID)
}

var fixtureCache sync.Map // *testing.T -> *store.ConceptStore

func fixture(t *testing.T) (*store.ConceptStore, uuid.UUID) {
	t.Helper()
	if v, ok := fixtureCache.Load(t); ok {
		return v.(*store.ConceptStore), testTenant(t)
	}
	cs := store.NewConceptStore(openApp(t))
	fixtureCache.Store(t, cs)
	return cs, testTenant(t)
}

func create(t *testing.T, c okf.Concept) {
	t.Helper()
	cs, tenant := fixture(t)
	if _, _, err := cs.Create(ctx, tenant, c, "seed"); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) okf.Concept {
	t.Helper()
	cs, tenant := fixture(t)
	c, err := cs.Read(ctx, tenant, path)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatalf("read(%q) = nil, want a concept", path)
	}
	return *c
}

var seedData = []struct{ path, title, body string }{
	{"detect/dormant", "Dormant Rule Identification", "Detection rules that have not fired in 90 days."},
	{"auth/flow", "OAuth2 Authorization Flow", "Standardised on PKCE for client authentication."},
	{"splunk/cursor", "Splunk Position Cursor", "The poller advances an index-time cursor. See [dormant](../detect/dormant.md)."},
}

func seed(t *testing.T) {
	t.Helper()
	for _, s := range seedData {
		create(t, okf.Concept{
			Path: s.path, Type: "Concept", Title: s.title, Body: s.body,
			Links: okf.ExtractLinks(s.body, s.path),
		})
	}
}

func TestReadKeepsJsonbKeyOrder(t *testing.T) {
	fm := okf.NewMap()
	fm.Set("zz", "1")
	fm.Set("a", "2")
	fm.Set("mmm", "3")
	create(t, okf.Concept{Path: "p", Type: "Concept", Frontmatter: fm})
	for range 20 { // a Go map would pass this sometimes; 20 draws make a pass by luck vanishingly rare
		c := read(t, "p")
		if !slices.Equal(c.Frontmatter.Keys(), []string{"a", "zz", "mmm"}) {
			t.Fatalf("keys %q: MUST be jsonb's order", c.Frontmatter.Keys())
		}
	}
}

func TestReadRoundTripsACreatedConcept(t *testing.T) {
	fm := okf.NewMap()
	fm.Set("owner", "sec")
	c := okf.Concept{
		Path: "a/b", Type: "Concept", Title: "T", Description: "D",
		Body: "B [x](../detect/dormant.md)", Frontmatter: fm,
		Links: []string{"detect/dormant"}, Version: 1,
	}
	create(t, c)
	got := read(t, "a/b")
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("read = %+v, want %+v", got, c)
	}
}

func TestReadReturnsNoneForAnUnknownPath(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	c, err := cs.Read(ctx, tenant, "detect/nothing")
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Fatalf("Read(unknown) = %+v, want nil", c)
	}
}

func TestSearchOrsTermsRatherThanAnding(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	hits, err := cs.Search(ctx, tenant, "cursors stalling", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Path != "splunk/cursor" {
		t.Fatalf("hits = %+v, want splunk/cursor first", hits)
	}
}

func TestSearchRanksMoreMatchingTermsHigher(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	hits, err := cs.Search(ctx, tenant, "dormant detection rules", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Path != "detect/dormant" {
		t.Fatalf("hits = %+v, want detect/dormant first", hits)
	}
}

func TestSearchNeverReturnsABody(t *testing.T) {
	const secret = "zqxjkbody"
	create(t, okf.Concept{Path: "detect/secret", Type: "Concept", Title: "Dormant", Body: secret})
	cs, tenant := fixture(t)
	hits, err := cs.Search(ctx, tenant, "dormant", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := hits[0]
	if strings.Contains(h.Path+h.Type+h.Title+h.Description, secret) {
		t.Fatalf("Hit %+v carries the body", h)
	}
	if h.Path != "detect/secret" || h.Type != "Concept" || h.Title != "Dormant" {
		t.Fatalf("Hit = %+v, want (detect/secret, Concept, Dormant)", h)
	}
}

func TestSearchRespectsLimit(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	q := "cursor detection authentication"
	full, err := cs.Search(ctx, tenant, q, 10, nil)
	if err != nil || len(full) != len(seedData) {
		t.Fatalf("Search(limit=10) = %d hits, err %v, want %d", len(full), err, len(seedData))
	}
	two, err := cs.Search(ctx, tenant, q, 2, nil)
	if err != nil || len(two) != 2 {
		t.Fatalf("Search(limit=2) = %d hits, err %v, want 2", len(two), err)
	}
	// Negative, not zero: Postgres answers LIMIT 0 with no rows by itself, so only a
	// negative limit reaches the guard.
	neg, err := cs.Search(ctx, tenant, q, -1, nil)
	if err != nil || len(neg) != 0 {
		t.Fatalf("Search(limit=-1) = %v, err %v, want empty", neg, err)
	}
}

func TestSearchConfinesHitsToThePrefix(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	prefix := "detect/"
	hits, err := cs.Search(ctx, tenant, "cursor detection authentication", 10, &prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "detect/dormant" {
		t.Fatalf("hits = %+v, want just detect/dormant", hits)
	}
}

// Both concepts mention "dormant"; the one carrying it in its title outranks the other.
var dormantHits = []string{"detect/dormant", "splunk/cursor"}

func hitPaths(hits []store.Hit) []string {
	paths := make([]string, len(hits))
	for i, h := range hits {
		paths[i] = h.Path
	}
	return paths
}

func TestSearchIsCaseInsensitive(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	hits, err := cs.Search(ctx, tenant, "DORMANT", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hitPaths(hits), dormantHits) {
		t.Fatalf("hits = %v, want %v", hitPaths(hits), dormantHits)
	}
}

func TestSearchTreatsTsquerySyntaxAsText(t *testing.T) {
	cs, tenant := fixture(t)
	hits, err := cs.Search(ctx, tenant, "dormant | ) & !(", 10, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("Search before seeding = %v, err %v, want empty", hits, err)
	}
	seed(t)
	hits, err = cs.Search(ctx, tenant, "dormant | ) & !(", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hitPaths(hits), dormantHits) {
		t.Fatalf("hits = %v, want %v", hitPaths(hits), dormantHits)
	}
}

func TestSearchOfATermlessQueryIsEmptyNotInvalid(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	hits, err := cs.Search(ctx, tenant, "  !!!  ", 10, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("Search(termless) = %v, err %v, want empty", hits, err)
	}
}

func grepPaths(t *testing.T, hits []store.GrepHit) []string {
	t.Helper()
	paths := make([]string, len(hits))
	for i, h := range hits {
		paths[i] = h.Path
	}
	return paths
}

func TestGrepMatchesARegexAndIsLimited(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)

	hits, err := cs.Grep(ctx, tenant, "index-time", 10)
	if err != nil || !slices.Equal(grepPaths(t, hits), []string{"splunk/cursor"}) {
		t.Fatalf("Grep(index-time) = %v, err %v", hits, err)
	}
	hits, err = cs.Grep(ctx, tenant, "inde.-tim[ez]", 10)
	if err != nil || !slices.Equal(grepPaths(t, hits), []string{"splunk/cursor"}) {
		t.Fatalf("Grep(inde.-tim[ez]) = %v, err %v", hits, err)
	}
	hits, err = cs.Grep(ctx, tenant, "[a-z]", 10)
	if err != nil || len(hits) != len(seedData) {
		t.Fatalf("Grep([a-z], limit=10) = %d hits, err %v, want %d", len(hits), err, len(seedData))
	}
	hits, err = cs.Grep(ctx, tenant, "[a-z]", 2)
	if err != nil || len(hits) != 2 {
		t.Fatalf("Grep([a-z], limit=2) = %d hits, err %v, want 2", len(hits), err)
	}
	hits, err = cs.Grep(ctx, tenant, "index-time", -1)
	if err != nil || len(hits) != 0 {
		t.Fatalf("Grep(limit=-1) = %v, err %v, want empty", hits, err)
	}
}

func TestGrepMatchesTitlesAsWellAsBodies(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	for _, pattern := range []string{"OAuth2", "oauth2"} {
		hits, err := cs.Grep(ctx, tenant, pattern, 10)
		if err != nil || !slices.Equal(grepPaths(t, hits), []string{"auth/flow"}) {
			t.Fatalf("Grep(%q) = %v, err %v", pattern, hits, err)
		}
	}
}

func TestGrepSnippetCarriesSurroundingContext(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	hits, err := cs.Grep(ctx, tenant, "index-time", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("Grep(index-time) = %v, err %v, want one hit", hits, err)
	}
	if !strings.Contains(hits[0].Snippet, "poller advances an index-time cursor") {
		t.Fatalf("snippet = %q, want surrounding context", hits[0].Snippet)
	}
}

func TestGrepSnippetIsBoundedAndSingleLine(t *testing.T) {
	lines := make([]string, 400)
	for i := range lines {
		lines[i] = "filler line " + string(rune('0'+i%10))
	}
	body := "needle here\n" + strings.Join(lines, "\n")
	create(t, okf.Concept{Path: "big/one", Type: "Concept", Title: "Big", Body: body})
	cs, tenant := fixture(t)
	hits, err := cs.Grep(ctx, tenant, "needle", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("Grep(needle) = %v, err %v, want one hit", hits, err)
	}
	snippet := hits[0].Snippet
	if !strings.Contains(snippet, "needle") {
		t.Fatalf("snippet = %q, want it to contain needle", snippet)
	}
	if len(snippet) > 200 {
		t.Fatalf("snippet len = %d, want <= 200", len(snippet))
	}
	if strings.Contains(snippet, "\n") {
		t.Fatalf("snippet = %q, want a single line", snippet)
	}
}

func TestGrepRejectsAnUncompilablePattern(t *testing.T) {
	patterns := []string{
		"index-time(",
		// Postgres refuses to compile this one rather than scanning with it.
		"((((((((((a{1,10}){1,10}){1,10}){1,10}){1,10}){1,10}){1,10}){1,10}){1,10}){1,10}",
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			seed(t)
			cs, tenant := fixture(t)
			_, err := cs.Grep(ctx, tenant, pattern, 10)
			if err == nil || !strings.Contains(err.Error(), "regular expression") {
				t.Fatalf("Grep(%q) err = %v, want a 'regular expression' error", pattern, err)
			}
		})
	}
}

func TestGrepIsCancelledRatherThanHoldingThePod(t *testing.T) {
	// Every store call blocks the pod's event loop, so an expensive pattern must be
	// the caller's problem and not every sibling agent's.
	s := openApp(t)
	cs := store.NewConceptStore(s)
	tenant := testTenant(t)
	err := s.Scope(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			"INSERT INTO concept (tenant_id, path, type, body) "+
				"SELECT $1, 'p/' || g, 'Concept', repeat('lorem ipsum dolor ', 100) "+
				"FROM generate_series(1, 2000) g",
			tenant)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// The scan costs ~10ms, so 1ms cancels with an order of magnitude to spare.
	restore := store.SetGrepTimeoutMS(1)
	defer restore()
	_, err = cs.Grep(ctx, tenant, "(lorem|ipsum|dolor)+ z", 10)
	if err == nil || !strings.Contains(err.Error(), "took longer than 1ms") {
		t.Fatalf("Grep err = %v, want 'took longer than 1ms'", err)
	}
}

func TestBacklinksAreComputedNotStored(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	bl, err := cs.Backlinks(ctx, tenant, "detect/dormant")
	if err != nil || !slices.Equal(bl, []string{"splunk/cursor"}) {
		t.Fatalf("Backlinks(detect/dormant) = %v, err %v", bl, err)
	}
	bl, err = cs.Backlinks(ctx, tenant, "auth/flow")
	if err != nil || len(bl) != 0 {
		t.Fatalf("Backlinks(auth/flow) = %v, err %v, want empty", bl, err)
	}
}

func TestListReturnsChildrenOfPrefix(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	got, err := cs.List(ctx, tenant, "detect/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.PathType{{Path: "detect/dormant", Type: "Concept"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List(detect/) = %+v, want %+v", got, want)
	}
	all, err := cs.List(ctx, tenant, "")
	if err != nil || len(all) != len(seedData) {
		t.Fatalf("List(\"\") = %d entries, err %v, want %d", len(all), err, len(seedData))
	}
}

func TestListTreatsThePrefixLiterally(t *testing.T) {
	// LIKE would read the underscore as a wildcard.
	create(t, okf.Concept{Path: "a_b/one", Type: "Concept"})
	create(t, okf.Concept{Path: "axb/two", Type: "Concept"})
	cs, tenant := fixture(t)
	got, err := cs.List(ctx, tenant, "a_b/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.PathType{{Path: "a_b/one", Type: "Concept"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("List(a_b/) = %+v, want %+v", got, want)
	}
}

// The admin console's read paths. tenant=nil means every tenant and must take
// AdminScope; a concrete tenant must never fall through to it.

func summaryPaths(page []store.Summary) []string {
	paths := make([]string, len(page))
	for i, s := range page {
		paths[i] = s.Path
	}
	return paths
}

func TestPageReturnsSummariesUnderPrefix(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	page, err := cs.Page(ctx, &tenant, "detect/", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(summaryPaths(page), []string{"detect/dormant"}) {
		t.Fatalf("Page(detect/) = %+v", page)
	}
	if page[0].Type != "Concept" || page[0].Title != "Dormant Rule Identification" {
		t.Fatalf("Page(detect/)[0] = %+v", page[0])
	}
}

func TestPageRespectsLimitAndOffset(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	first, err := cs.Page(ctx, &tenant, "", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cs.Page(ctx, &tenant, "", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(summaryPaths(first), []string{"auth/flow", "detect/dormant"}) {
		t.Fatalf("Page(limit=2, offset=0) = %+v", first)
	}
	if !slices.Equal(summaryPaths(second), []string{"splunk/cursor"}) {
		t.Fatalf("Page(limit=2, offset=2) = %+v", second)
	}
}

func TestPageOfANegativeLimitIsEmptyNotInvalid(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	page, err := cs.Page(ctx, &tenant, "", -1, 0)
	if err != nil || len(page) != 0 {
		t.Fatalf("Page(limit=-1) = %v, err %v, want empty", page, err)
	}
}

func TestCountMatchesWhatPageCovers(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	n, err := cs.Count(ctx, &tenant, "")
	if err != nil || n != len(seedData) {
		t.Fatalf("Count(\"\") = %d, err %v, want %d", n, err, len(seedData))
	}
	n, err = cs.Count(ctx, &tenant, "detect/")
	if err != nil || n != 1 {
		t.Fatalf("Count(detect/) = %d, err %v, want 1", n, err)
	}
}

func TestPageOfOneTenantNeverReturnsAnothersRows(t *testing.T) {
	other := uuid.New()
	seed(t)
	cs, tenant := fixture(t)
	if _, _, err := cs.Create(ctx, other, okf.Concept{Path: "other/one", Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}
	page, err := cs.Page(ctx, &tenant, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range page {
		if s.Path == "other/one" {
			t.Fatalf("Page(tenant) leaked another tenant's row: %+v", page)
		}
		if s.TenantID != tenant {
			t.Fatalf("Page(tenant) row tenant_id = %v, want %v", s.TenantID, tenant)
		}
	}
}

func TestPageOfEveryTenantReturnsBoth(t *testing.T) {
	other := uuid.New()
	seed(t)
	cs, tenant := fixture(t)
	if _, _, err := cs.Create(ctx, other, okf.Concept{Path: "other/one", Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}
	page, err := cs.Page(ctx, nil, "", 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]bool{}
	for _, s := range page {
		seen[s.TenantID] = true
	}
	if !seen[tenant] || !seen[other] {
		t.Fatalf("Page(nil) tenants seen = %v, want both %v and %v", seen, tenant, other)
	}
}

// Two tenants can hold the same path, so under AdminScope path alone is not a
// total order. Paging the same path one row at a time is what catches a missing
// tie-breaker: distinct paths across tenants would not.
func TestPageOfEveryTenantBreaksATiedPathByTenantId(t *testing.T) {
	cs, tenant := fixture(t)
	other := uuid.New()
	// Unique per run, not a bare literal: an admin-scope query sees every tenant
	// that ever wrote this path, and TestMain's database is never reset between
	// `go test -count=N` runs.
	path := "decisions/retry-policy/" + tenant.String()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: path, Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.Create(ctx, other, okf.Concept{Path: path, Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}

	first, err := cs.Page(ctx, nil, path, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cs.Page(ctx, nil, path, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Path != path || len(second) != 1 || second[0].Path != path {
		t.Fatalf("first = %+v, second = %+v, want one %q row each", first, second, path)
	}
	if first[0].TenantID == second[0].TenantID {
		t.Fatalf("first and second tenant_id both %v, want distinct", first[0].TenantID)
	}
	got := map[uuid.UUID]bool{first[0].TenantID: true, second[0].TenantID: true}
	if !got[tenant] || !got[other] {
		t.Fatalf("tenants seen = %v, want %v and %v", got, tenant, other)
	}
}

func TestTotalsCountsConceptsTypesRevisionsLinksAndOrphans(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	totals, err := cs.Totals(ctx, &tenant)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Concepts != len(seedData) {
		t.Errorf("Concepts = %d, want %d", totals.Concepts, len(seedData))
	}
	if want := map[string]int{"Concept": len(seedData)}; !reflect.DeepEqual(totals.ByType, want) {
		t.Errorf("ByType = %v, want %v", totals.ByType, want)
	}
	if totals.Revisions != len(seedData) {
		t.Errorf("Revisions = %d, want %d", totals.Revisions, len(seedData))
	}
	// Only splunk/cursor carries a link, to detect/dormant.
	if totals.Links != 1 {
		t.Errorf("Links = %d, want 1", totals.Links)
	}
	// detect/dormant has an inbound link; the other two don't.
	if totals.Orphans != 2 {
		t.Errorf("Orphans = %d, want 2", totals.Orphans)
	}
}

func revisionPaths(revs []store.Revision) []string {
	paths := make([]string, len(revs))
	for i, r := range revs {
		paths[i] = r.Path
	}
	return paths
}

func TestActivityReturnsRecentRevisionsNewestFirst(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	revs, err := cs.Activity(ctx, &tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	// seed writes dormant, then flow, then cursor: newest-first reverses that.
	want := []string{"splunk/cursor", "auth/flow", "detect/dormant"}
	if !slices.Equal(revisionPaths(revs), want) {
		t.Fatalf("Activity paths = %v, want %v", revisionPaths(revs), want)
	}
}

// A path is only unique within a tenant, so two tenants can both write
// decisions/policy. Without tenant_id on each row, an all-tenants feed would
// render both writes as the same concept.
func TestActivityOfEveryTenantTagsEachRowWithItsOwnTenant(t *testing.T) {
	other := uuid.New()
	cs, tenant := fixture(t)
	// Unique per run: Activity(nil, 10)'s tenant-wide window can still hold an
	// earlier run's row for a bare literal path, since TestMain's database is
	// never reset between `go test -count=N` runs.
	path := "decisions/policy/" + tenant.String()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: path, Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.Create(ctx, other, okf.Concept{Path: path, Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}

	revs, err := cs.Activity(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	var samePath []store.Revision
	for _, r := range revs {
		if r.Path == path {
			samePath = append(samePath, r)
		}
	}
	if len(samePath) != 2 {
		t.Fatalf("revisions for %s = %+v, want 2", path, samePath)
	}
	seen := map[uuid.UUID]bool{samePath[0].TenantID: true, samePath[1].TenantID: true}
	if !seen[tenant] || !seen[other] {
		t.Fatalf("tenants seen = %v, want %v and %v", seen, tenant, other)
	}
}

// A path is unique only within a tenant, so a link held by one tenant must not
// un-orphan the same path in another. Both directions are asserted: a fix that
// only correlated one side of the anti-join would still pass half of this.
func TestTotalsCountsAnOrphanPerTenantRatherThanAcrossTenants(t *testing.T) {
	other := uuid.New()
	cs, tenant := fixture(t)
	path := "orphans/shared-path"
	before, err := cs.Totals(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: path, Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "orphans/linker", Type: "Concept", Links: []string{path}}, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.Create(ctx, other, okf.Concept{Path: path, Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}

	// Linked from inside its own tenant, so only the linker is orphaned here.
	tenantTotals, err := cs.Totals(ctx, &tenant)
	if err != nil || tenantTotals.Orphans != 1 {
		t.Fatalf("Totals(tenant).Orphans = %d, err %v, want 1", tenantTotals.Orphans, err)
	}
	// The other tenant's copy of that path is linked from nowhere in its own tenant.
	otherTotals, err := cs.Totals(ctx, &other)
	if err != nil || otherTotals.Orphans != 1 {
		t.Fatalf("Totals(other).Orphans = %d, err %v, want 1", otherTotals.Orphans, err)
	}
	// Two new orphans across every tenant; one, if a foreign link can un-orphan.
	after, err := cs.Totals(ctx, nil)
	if err != nil || after.Orphans != before.Orphans+2 {
		t.Fatalf("Totals(nil).Orphans = %d, err %v, want %d", after.Orphans, err, before.Orphans+2)
	}
}

// created_at, path, version is not a total order under AdminScope: two tenants can
// hold one path at one version, and a bulk write shares a clock reading. Both rows
// go in one transaction so now() ties created_at exactly.
func TestActivityOfEveryTenantBreaksATiedRevisionByTenantId(t *testing.T) {
	s := openApp(t)
	tenant := testTenant(t)
	other := uuid.New()
	low, high := tenant, other
	if strings.Compare(low.String(), high.String()) > 0 {
		low, high = high, low
	}
	path := "activity/tied"
	const insertRevision = "INSERT INTO concept_revision (tenant_id, path, version, op, snapshot) " +
		"VALUES ($1, $2, 1, 'create', '{}'::jsonb)"

	err := s.Scope(ctx, low, func(tx pgx.Tx) error {
		// Lowest tenant first: an untied LIMIT keeps the row it scanned first, which
		// is the one a descending tie-break must not return.
		if _, err := tx.Exec(ctx, insertRevision, low, path); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", store.TenantGUC, high.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, insertRevision, high, path)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	cs := store.NewConceptStore(s)
	newest, err := cs.Activity(ctx, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(newest) != 1 || newest[0].Path != path || newest[0].TenantID != high {
		t.Fatalf("Activity(nil, 1) = %+v, want one row for %q tagged %v", newest, path, high)
	}
	again, err := cs.Activity(ctx, nil, 1)
	if err != nil || !reflect.DeepEqual(again, newest) {
		t.Fatalf("Activity(nil, 1) again = %+v, err %v, want %+v", again, err, newest)
	}
}

// activity()'s cap is tenant-wide, so a quiet concept can fall off it.
// RevisionsFor filters by path in SQL, so other paths cannot crowd it out.
func TestRevisionsForIsImmuneToOtherPathsCrowdingTheFeed(t *testing.T) {
	cs, tenant := fixture(t)
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "detect/dormant", Type: "Concept", Title: "v1"}, "seed"); err != nil {
		t.Fatal(err)
	}
	one := 1
	if _, _, err := cs.Update(ctx, tenant, okf.Concept{Path: "detect/dormant", Type: "Concept", Title: "v2"}, "seed", &one); err != nil {
		t.Fatal(err)
	}
	// More noisy revisions on other paths than the limit passed below: with a
	// tenant-wide scan, these alone would crowd "detect/dormant" out entirely.
	for i := range 5 {
		if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "noise/" + string(rune('0'+i)), Type: "Concept", Title: "noise"}, "seed"); err != nil {
			t.Fatal(err)
		}
	}

	revs, err := cs.RevisionsFor(ctx, &tenant, "detect/dormant", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || revs[0].Version != 2 || revs[1].Version != 1 {
		t.Fatalf("RevisionsFor versions = %+v, want [2, 1]", revs)
	}
	for _, r := range revs {
		if r.TenantID != tenant {
			t.Fatalf("revision tenant_id = %v, want %v", r.TenantID, tenant)
		}
	}
}

func TestDailyWritesGroupsByDayAndZeroFillsGaps(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	writes, err := cs.DailyWrites(ctx, &tenant, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 7 {
		t.Fatalf("len(writes) = %d, want 7", len(writes))
	}
	last := writes[len(writes)-1]
	today := time.Now().UTC().Format("2006-01-02")
	if last.Date.Format("2006-01-02") != today || last.Count != len(seedData) {
		t.Fatalf("last write = %+v, want today (%s) with count %d", last, today, len(seedData))
	}
	for _, w := range writes[:len(writes)-1] {
		if w.Count != 0 {
			t.Fatalf("write = %+v, want count 0", w)
		}
	}
}

func TestTenantsListsEveryTenantWithItsConceptCount(t *testing.T) {
	cs, _ := fixture(t)
	a, b := uuid.New(), uuid.New()
	for _, p := range []string{"a/one", "a/two"} {
		if _, _, err := cs.Create(ctx, a, okf.Concept{Path: p, Type: "Concept"}, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := cs.Create(ctx, b, okf.Concept{Path: "b/one", Type: "Concept"}, "seed"); err != nil {
		t.Fatal(err)
	}

	counts, err := cs.Tenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]int{}
	for _, tc := range counts {
		got[tc.TenantID] = tc.Count
	}
	if got[a] != 2 {
		t.Errorf("counts[a] = %d, want 2", got[a])
	}
	if got[b] != 1 {
		t.Errorf("counts[b] = %d, want 1", got[b])
	}
}

func TestRevisionsReturnsOldestFirstAndEmptyForANegativeLimit(t *testing.T) {
	seed(t)
	cs, tenant := fixture(t)
	revs, err := cs.Revisions(ctx, tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	// seed writes dormant, then flow, then cursor: oldest-first keeps that order.
	want := []string{"detect/dormant", "auth/flow", "splunk/cursor"}
	if !slices.Equal(revisionPaths(revs), want) {
		t.Fatalf("Revisions paths = %v, want %v", revisionPaths(revs), want)
	}
	empty, err := cs.Revisions(ctx, tenant, -1)
	if err != nil || len(empty) != 0 {
		t.Fatalf("Revisions(-1) = %v, err %v, want empty", empty, err)
	}
}

// Graph is exercised at the store level here: the node/edge/truncation shaping in
// 2de90d2:tests/test_api.py's graph tests lives in the API layer, out of scope for this task.
func TestGraphReturnsPathTypeTitleAndLinksOrderedByPath(t *testing.T) {
	create(t, okf.Concept{Path: "b", Type: "Concept", Title: "B", Links: []string{"a", "ghost"}})
	create(t, okf.Concept{Path: "a", Type: "Concept", Title: "A", Links: []string{"b"}})
	cs, tenant := fixture(t)

	rows, err := cs.Graph(ctx, tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.GraphRow{
		{Path: "a", Type: "Concept", Title: "A", Links: []string{"b"}},
		{Path: "b", Type: "Concept", Title: "B", Links: []string{"a", "ghost"}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("Graph = %+v, want %+v", rows, want)
	}
}

func TestGraphRespectsLimit(t *testing.T) {
	create(t, okf.Concept{Path: "a", Type: "Concept"})
	create(t, okf.Concept{Path: "b", Type: "Concept"})
	cs, tenant := fixture(t)

	rows, err := cs.Graph(ctx, tenant, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "a" {
		t.Fatalf("Graph(limit=1) = %+v, want just \"a\"", rows)
	}
}
