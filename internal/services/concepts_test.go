// Ported from 2de90d2:tests/test_tools.py: the tool bodies, called directly.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/migrate"
	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/repositories"
	"github.com/roee-fs/keepsake/okf"
)

var (
	db  *pgtest.DB
	ctx = context.Background()
)

// secret is distinctive enough that its appearance anywhere is proof, not coincidence.
const secret = "zqxjkbody"

func TestMain(m *testing.M) {
	pgtest.Main(m, func(d *pgtest.DB) error {
		db = d
		return migrate.Up(ctx, d.OwnerDSN, "okf")
	})
}

// obj builds an ordered JSON object from key, value pairs.
func obj(kv ...any) *okf.Map {
	m := okf.NewMap()
	for i := 0; i < len(kv); i += 2 {
		m.Set(kv[i].(string), kv[i+1])
	}
	return m
}

func conceptStore(t *testing.T) *repositories.ConceptRepository {
	return pgtest.ConceptRepository(t, db.AppDSN)
}

func newTools(t *testing.T) *ConceptService {
	t.Helper()
	return NewConceptService(conceptStore(t), uuid.New(), "mcp")
}

func seed(t *testing.T, tools *ConceptService, path string, kw map[string]any) {
	t.Helper()
	if _, ok := kw["type"]; !ok {
		kw["type"] = "Concept"
	}
	if _, err := tools.Create(ctx, path, kw); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, tools *ConceptService, path string, include ...string) *Concept {
	t.Helper()
	c, err := tools.Read(ctx, path, ReadOptions{Include: include})
	if err != nil || c == nil {
		t.Fatalf("read %s = %v, %v", path, c, err)
	}
	return c
}

// toolError asserts err is a ToolError and returns its message.
func toolError(t *testing.T, err error) string {
	t.Helper()
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want a *ToolError", err)
	}
	return te.Msg
}

// Each read tool logs one read, and a write logs none.
func TestReadToolsLogOneReadEach(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{})
	read(t, tools, "a/b")
	if _, err := tools.Search(ctx, "b", 5, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Grep(ctx, "b", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.List(ctx, "", 200); err != nil {
		t.Fatal(err)
	}
	tenant := tools.Tenant()
	buckets, err := tools.c.Timeseries(ctx, &tenant, "day", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].Reads != 4 || buckets[0].Writes != 1 {
		t.Fatalf("buckets = %+v, want 4 reads and 1 write today", buckets)
	}
}

func wantToolError(t *testing.T, err error, contains string) {
	t.Helper()
	if msg := toolError(t, err); !strings.Contains(msg, contains) {
		t.Fatalf("error %q does not contain %q", msg, contains)
	}
}

func TestCreateRejectsAConceptWithoutType(t *testing.T) {
	_, err := newTools(t).Create(ctx, "a/b", map[string]any{"type": "", "title": "x", "description": "", "body": "", "links": []any{}})
	wantToolError(t, err, "type is required")
}

// There is no delete tool, so one concept at `index` would make the whole tenant
// un-exportable for good.
func TestCreateRejectsAPathReservedForAGeneratedBundleFile(t *testing.T) {
	tools := newTools(t)
	_, err := tools.Create(ctx, "index", map[string]any{"type": "Concept"})
	wantToolError(t, err, "reserved")
	_, err = tools.Create(ctx, "architecture/index", map[string]any{"type": "Concept"})
	wantToolError(t, err, "reserved")
}

func TestCreateRejectsAPathThatIsTaken(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "first"})
	_, err := tools.Create(ctx, "a/b", map[string]any{"type": "Concept", "body": "second"})
	wantToolError(t, err, "already exists")
}

func TestLinksComeFromTheBodyNotFromTheArgument(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "see [y](/a/y.md)", "links": []any{"forged/edge"}})
	if got := read(t, tools, "a/b").Links; !reflect.DeepEqual(got, []string{"a/y"}) {
		t.Fatalf("links = %v", got)
	}
}

