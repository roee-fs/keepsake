package okf

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExtractLinksMatchesPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/goldens/links.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Body, Path string
		Links      []string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 40 {
		t.Fatalf("only %d link goldens", len(cases))
	}
	for _, c := range cases {
		got := ExtractLinks(c.Body, c.Path)
		if got == nil {
			got = []string{}
		}
		if !slices.Equal(got, c.Links) {
			t.Errorf("ExtractLinks(%q, %q) = %q, python says %q", c.Body, c.Path, got, c.Links)
		}
	}
}

func TestExtractLinksUnicodeSpace(t *testing.T) {
	// Python's \s is Unicode-aware; RE2's is ASCII. A no-break space is whitespace to Python.
	got := ExtractLinks("[a](\u00a0b.md)", "p/q")
	if !slices.Equal(got, []string{"p/b"}) {
		t.Fatalf("got %q", got)
	}
}

func TestExtractLinksIsLinearInTheBody(t *testing.T) {
	// A regex retried at every '[' took seconds on 32 KiB of these, and hours at the request limit.
	for _, body := range []string{strings.Repeat("[", 1<<22), strings.Repeat("[x](", 1<<20)} {
		start := time.Now()
		ExtractLinks(body, "a/b")
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("ExtractLinks on %d bytes took %v", len(body), d)
		}
	}
}

func TestExtractLinksSkipsImagesButNotALaterLink(t *testing.T) {
	// Python's lookbehind fails at the `[` after `!` and retries at the next `[`.
	got := ExtractLinks("![x [y](z.md)", "")
	if !slices.Equal(got, []string{"z"}) {
		t.Fatalf("got %q", got)
	}
}
