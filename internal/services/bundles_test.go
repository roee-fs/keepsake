package services

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const doc = `---
type: Concept
title: Five Layer Architecture
---
The spec sits beneath the convention.
`

func writeFile(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceRefusesAnEmptyBundle(t *testing.T) {
	_, _, err := NewBundleService(conceptStore(t)).Replace(ctx, uuid.New(), "docs", nil, "human:x", false)
	var te *ToolError
	if !errors.As(err, &te) || te.Msg != "bundle holds no concepts" {
		t.Fatalf("err = %v, want the empty-bundle ToolError", err)
	}
}

func TestTheLogIsDatedAndSaysWhenItIsTruncated(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	b, src := NewBundleService(cs), t.TempDir()
	for _, step := range []struct{ name, text string }{
		{"layers.md", doc}, {"layers.md", strings.ReplaceAll(doc, "beneath", "above")}, {"other.md", doc},
	} {
		writeFile(t, src, filepath.Join("architecture", step.name), step.text)
		if _, err := b.Import(ctx, tenant, src); err != nil {
			t.Fatal(err)
		}
	}

	// Three revisions: layers, its edit, and other. The third import replays layers unchanged, which writes nothing.
	full, err := b.renderLog(ctx, tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(full, "- `architecture/"); n != 3 {
		t.Fatalf("%d entries:\n%s", n, full)
	}
	if strings.Contains(full, "omitted") {
		t.Fatal(full)
	}
	// OKF §9 lists a log newest first.
	newest, _, _ := strings.Cut(full[strings.Index(full, "`architecture/layers`"):], "\n")
	if !strings.Contains(newest, " update by") {
		t.Fatal(full)
	}
	revisions, err := cs.Revisions(ctx, tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "## "+revisions[0].Day) {
		t.Fatal(full)
	}
	if rs, _ := cs.Revisions(ctx, tenant, -1); len(rs) != 0 {
		t.Fatal(rs)
	}
	if one, _ := b.renderLog(ctx, tenant, 1); !strings.Contains(one, "omitted") {
		t.Fatal(one)
	}
}

func TestFilesAreReadInPythonsPathOrder(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a-b/x.md", "a/x.md", "a.md", "b.md"} {
		writeFile(t, dir, f, "---\ntype: Concept\n---\n")
	}
	cs, err := documents(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		got = append(got, c.Path)
	}
	// pathlib compares parts, so "a" sorts before "a-b" and "a.md".
	if want := []string{"a/x", "a-b/x", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}