func TestUpdateConflictReturnsCurrentContent(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/c", map[string]any{"title": "t", "body": "v1"})
	one := read(t, tools, "a/c").Version
	two, err := tools.Update(ctx, "a/c", &one, map[string]any{"body": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tools.Update(ctx, "a/c", &one, map[string]any{"body": "v3"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (ConflictResult{Conflict: true, CurrentVersion: two.(WriteResult).Version, CurrentBody: "v2"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestUpdateKeepsTheFieldsItWasNotGiven(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"title": "Original", "description": "D", "body": "v1", "frontmatter": obj("owner", "sec")})
	if _, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "v2"}); err != nil {
		t.Fatal(err)
	}
	c := read(t, tools, "a/b", "generated")
	owner, _ := c.Frontmatter.Get("owner")
	if c.Title != "Original" || c.Description != "D" || c.Body != "v2" || owner != "sec" || !slices.Equal(c.Frontmatter.Keys(), []string{"owner", "generated"}) {
		t.Fatalf("got %+v", c)
	}
}

func TestFrontmatterThatIsNotAnObjectIsCorrectable(t *testing.T) {
	_, err := newTools(t).Create(ctx, "a/b", map[string]any{"type": "Concept", "frontmatter": "owner: sec"})
	wantToolError(t, err, "frontmatter must be an object")
}

// null is the likeliest way an agent says "leave it alone"; it MUST NOT wipe.
func TestANullFrontmatterIsRefusedRatherThanErasing(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "v1", "frontmatter": obj("owner", "sec")})
	_, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "v2", "frontmatter": nil})
	wantToolError(t, err, "frontmatter must be an object")
	if fm := read(t, tools, "a/b", "generated").Frontmatter; !slices.Equal(fm.Keys(), []string{"owner", "generated"}) {
		t.Fatalf("frontmatter keys = %v", fm.Keys())
	} else if owner, _ := fm.Get("owner"); owner != "sec" {
		t.Fatalf("owner = %v", owner)
	}
}

