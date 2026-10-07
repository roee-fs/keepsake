package server

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

func computationMD(body string) string {
	return "---\ntype: Attested Computation\nruntime: bigquery\n---\n" + body + "\n"
}

// storedComputation is a tenant holding docs/rev, an Attested Computation, and docs/note, as an import leaves them.
func storedComputation(t *testing.T, cs *store.ConceptStore) uuid.UUID {
	t.Helper()
	tenant := uuid.New()
	rev, err := okf.Parse(computationMD("SELECT 1"), "docs/rev")
	if err != nil {
		t.Fatal(err)
	}
	note, _ := okf.Parse(md("Doc", "n"), "docs/note")
	if _, err := cs.ImportMany(ctx, tenant, []okf.Concept{rev, note}, "process:import"); err != nil {
		t.Fatal(err)
	}
	return tenant
}

func TestAnUploadThatChangesAComputationNeedsTheComputationsScope(t *testing.T) {
	cs := conceptStore(t)
	h := issuer.Middleware(replaceBundle(cs))
	note := entry{name: "note.md", body: md("Doc", "n")}
	same := entry{name: "rev.md", body: computationMD("SELECT 1")}
	for name, tc := range map[string]struct {
		entries []entry
		path    string
	}{
		"add":    {[]entry{same, note, {name: "new.md", body: computationMD("SELECT 3")}}, "docs/new"},
		"change": {[]entry{{name: "rev.md", body: computationMD("SELECT 2")}, note}, "docs/rev"},
		"delete": {[]entry{note}, "docs/rev"},
		"retype": {[]entry{{name: "rev.md", body: md("Metric", "SELECT 1")}, note}, "docs/rev"},
	} {
		t.Run(name, func(t *testing.T) {
			tenant := storedComputation(t, cs)
			code, body := put(t, h, "docs", tarball(t, tc.entries...), tenant)
			if code != http.StatusForbidden || !strings.Contains(body, "computations") || !strings.Contains(body, tc.path) {
				t.Fatalf("PUT = %d %s", code, body)
			}
			if c, err := cs.Read(ctx, tenant, "docs/rev"); err != nil || c == nil || c.Body != "SELECT 1\n" || c.Type != okf.AttestedComputation {
				t.Fatalf("docs/rev = %+v, %v", c, err)
			}
			if p := listPaths(t, cs, tenant); !reflect.DeepEqual(p, []string{"docs/note", "docs/rev"}) {
				t.Fatalf("paths = %v", p)
			}
		})
	}
}

func TestAnUploadThatLeavesComputationsAloneNeedsOnlyTheBundleScope(t *testing.T) {
	cs := conceptStore(t)
	h := issuer.Middleware(replaceBundle(cs))
	retitled := "---\ntype: Attested Computation\ntitle: Revenue\nruntime: bigquery\n---\nSELECT 1\n"
	for name, rev := range map[string]string{"identical": computationMD("SELECT 1"), "retitled": retitled} {
		t.Run(name, func(t *testing.T) {
			entries := tarball(t, entry{name: "rev.md", body: rev}, entry{name: "note.md", body: md("Doc", "edited")})
			if code, body := put(t, h, "docs", entries, storedComputation(t, cs)); code != http.StatusOK {
				t.Fatalf("PUT = %d %s", code, body)
			}
		})
	}
}

func TestAnUploadWithBothScopesMayChangeAComputation(t *testing.T) {
	cs := conceptStore(t)
	tenant := storedComputation(t, cs)
	entries := tarball(t, entry{name: "rev.md", body: computationMD("SELECT 2")})
	code, body := putWith(t, issuer.Middleware(replaceBundle(cs)), "docs", entries, tenant, uploadScope, computationsScope)
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if c, _ := cs.Read(ctx, tenant, "docs/rev"); c == nil || c.Body != "SELECT 2\n" {
		t.Fatalf("docs/rev = %+v", c)
	}
}
