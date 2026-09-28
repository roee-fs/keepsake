package okf

import (
	"encoding/json"
	"os"
	"reflect"
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

var plainTimestamp = map[string]bool{"V_DATE": true, "V_DATETIME_OFFSET": true, "V_DATETIME_Z": true, "V_SINGLE_QUOTED_DATE": true}

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
			want := g.Exported
			if plainTimestamp[g.Name] {
				// Python quotes a timestamp, so typed readers see a string. OKF §5 values are timestamps.
				want = strings.Replace(strings.Replace(want, "v: '", "v: ", 1), "'\n---", "\n---", 1)
			}
			if got != want {
				t.Errorf("go:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestSerializeWritesThePromotedFieldsOverAFrontmatterCopy(t *testing.T) {
	// A row written before Validate refused these keys MUST still export a non-empty type.
	fm := NewMap()
	fm.Set("x", "1")
	fm.Set("type", "")
	fm.Set("title", "Other")
	got, _ := Serialize(Concept{Type: "Concept", Title: "T", Frontmatter: fm})
	if got != "---\ntype: Concept\ntitle: T\nx: '1'\n---\n" {
		t.Fatalf("%q", got)
	}
}

func TestATimestampRoundTripsUnquoted(t *testing.T) {
	doc := "---\ntype: '2026-01-01'\ngenerated: {by: human:a, at: 2026-06-20T22:53:05Z}\nstale_after: 2026-09-23\n---\n"
	c, err := Parse(doc, "p")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := Serialize(c)
	want := "---\ntype: '2026-01-01'\ngenerated: {by: human:a, at: 2026-06-20T22:53:05+00:00}\nstale_after: 2026-09-23\n---\n"
	if got != want {
		t.Fatalf("%q", got)
	}
	if again, _ := Parse(got, "p"); !reflect.DeepEqual(again, c) {
		t.Fatalf("%+v", again)
	}
	// An impossible date stays quoted, or the export would not import.
	fm := NewMap()
	fm.Set("v", "2026-02-30")
	if got, _ := Serialize(Concept{Type: "C", Frontmatter: fm}); !strings.Contains(got, "v: '2026-02-30'") {
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