func TestANullTextFieldIsRefusedRatherThanErasing(t *testing.T) {
	for _, field := range []string{"body", "type", "title", "description"} {
		t.Run(field, func(t *testing.T) {
			tools := newTools(t)
			seed(t, tools, "a/b", map[string]any{"title": "T", "description": "D", "body": "v1"})
			_, err := tools.Update(ctx, "a/b", nil, map[string]any{field: nil})
			wantToolError(t, err, field+" must be a string")
			c := read(t, tools, "a/b")
			if c.Body != "v1" || c.Type != "Concept" || c.Title != "T" || c.Description != "D" {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestUpdateOfAnotherTenantsPathIsIndistinguishableFromAbsent(t *testing.T) {
	tools := newTools(t)
	if _, _, err := tools.c.Create(ctx, uuid.New(), okf.Concept{Path: "a/b", Type: "Concept", Body: secret, Frontmatter: okf.NewMap()}, "seed"); err != nil {
		t.Fatal(err)
	}
	_, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "mine"})
	taken := toolError(t, err)
	_, err = tools.Update(ctx, "nowhere/at/all", nil, map[string]any{"body": "mine"})
	absent := toolError(t, err)
	if strings.Contains(taken, secret) {
		t.Fatalf("leaked a body: %q", taken)
	}
	if strings.ReplaceAll(taken, "a/b", "X") != strings.ReplaceAll(absent, "nowhere/at/all", "X") {
		t.Fatalf("distinguishable: %q vs %q", taken, absent)
	}
}

// Python monkeypatches read to return a phantom; here the phantom goes straight to write.
func TestUpdateOfARowThatVanishedMidWriteIsNotFound(t *testing.T) {
	tools := newTools(t)
	phantom := okf.Concept{Path: "ghost/path", Type: "Concept", Frontmatter: okf.NewMap()}
	_, err := tools.write(ctx, phantom, "ghost/path", nil, map[string]any{"body": "x"}, true)
	if msg := toolError(t, err); msg != "no concept at ghost/path" {
		t.Fatalf("got %q", msg)
	}
}

func TestSearchReturnsCardsAndNeverABody(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "detect/dormant", map[string]any{"title": "Dormant Rule", "body": secret})
	hits, err := tools.Search(ctx, "dormant", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Path != "detect/dormant" {
		t.Fatalf("hits = %+v", hits)
	}
	if b, _ := json.Marshal(hits); strings.Contains(string(b), secret) {
		t.Fatalf("leaked a body: %s", b)
	}
}

func TestSearchTermsAreOredSoExtraTermsBroaden(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/one", map[string]any{"title": "Dormant Rule"})
	seed(t, tools, "a/two", map[string]any{"title": "Splunk Cursor"})
	hits, err := tools.Search(ctx, "dormant splunk", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.Path] = true
	}
	if !reflect.DeepEqual(got, map[string]bool{"a/one": true, "a/two": true}) {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestSearchAndReadCarryTrustSignals(t *testing.T) {
	tools := newTools(t)
	fm := okf.NewMap()
	fm.Set("status", "deprecated")
	fm.Set("stale_after", "2000-01-01T00:00:00Z")
	fm.Set("verified", obj("by", "human:ann", "at", "2026-06-25T09:00:00Z"))
	fm.Set("generated", obj("by", "agent/v1", "at", "2026-06-20T22:53:05Z"))
	// Written through the store, since a tool write would replace generated with its own stamp.
	if _, _, err := tools.c.Create(ctx, tools.t, okf.Concept{Path: "a/old", Type: "Concept", Title: "Zqxold", Frontmatter: fm}, "process:import"); err != nil {
		t.Fatal(err)
	}
	seed(t, tools, "a/new", map[string]any{"title": "Zqxold"})
	_, stamped := generated(t, tools, "a/new")

	want := map[string]okf.Signals{
		"a/old": {Status: "deprecated", Stale: true, Trust: okf.HumanReviewed, GeneratedAt: "2026-06-20T22:53:05Z"},
		"a/new": {Status: "stable", Trust: okf.Unverified, GeneratedAt: stamped},
	}
	hits, err := tools.Search(ctx, "zqxold", 5, nil)
	if err != nil || len(hits) != 2 {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	for _, h := range hits {
		if h.Signals != want[h.Path] {
			t.Errorf("search %s: %+v", h.Path, h.Signals)
		}
		c, err := tools.Read(ctx, h.Path, ReadOptions{})
		if err != nil || c == nil || c.Signals != want[h.Path] {
			t.Errorf("read %s: %+v, %v", h.Path, c, err)
		}
	}
}

func TestSearchRanksADeprecatedConceptLikeAnyOther(t *testing.T) {
	tools := newTools(t)
	fm := okf.NewMap()
	fm.Set("status", "deprecated")
	seed(t, tools, "a/a", map[string]any{"title": "Zqxtie", "frontmatter": fm})
	seed(t, tools, "a/b", map[string]any{"title": "Zqxtie"})
	hits, err := tools.Search(ctx, "zqxtie", 5, nil)
	if err != nil || len(hits) != 2 || hits[0].Score != hits[1].Score {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
}

func generated(t *testing.T, tools *ConceptService, path string) (by, at string) {
	t.Helper()
	g, _ := read(t, tools, path, "generated").Frontmatter.Get("generated")
	m, _ := g.(*okf.Map)
	if m == nil {
		t.Fatalf("%s has no generated: %v", path, g)
	}
	b, _ := m.Get("by")
	a, _ := m.Get("at")
	by, _ = b.(string)
	at, _ = a.(string)
	return by, at
}

func TestEveryAuthoringWriteStampsWhoWroteItAndWhen(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	agent, human := NewConceptService(cs, tenant, "support-agent/1.4"), NewConceptService(cs, tenant, "human:ann")
	before := time.Now().UTC().Add(-time.Minute)

	seed(t, agent, "a/b", map[string]any{"frontmatter": obj("generated", "yes")})
	by, at := generated(t, agent, "a/b")
	stamp, err := time.Parse(time.RFC3339, at)
	if by != "support-agent/1.4" || err != nil || stamp.Before(before) || !strings.HasSuffix(at, "Z") {
		t.Fatalf("create: by %q at %q (%v)", by, at, err)
	}

	if _, err := human.Update(ctx, "a/b", nil, map[string]any{"body": "edited"}); err != nil {
		t.Fatal(err)
	}
	if by, _ := generated(t, human, "a/b"); by != "human:ann" {
		t.Fatalf("update: by %q", by)
	}

	_, edited := generated(t, human, "a/b")
	seed(t, agent, "a/c", map[string]any{})
	if _, err := agent.Relate(ctx, "a/b", "a/c"); err != nil {
		t.Fatal(err)
	}
	if by, at := generated(t, agent, "a/b"); by != "human:ann" || at != edited {
		t.Fatalf("relate: by %q at %q", by, at)
	}
}

func TestAStampLeavesTheCallersFrontmatterAsItWas(t *testing.T) {
	fm := obj("k", 1)
	seed(t, newTools(t), "a/b", map[string]any{"frontmatter": fm})
	if _, ok := fm.Get("generated"); ok || fm.Len() != 1 {
		t.Fatalf("frontmatter = %v", fm.Keys())
	}
}

func TestAnUpdateThatChangesNothingWritesNothing(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	ann, bob := NewConceptService(cs, tenant, "human:ann"), NewConceptService(cs, tenant, "human:bob")
	seed(t, ann, "a/b", map[string]any{"body": "same", "frontmatter": obj("k", 1)})
	before, err := ann.Read(ctx, "a/b", ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, at := generated(t, ann, "a/b")
	stale := before.Version - 1
	result, err := bob.Update(ctx, "a/b", &stale, map[string]any{"body": "same", "frontmatter": obj("k", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if want := (WriteResult{"a/b", before.Version}); result != want {
		t.Fatalf("got %+v, want %+v", result, want)
	}
	if by, now := generated(t, bob, "a/b"); by != "human:ann" || now != at {
		t.Fatalf("restamped: by %q at %q", by, now)
	}
	if _, _, history, err := cs.Detail(ctx, tenant, "a/b", 10); err != nil || len(history) != 1 {
		t.Fatalf("history %d, %v", len(history), err)
	}
}

func TestGrepReturnsASnippetForEveryMatch(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "alpha " + secret + " omega"})
	hits, err := tools.Grep(ctx, "al.ha", 5)
	if err != nil {
		t.Fatal(err)
	}
	if want := []GrepHit{{Path: "a/b", Snippet: "alpha " + secret + " omega"}}; !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestGrepReportsAnUnusablePatternWithoutABody(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": secret})
	_, err := tools.Grep(ctx, "alpha(", 5)
	if msg := toolError(t, err); strings.Contains(msg, secret) {
		t.Fatalf("leaked a body: %q", msg)
	}
}

func TestAWriteIsRefusedForAMalformedFamilyItChanges(t *testing.T) {
	_, err := newTools(t).Create(ctx, "a/b", map[string]any{"type": "Concept", "frontmatter": obj("stale_after", "2026-09-23")})
	wantToolError(t, err, "stale_after must be an ISO 8601 datetime")
}

func TestAMalformedFamilyAlreadyStoredDoesNotBlockAnUnrelatedEdit(t *testing.T) {
	tools := newTools(t)
	// Written through the store, as an import of an older bundle would.
	old := okf.Concept{Path: "a/b", Type: "Concept", Frontmatter: obj("stale_after", "2026-09-23")}
	if _, _, err := tools.c.Create(ctx, tools.t, old, "process:import"); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "edited"}); err != nil {
		t.Fatal(err)
	}
}

// jsonb stores object keys shortest first, so a caller's own key order MUST NOT read as a change.
func TestAFamilyRewrittenInAnotherKeyOrderIsUnchanged(t *testing.T) {
	tools := newTools(t)
	stored := obj("sources", []any{obj("resource", "r", "id", "s", "usage_count", -1)})
	old := okf.Concept{Path: "a/b", Type: "Concept", Frontmatter: stored}
	if _, _, err := tools.c.Create(ctx, tools.t, old, "process:import"); err != nil {
		t.Fatal(err)
	}
	echoed := obj("sources", []any{obj("usage_count", -1, "resource", "r", "id", "s")})
	if _, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "edited", "frontmatter": echoed}); err != nil {
		t.Fatal(err)
	}
}

func TestAnAddedNullFamilyIsChecked(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{})
	_, err := tools.Update(ctx, "a/b", nil, map[string]any{"frontmatter": obj("status", nil)})
	wantToolError(t, err, "status must be draft")
}

func TestACallerSentVerificationIsChecked(t *testing.T) {
	_, err := newTools(t).Create(ctx, "a/b", map[string]any{"type": "Concept", "frontmatter": obj("verified", obj("by", "human:ann"))})
	wantToolError(t, err, "verified[0].at")
}

func TestRetypingDoesNotRecheckAnUnrelatedStoredField(t *testing.T) {
	tools := newTools(t)
	old := okf.Concept{Path: "a/b", Type: "Concept", Frontmatter: obj("stale_after", "2026-09-23")}
	if _, _, err := tools.c.Create(ctx, tools.t, old, "process:import"); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Update(ctx, "a/b", nil, map[string]any{"type": "Metric"}); err != nil {
		t.Fatal(err)
	}
}

func TestRetypingToAnAttestedComputationChecksItsContract(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{})
	_, err := tools.Update(ctx, "a/b", nil, map[string]any{"type": okf.AttestedComputation})
	wantToolError(t, err, "runtime is required")
}

func TestABodyEditMustKeepTheComputation(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"type": okf.AttestedComputation, "frontmatter": obj("runtime", "bigquery"),
		"body": "# Computation\n\n```sql\nSELECT 1\n```\n"})
	_, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "# Computation\n\nGone.\n"})
	wantToolError(t, err, "exactly one code block under # Computation")
}

