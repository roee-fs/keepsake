package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/roee-fs/keepsake/okf"
)

// seedVerified stores a concept the way an import of a human-reviewed bundle does.
func seedVerified(t *testing.T, tools *Tools, path string) {
	t.Helper()
	c := okf.Concept{Path: path, Type: "Concept", Body: "v1",
		Frontmatter: obj("owner", "sec", "verified", obj("by", "human:alice", "at", "2026-06-25T09:00:00+00:00"))}
	if _, _, err := tools.c.Create(ctx, tools.t, c, "import"); err != nil {
		t.Fatal(err)
	}
}

func verified(t *testing.T, tools *Tools, path string) bool {
	t.Helper()
	_, ok := read(t, tools, path).Frontmatter.Get("verified")
	return ok
}

func TestAToolCannotMarkAConceptVerified(t *testing.T) {
	tools := newTools(t)
	_, err := tools.Create(ctx, "a/b", map[string]any{"type": "Concept", "frontmatter": obj("verified", obj("by", "human:me"))})
	wantToolError(t, err, "verified")
	seedVerified(t, tools, "a/c")
	_, err = tools.Update(ctx, "a/c", nil, map[string]any{"frontmatter": obj("verified", obj("by", "human:me"))})
	wantToolError(t, err, "verified")
}

func TestAnUnchangedVerifiedMarkMayBePassedBack(t *testing.T) {
	tools := newTools(t)
	seedVerified(t, tools, "a/b")
	fm := read(t, tools, "a/b").Frontmatter
	fm.Set("status", "deprecated")
	got, err := tools.Update(ctx, "a/b", nil, map[string]any{"frontmatter": fm})
	if err != nil {
		t.Fatal(err)
	}
	if got.(writeResult).Unverified || !verified(t, tools, "a/b") {
		t.Fatalf("a frontmatter-only change cleared verified: %+v", got)
	}
}

func TestEditingTheTextClearsVerified(t *testing.T) {
	tools := newTools(t)
	seedVerified(t, tools, "a/b")
	got, err := tools.Update(ctx, "a/b", nil, map[string]any{"body": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.(writeResult).Unverified || verified(t, tools, "a/b") {
		t.Fatalf("verified survived an edit to the body: %+v", got)
	}
}

func TestRelatingKeepsVerified(t *testing.T) {
	tools := newTools(t)
	seedVerified(t, tools, "a/b")
	seed(t, tools, "a/c", map[string]any{})
	if _, err := tools.Relate(ctx, "a/b", "a/c"); err != nil {
		t.Fatal(err)
	}
	if !verified(t, tools, "a/b") {
		t.Fatal("appending a link cleared verified")
	}
}

func TestWritesStampGenerated(t *testing.T) {
	tools := newTools(t)
	seed(t, tools, "a/b", map[string]any{"frontmatter": obj("generated", obj("by", "human:forged"))})
	g, _ := read(t, tools, "a/b").Frontmatter.Get("generated")
	by, _ := g.(*okf.Map).Get("by")
	at, _ := g.(*okf.Map).Get("at")
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(at)); by != "mcp" || err != nil {
		t.Fatalf("generated = %v, want by mcp at an RFC 3339 instant", g)
	}
}
