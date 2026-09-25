package okf

import (
	"slices"
	"strings"
	"testing"
)

func TestMissingTypeIsAnError(t *testing.T) {
	if got := Validate(Concept{Path: "a/b", Type: ""}); !slices.Equal(got, []string{"type is required"}) {
		t.Fatalf("got %q", got)
	}
}

func TestPathMustNotBeAbsoluteOrTraverse(t *testing.T) {
	if errs := Validate(Concept{Path: "/a/b", Type: "Concept"}); !strings.Contains(errs[0], "path must be relative") {
		t.Fatalf("got %q", errs)
	}
	if errs := Validate(Concept{Path: "a/../../b", Type: "Concept"}); !strings.Contains(errs[0], "path must not traverse") {
		t.Fatalf("got %q", errs)
	}
}

func TestEmptyPathIsAnError(t *testing.T) {
	if got := Validate(Concept{Path: "  ", Type: "Concept"}); !slices.Equal(got, []string{"path is required"}) {
		t.Fatalf("got %q", got)
	}
}

func TestEveryBrokenRuleIsReported(t *testing.T) {
	// Rules are independent so one call shows the caller everything to fix.
	want := []string{"type is required", "path must be relative, not absolute"}
	if got := Validate(Concept{Path: "/a/b", Type: ""}); !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestValidConceptHasNoErrors(t *testing.T) {
	if got := Validate(Concept{Path: "a/b", Type: "Concept"}); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestTheGeneratedBundleNamesAreReservedAtTheRoot(t *testing.T) {
	// A bundle writes index.md and log.md itself, so a concept holding one of those
	// paths would be exported over and skipped on the way back in.
	if errs := Validate(Concept{Path: "index", Type: "Concept"}); !strings.Contains(errs[0], "reserved") {
		t.Fatalf("got %q", errs)
	}
	if errs := Validate(Concept{Path: "log", Type: "Concept"}); !strings.Contains(errs[0], "reserved") {
		t.Fatalf("got %q", errs)
	}
	// Only at the root: deeper in the tree the name is ordinary knowledge.
	if got := Validate(Concept{Path: "architecture/index", Type: "Concept"}); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestAMultibytePathIsMeasuredInBytesNotCharacters(t *testing.T) {
	// The btree limit these caps stand in for is a byte limit. Measured in
	// characters, 1024 CJK characters is 3072 bytes and blows the 2704-byte index
	// entry the cap exists to keep it under -- admitting exactly what it stops.
	path := strings.Repeat("漢", MaxPath) // 1024 characters, 3072 bytes
	errs := Validate(Concept{Path: path, Type: "Concept"})
	if len(errs) != 1 {
		t.Fatalf("got %q", errs)
	}
	want := "path is too long: 3072 bytes, at most 1024"
	if errs[0] != want {
		t.Fatalf("got %q, want %q", errs[0], want)
	}

	// And the limit is still reachable in full when the path is ASCII.
	if got := Validate(Concept{Path: strings.Repeat("a", MaxPath), Type: "Concept"}); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestAMultibyteBodyIsMeasuredInBytesToo(t *testing.T) {
	// Same defect, same reason: the tsvector ceiling is counted in bytes.
	body := strings.Repeat("漢", MaxBody)
	errs := Validate(Concept{Path: "a/b", Type: "Concept", Body: body})
	if len(errs) != 1 {
		t.Fatalf("got %q", errs)
	}
	if !strings.HasPrefix(errs[0], "body is too long:") || !strings.HasSuffix(errs[0], "bytes, at most 262144") {
		t.Fatalf("got %q", errs[0])
	}
}

func TestSizesAreBytesNotRunes(t *testing.T) {
	c := Concept{Path: strings.Repeat("日", 342), Type: "Concept"} // 1026 bytes, 342 runes
	if errs := Validate(c); !slices.Contains(errs, "path is too long: 1026 bytes, at most 1024") {
		t.Fatalf("got %q", errs)
	}
}

func TestANULAnywhereInTheFrontmatterIsAnError(t *testing.T) {
	nested := NewMap()
	nested.Set("k", []any{"ok", "a\x00b"})
	key := NewMap()
	key.Set("a\x00", "v")
	for _, fm := range []*Map{nested, key} {
		if got := Validate(Concept{Path: "a/b", Type: "Concept", Frontmatter: fm}); !slices.Equal(got, []string{"frontmatter must not contain a NUL byte"}) {
			t.Fatalf("got %q", got)
		}
	}
}

func TestErrorsComeInPythonOrder(t *testing.T) {
	errs := Validate(Concept{Path: "/../index", Type: " "})
	want := []string{"type is required", "path must be relative, not absolute", "path must not traverse upward"}
	if !slices.Equal(errs[:3], want) {
		t.Fatalf("got %q", errs)
	}
}

func TestStorableRefusesNonFiniteFrontmatter(t *testing.T) {
	c, err := Parse("---\ntype: Note\nscore: .nan\n---\nbody\n", "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := Storable(c); err == nil || err.Error() != "frontmatter holds NaN or Infinity, which JSON cannot store" {
		t.Fatalf("Storable = %v", err)
	}
}

func TestStorableJoinsEveryValidationError(t *testing.T) {
	err := Storable(Concept{Path: "../x"})
	if err == nil || err.Error() != "type is required; path must not traverse upward" {
		t.Fatalf("Storable = %v", err)
	}
}

func TestStorableAcceptsAValidConcept(t *testing.T) {
	if err := Storable(Concept{Path: "a/b", Type: "Note"}); err != nil {
		t.Fatal(err)
	}
}