func TestABodyEditBesideAComputationFileIsChecked(t *testing.T) {
	tools := newTools(t)
	fm := obj("runtime", "bigquery", "computation", "lib/x.sql")
	old := okf.Concept{Path: "a/b", Type: okf.AttestedComputation, Body: "# Computation\n\n```sql\nSELECT 1\n```\n", Frontmatter: fm}
	if _, _, err := tools.c.Create(ctx, tools.t, old, "process:import"); err != nil {
		t.Fatal(err)
	}
	_, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": old.Body + "\n```sql\nSELECT 2\n```\n"})
	wantToolError(t, err, "computation names a file")
}

// A CRLF body predates write normalization, so it MUST NOT read as a body edit.
func TestAStoredConceptWithoutAComputationTakesOtherEdits(t *testing.T) {
	tools := newTools(t)
	old := okf.Concept{Path: "a/b", Type: okf.AttestedComputation, Body: "Revenue.\r\nMore.\r\n", Frontmatter: obj("runtime", "bigquery")}
	if _, _, err := tools.c.Create(ctx, tools.t, old, "process:import"); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Update(ctx, "a/b", nil, map[string]any{"title": "Revenue"}); err != nil {
		t.Fatal(err)
	}
	seed(t, tools, "a/c", map[string]any{})
	if _, err := tools.Relate(ctx, "a/b", "a/c"); err != nil {
		t.Fatal(err)
	}
}

