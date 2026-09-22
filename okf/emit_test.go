package okf

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

type exportGolden struct {
	Name        string
	Path        string
	JSONBText   string `json:"jsonb_text"`
	Type        string
	Title       string
	Description string
	Body        string
	Exported    string
}

func loadExportGoldens(t *testing.T) []exportGolden {
	raw, err := os.ReadFile("testdata/goldens/export.json")
	if err != nil {
		t.Fatal(err)
	}
	var gs []exportGolden
	if err := json.Unmarshal(raw, &gs); err != nil {
		t.Fatal(err)
	}
	if len(gs) < 81 {
		t.Fatalf("only %d export goldens", len(gs))
	}
	return gs
}

func TestSerializeMatchesPythonExport(t *testing.T) {
	for _, g := range loadExportGoldens(t) {
		t.Run(g.Name, func(t *testing.T) {
			var fm Map
			if err := json.Unmarshal([]byte(g.JSONBText), &fm); err != nil {
				t.Fatal(err)
			}
			got, err := Serialize(Concept{Path: g.Path, Type: g.Type, Title: g.Title, Description: g.Description, Body: g.Body, Frontmatter: &fm})
			if err != nil {
				t.Fatal(err)
			}
			if got != g.Exported {
				t.Errorf("go:\n%s\npython:\n%s", got, g.Exported)
			}
		})
	}
}

func TestSerializeLetsFrontmatterOverwriteAPromotedFieldInPlace(t *testing.T) {
	// Python meta.update keeps `type` first and takes the frontmatter's value.
	fm := NewMap()
	fm.Set("x", "1")
	fm.Set("type", "Other")
	got, _ := Serialize(Concept{Type: "Concept", Frontmatter: fm})
	if got != "---\ntype: Other\nx: '1'\n---\n" {
		t.Fatalf("%q", got)
	}
}

func TestSerializeRefusesNonFiniteFloats(t *testing.T) {
	for _, v := range []NonFinite{PyInf, PyNegInf, PyNaN} {
		fm := NewMap()
		fm.Set("n", []any{v})
		if _, err := Serialize(Concept{Type: "Concept", Frontmatter: fm}); err == nil {
			t.Errorf("%s serialized", v)
		}
	}
}

func TestEveryExportParsesBackToTheSameValues(t *testing.T) {
	for _, g := range loadExportGoldens(t) {
		c, err := Parse(g.Exported, g.Path)
		if err != nil {
			t.Fatalf("%s: %v", g.Name, err)
		}
		fm, _ := json.Marshal(c.Frontmatter)
		if !jsonEqualOrdered(fm, []byte(g.JSONBText)) {
			t.Errorf("%s: exported bundle re-imports as %s, stored %s", g.Name, fm, g.JSONBText)
		}
	}
}

func TestRoundTripIsByteIdentical(t *testing.T) {
	roundTrip(t, doc, doc)
}

func TestRoundTripOfScalarOnlyFrontmatterStaysBlockStyle(t *testing.T) {
	const minimal = "---\ntype: Concept\ntitle: Five Layer Architecture\n---\nThe spec sits beneath the convention.\n"
	roundTrip(t, minimal, minimal)
}

func TestCRLFDocumentRoundTripsAsLF(t *testing.T) {
	roundTrip(t, "---\r\ntype: Concept\r\ntitle: T\r\n---\r\nBody.\r\n", "---\ntype: Concept\ntitle: T\n---\nBody.\n")
}

func roundTrip(t *testing.T, in, want string) {
	t.Helper()
	c, err := Parse(in, "architecture/layers")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Serialize(c)
	if err != nil || got != want {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestConcurrentRoundTripsDoNotShareEmitterState(t *testing.T) {
	c, _ := Parse(doc, "architecture/layers")
	want, _ := Serialize(c)
	var wg sync.WaitGroup
	jobs := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			for range jobs {
				c, err := Parse(doc, "a/b")
				if err != nil {
					t.Error(err)
					continue
				}
				if got, err := Serialize(c); err != nil || got != want {
					t.Errorf("%q, %v", got, err)
				}
			}
		})
	}
	for range 400 {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()
}

func TestPlainFrontmatterEmitsLeafListsInFlowStyle(t *testing.T) {
	fm := NewMap()
	fm.Set("tags", []any{"architecture", "tooling"})
	fm.Set("custom_vendor_field", "keep-me")
	got, err := Serialize(Concept{
		Path:        "architecture/layers",
		Type:        "Concept",
		Title:       "Five Layer Architecture",
		Description: "How the layers stack.",
		Body:        "The spec sits beneath the convention.\n",
		Frontmatter: fm,
	})
	if err != nil || got != doc {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestSerializeWritesAnOverflowingFloatAsInfinity(t *testing.T) {
	var fm Map
	big := strings.Repeat("0", 400)
	if err := json.Unmarshal([]byte(`{"n": [1`+big+`.5, -1`+big+`.5, 1e-400]}`), &fm); err != nil {
		t.Fatal(err)
	}
	got, err := Serialize(Concept{Type: "C", Frontmatter: &fm})
	if err != nil || got != "---\ntype: C\nn: [.inf, -.inf, 0.0]\n---\n" {
		t.Fatalf("%q, %v", got, err)
	}
}