func TestReadOfAMissingPathIsNil(t *testing.T) {
	c, err := newTools(t).Read(ctx, "nothing/here", ReadOptions{})
	if err != nil || c != nil {
		t.Fatalf("got %v, %v", c, err)
	}
}

func TestPagingThroughABodyRebuildsItExactly(t *testing.T) {
	tools := newTools(t)
	// Multi-byte and astral characters, so a byte-counted page would split one.
	body := strings.Repeat("aé🙂\n", 10)
	seed(t, tools, "a/b", map[string]any{"body": body})
	var got strings.Builder
	offset := 0
	for range 20 {
		c, err := tools.Read(ctx, "a/b", ReadOptions{Offset: offset, MaxChars: 7})
		if err != nil {
			t.Fatal(err)
		}
		if c.BodyChars != 40 {
			t.Fatalf("body_chars = %d, want 40", c.BodyChars)
		}
		got.WriteString(c.Body)
		if c.NextOffset == nil {
			break
		}
		offset = *c.NextOffset
	}
	if got.String() != body {
		t.Fatalf("pages rebuild %q, want %q", got.String(), body)
	}
}

func TestAnOffsetPastTheEndIsAnEmptyLastPage(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": "short"})
	c, err := tools.Read(ctx, "a/b", ReadOptions{Offset: 99})
	if err != nil || c.Body != "" || c.NextOffset != nil || c.BodyChars != 5 {
		t.Fatalf("got %+v, %v", c, err)
	}
	if c := read(t, tools, "a/b"); c.Body != "short" || c.NextOffset != nil {
		t.Fatalf("a whole body = %q, next_offset %v", c.Body, c.NextOffset)
	}
}

func TestReadCapsLinksAndBacklinksAndCountsThemAll(t *testing.T) {
	tools := newTools(t)
	n := MaxLinks + 3
	var body strings.Builder
	var want []string
	for i := range n {
		p := fmt.Sprintf("t/%02d", n-i)
		want = append(want, p)
		fmt.Fprintf(&body, "[x](/%s.md)\n", p)
		seed(t, tools, fmt.Sprintf("s/%02d", n-i), map[string]any{"body": "[hub](/hub.md)"})
	}
	seed(t, tools, "hub", map[string]any{"body": body.String()})
	c := read(t, tools, "hub")
	if !reflect.DeepEqual(c.Links, want[:MaxLinks]) || c.LinksCount != n {
		t.Fatalf("links = %v (%d), want the first %d of %d in body order", c.Links, c.LinksCount, MaxLinks, n)
	}
	if len(c.Backlinks) != MaxLinks || c.Backlinks[0] != "s/01" || !slices.IsSorted(c.Backlinks) || c.BacklinksCount != n {
		t.Fatalf("backlinks = %v (%d), want the first %d of %d by path", c.Backlinks, c.BacklinksCount, MaxLinks, n)
	}
}

func TestProvenanceIsLeftOutUnlessRequested(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"frontmatter": obj("rule_id", "R1", "sources", []any{obj("resource", "s3://x")})})
	c := read(t, tools, "a/b")
	if keys := c.Frontmatter.Keys(); !reflect.DeepEqual(keys, []string{"rule_id"}) {
		t.Fatalf("default frontmatter keys = %v", keys)
	}
	if c.GeneratedAt == "" {
		t.Fatal("generated_at MUST still come from the stamp left out of frontmatter")
	}
	c, err := tools.Read(ctx, "a/b", ReadOptions{Include: []string{"sources", "generated"}})
	if err != nil {
		t.Fatal(err)
	}
	if keys := c.Frontmatter.Keys(); !reflect.DeepEqual(keys, []string{"rule_id", "sources", "generated"}) {
		t.Fatalf("included frontmatter keys = %v", keys)
	}
}

func TestAnUpdateWithoutSourcesKeepsTheStoredSources(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"frontmatter": obj("status", "draft", "sources", []any{obj("resource", "s3://x")})})
	fm := read(t, tools, "a/b").Frontmatter
	fm.Set("status", "stable")
	if _, err := tools.Update(ctx, "a/b", nil, map[string]any{"frontmatter": fm}); err != nil {
		t.Fatal(err)
	}
	got := read(t, tools, "a/b", "sources").Frontmatter
	status, _ := got.Get("status")
	sources, _ := got.Get("sources")
	if b, _ := json.Marshal(sources); status != "stable" || string(b) != `[{"resource":"s3://x"}]` {
		t.Fatalf("frontmatter = %v", got)
	}
	if _, err := tools.Update(ctx, "a/b", nil, map[string]any{"frontmatter": obj("sources", []any{})}); err != nil {
		t.Fatal(err)
	}
	if sources, _ := read(t, tools, "a/b", "sources").Frontmatter.Get("sources"); !reflect.DeepEqual(sources, []any{}) {
		t.Fatalf("sources = %v, want them replaced when given", sources)
	}
}

func TestIdentifyingKeysSurviveAnOverBudgetFrontmatter(t *testing.T) {
	tools := newTools(t)
	big := strings.Repeat("x", FrontmatterBytes)
	// jsonb orders keys shorter first, so "a" leads, "bb" overruns and "after_the_overrun" follows it.
	seed(t, tools, "a/b", map[string]any{"frontmatter": obj(
		"a", "kept", "bb", big, "rule_id", "R1", "rule_uids", []any{"u1"}, "alert_names", []any{"A"},
		"status", "stable", "completeness", "partial", "after_the_overrun", "cut",
	)})
	c := read(t, tools, "a/b")
	want := []string{"a", "status", "rule_id", "rule_uids", "alert_names", "completeness"}
	if keys := c.Frontmatter.Keys(); !reflect.DeepEqual(keys, want) || !c.FrontmatterTruncated {
		t.Fatalf("keys = %v, truncated %v; want %v, true", keys, c.FrontmatterTruncated, want)
	}
	if id, _ := c.Frontmatter.Get("rule_id"); id != "R1" {
		t.Fatalf("rule_id = %v", id)
	}
}

func TestASmallFrontmatterIsNotMarkedTruncated(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"frontmatter": obj("rule_id", "R1")})
	if c := read(t, tools, "a/b"); c.FrontmatterTruncated {
		t.Fatalf("truncated a frontmatter of %v", c.Frontmatter.Keys())
	}
}

func TestTheFrontmatterBudgetCountsTheEscapedText(t *testing.T) {
	tools := newTools(t)
	// 2 KB as UTF-8, but 6 KB once the text content escapes each é.
	seed(t, tools, "a/b", map[string]any{"frontmatter": obj("note", strings.Repeat("é", 1000))})
	seed(t, tools, "a/c", map[string]any{"frontmatter": obj(strings.Repeat("é", 1000), "note")})
	for _, p := range []string{"a/b", "a/c"} {
		if c := read(t, tools, p); !c.FrontmatterTruncated {
			t.Fatalf("%s kept %v", p, c.Frontmatter.Keys())
		}
	}
}

func TestSnippetsStayWithinTheirCap(t *testing.T) {
	tools := newTools(t)
	// Four bytes a character, and no whitespace for either snippet to break at.
	long := strings.Repeat("🙂", 300)
	seed(t, tools, "a/b", map[string]any{"body": "dormant " + long + " dormant " + long})
	hits, err := tools.Grep(ctx, "dormant", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("grep = %v, %v", hits, err)
	}
	cards, err := tools.Search(ctx, "dormant", 5, nil)
	if err != nil || len(cards) != 1 {
		t.Fatalf("search = %v, %v", cards, err)
	}
	for _, s := range []string{hits[0].Snippet, cards[0].Snippet} {
		if len(s) > SnippetBytes || !utf8.ValidString(s) || !strings.Contains(s, "dormant") {
			t.Fatalf("snippet of %d bytes (cap %d): %q", len(s), SnippetBytes, s)
		}
	}
}

func TestASearchSnippetKeepsAMatchThatFollowsALongRun(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"body": strings.Repeat("QUJD", 200) + " dormant rules here"})
	cards, err := tools.Search(ctx, "dormant", 5, nil)
	if err != nil || len(cards) != 1 {
		t.Fatalf("search = %v, %v", cards, err)
	}
	if s := cards[0].Snippet; len(s) > SnippetBytes || !strings.Contains(s, "dormant") || strings.Contains(s, "\x02") {
		t.Fatalf("snippet of %d bytes: %q", len(s), s)
	}
}

func TestRelateRecordsAnEdgeBothWays(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/x", map[string]any{"body": "start"})
	seed(t, tools, "b/y", map[string]any{"body": "target"})
	if _, err := tools.Relate(ctx, "a/x", "b/y"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, tools, "a/x").Links; !reflect.DeepEqual(got, []string{"b/y"}) {
		t.Fatalf("links = %v", got)
	}
	if got := read(t, tools, "b/y").Backlinks; !reflect.DeepEqual(got, []string{"a/x"}) {
		t.Fatalf("backlinks = %v", got)
	}
}

// New: an agent that retries MUST NOT append the link twice.
func TestRelateIsIdempotent(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/x", map[string]any{"body": "start"})
	first, err := tools.Relate(ctx, "a/x", "b/y")
	if err != nil {
		t.Fatal(err)
	}
	second, err := tools.Relate(ctx, "a/x", "b/y")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || read(t, tools, "a/x").Body != "start\n\n[b/y](/b/y.md)\n" {
		t.Fatalf("first %+v, second %+v", first, second)
	}
}

// An imported body ends in a newline, so the link MUST NOT land after two blank lines.
func TestRelateLeavesOneBlankLineBeforeTheLink(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/x", map[string]any{"body": "start\n"})
	if _, err := tools.Relate(ctx, "a/x", "b/y"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, tools, "a/x").Body; got != "start\n\n[b/y](/b/y.md)\n" {
		t.Fatalf("body = %q", got)
	}
}

// A written body MUST come back LF-only, as an imported one does.
func TestWritesNormaliseCRLF(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/x", map[string]any{"body": "one\r\ntwo\r\n"})
	if got := read(t, tools, "a/x").Body; got != "one\ntwo\n" {
		t.Fatalf("created body = %q", got)
	}
	if _, err := tools.Update(ctx, "a/x", nil, map[string]any{"body": "three\r\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Relate(ctx, "a/x", "b/y"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, tools, "a/x").Body; got != "three\n\n[b/y](/b/y.md)\n" {
		t.Fatalf("updated body = %q", got)
	}
}

// New: a to_path whose link would not read back as to_path MUST NOT be appended.
func TestRelateRejectsANonCanonicalTarget(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/x", map[string]any{"body": "start"})
	for _, to := range []string{"/b/y", "./b/y", "b y", "b)y"} {
		_, err := tools.Relate(ctx, "a/x", to)
		wantToolError(t, err, "to_path "+okf.PyReprString(to)+" is not a concept path such as detect/dormant-rules")
	}
	if got := read(t, tools, "a/x").Body; got != "start" {
		t.Fatalf("body = %q", got)
	}
}

func TestRelateRejectsAMissingSource(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "b/y", map[string]any{})
	_, err := tools.Relate(ctx, "a/x", "b/y")
	wantToolError(t, err, "no concept at a/x")
}

func TestListReturnsPathsAndCountsByType(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/one", map[string]any{})
	seed(t, tools, "a/two", map[string]any{"type": "Runbook"})
	seed(t, tools, "b/three", map[string]any{})
	got, err := tools.List(ctx, "a/", 200)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(got); string(b) != `{"paths":["a/one","a/two"],"counts":{"Concept":1,"Runbook":1},"total":2,"truncated":false}` {
		t.Fatalf("got %s", b)
	}
}

func TestACappedListingSaysSoAndCountsEverything(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/one", map[string]any{})
	seed(t, tools, "a/two", map[string]any{"type": "Runbook"})
	seed(t, tools, "a/three", map[string]any{})
	got, err := tools.List(ctx, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(got); string(b) != `{"paths":["a/one","a/three"],"counts":{"Concept":2,"Runbook":1},"total":3,"truncated":true}` {
		t.Fatalf("got %s", b)
	}
}
