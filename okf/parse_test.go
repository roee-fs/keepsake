package okf

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"strings"
	"testing"
)

type parseGolden struct {
	Name            string
	InputB64        string `json:"input_b64"`
	OK              bool
	ErrorContains   string `json:"error_contains"`
	Type            string
	Title           string
	Description     string
	Body            string
	FrontmatterJSON string `json:"frontmatter_json"`
	Links           []string
}

func loadParseGoldens(t *testing.T) []parseGolden {
	raw, err := os.ReadFile("testdata/goldens/parse.json")
	if err != nil {
		t.Fatal(err)
	}
	var gs []parseGolden
	if err := json.Unmarshal(raw, &gs); err != nil {
		t.Fatal(err)
	}
	if len(gs) < 90 {
		t.Fatalf("only %d parse goldens", len(gs))
	}
	return gs
}

// isDeliberateRefusal names the cases Python accepts and Go refuses on purpose.
func isDeliberateRefusal(name string) bool {
	return name == "REFUSE_BINARY" || name == "REFUSE_SET" || name == "REFUSE_LOCAL_TAG"
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestParseMatchesPython(t *testing.T) {
	for _, g := range loadParseGoldens(t) {
		t.Run(g.Name, func(t *testing.T) {
			in, _ := base64.StdEncoding.DecodeString(g.InputB64)
			c, err := Parse(string(in), "corpus/"+g.Name)
			if !g.OK {
				if err == nil {
					t.Fatalf("python refused (%s), go accepted", g.ErrorContains)
				}
				return
			}
			if isDeliberateRefusal(g.Name) {
				if err == nil || !strings.Contains(err.Error(), "unsupported YAML tag !") {
					t.Fatalf("MUST refuse an exotic tag, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			fm, err := json.Marshal(finite(c.Frontmatter))
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqualOrdered(fm, []byte(pyNonFinite.Replace(g.FrontmatterJSON))) {
				t.Errorf("frontmatter\n go: %s\n py: %s", fm, g.FrontmatterJSON)
			}
			if c.Type != g.Type || c.Title != g.Title || c.Description != g.Description || c.Body != g.Body {
				t.Errorf("promoted fields differ: %+v", c)
			}
			if !slices.Equal(orEmpty(c.Links), g.Links) {
				t.Errorf("links %q, python %q", c.Links, g.Links)
			}
		})
	}
}

// pyNonFinite quotes the bare tokens json.dumps writes for non-finite floats; finite does the same on the Go side.
var pyNonFinite = strings.NewReplacer(": -Infinity", `: "-inf"`, ": Infinity", `: "inf"`, ": NaN", `: "nan"`)

func finite(v any) any {
	switch v := v.(type) {
	case NonFinite:
		return string(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = finite(e)
		}
		return out
	case *Map:
		out := NewMap()
		for _, k := range v.Keys() {
			e, _ := v.Get(k)
			out.Set(k, finite(e))
		}
		return out
	}
	return v
}

// jsonEqualOrdered compares key order and values, and numbers by value rather than text.
func jsonEqualOrdered(a, b []byte) bool {
	var ma, mb Map
	if json.Unmarshal(a, &ma) != nil || json.Unmarshal(b, &mb) != nil {
		return false
	}
	return valueEqual(&ma, &mb)
}

func valueEqual(a, b any) bool {
	switch a := a.(type) {
	case *Map:
		b, ok := b.(*Map)
		if !ok || !slices.Equal(a.Keys(), b.Keys()) {
			return false
		}
		for _, k := range a.Keys() {
			x, _ := a.Get(k)
			y, _ := b.Get(k)
			if !valueEqual(x, y) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		return ok && slices.EqualFunc(a, b, valueEqual)
	case json.Number:
		b, ok := b.(json.Number)
		if !ok {
			return false
		}
		x, _, errX := big.ParseFloat(string(a), 10, 256, big.ToNearestEven)
		y, _, errY := big.ParseFloat(string(b), 10, 256, big.ToNearestEven)
		return errX == nil && errY == nil && x.Cmp(y) == 0
	}
	return a == b
}

const doc = `---
type: Concept
title: Five Layer Architecture
description: How the layers stack.
tags: [architecture, tooling]
custom_vendor_field: keep-me
---
The spec sits beneath the convention.
`

func TestParsePromotesKnownFieldsAndKeepsUnknown(t *testing.T) {
	c, err := Parse(doc, "architecture/layers")
	if err != nil {
		t.Fatal(err)
	}
	if c.Type != "Concept" || c.Title != "Five Layer Architecture" || c.Version != 1 {
		t.Fatalf("%+v", c)
	}
	if v, _ := c.Frontmatter.Get("custom_vendor_field"); v != "keep-me" {
		t.Fatalf("custom_vendor_field = %#v", v)
	}
	if _, ok := c.Frontmatter.Get("type"); ok {
		t.Fatal("type left in frontmatter")
	}
}

func TestNonStringKnownFieldIsCoerced(t *testing.T) {
	c, err := Parse("---\ntype: 42\n---\nb\n", "p")
	if err != nil || c.Type != "42" {
		t.Fatalf("%q, %v", c.Type, err)
	}
}

func TestAKnownFieldLeftEmptyIsEmptyNotTheWordNone(t *testing.T) {
	c, err := Parse("---\ntype: Concept\ntitle:\ndescription:\n---\nb\n", "p")
	if err != nil || c.Title != "" || c.Description != "" {
		t.Fatalf("%+v, %v", c, err)
	}
}

func TestEmptyFrontmatterParsesToAnEmptyMapping(t *testing.T) {
	c, err := Parse("---\n---\nThe spec sits beneath the convention.\n", "p")
	if err != nil {
		t.Fatal(err)
	}
	if c.Frontmatter.Len() != 0 || c.Body != "The spec sits beneath the convention.\n" {
		t.Fatalf("%+v", c)
	}
}

func TestCRLFDocumentNormalisesToLF(t *testing.T) {
	c, err := Parse("---\r\ntype: Concept\r\ntitle: T\r\n---\r\nBody.\r\n", "p")
	if err != nil || c.Title != "T" || c.Body != "Body.\n" {
		t.Fatalf("%+v, %v", c, err)
	}
}

func TestInvalidUTF8IsRefusedByName(t *testing.T) {
	_, err := Parse("---\ntype: Concept\nv: \xff\n---\nb\n", "p")
	var ye YAMLError
	if !errors.As(err, &ye) || ye.Msg != "not valid UTF-8" {
		t.Fatalf("%v", err)
	}
}

func TestExoticTagIsRefusedByName(t *testing.T) {
	_, err := Parse("---\ntype: Concept\nv: !!omap [a: 1]\n---\nb\n", "p")
	var ye YAMLError
	if !errors.As(err, &ye) || ye.Msg != "unsupported YAML tag !!omap" {
		t.Fatalf("%v", err)
	}
}

func TestNonMappingFrontmatterIsRefused(t *testing.T) {
	for _, root := range []string{"- a", "hello", "1"} {
		if _, err := Parse("---\n"+root+"\n---\nb\n", "p"); err == nil {
			t.Errorf("%s: accepted", root)
		}
	}
}

// Python loads with `load(...) or {}`, so a falsy root reads as no frontmatter.
func TestFalsyFrontmatterIsAnEmptyMapping(t *testing.T) {
	for _, root := range []string{"~", "null", "false", "0", "0.0", "''", "[]", "{}"} {
		c, err := Parse("---\n"+root+"\n---\nb\n", "p")
		if err != nil || c.Frontmatter.Len() != 0 {
			t.Errorf("%s: %v", root, err)
		}
	}
}

// Each want is what ruamel and json.dumps(default=str) produce for `v: <yaml>`, and str() for `type: <yaml>`.
func TestScalarsResolveAsRuamelDoes(t *testing.T) {
	for _, c := range []struct{ yaml, json, str string }{
		{"007", `7`, "7"},
		{"+1", `1`, "1"},
		{"0b101", `5`, "5"},
		{"0x_1", `1`, "1"},
		{"1__0", `10`, "10"},
		{"_1", `"_1"`, "_1"},
		{"0o", `"0o"`, "0o"},
		{"1.", `1.0`, "1.0"},
		{".5e3", `".5e3"`, ".5e3"},
		{".5e+3", `500.0`, "500.0"},
		{"1e15", `1000000000000000.0`, "1000000000000000.0"},
		{"1e16", `1e+16`, "1e+16"},
		{"0.00001", `1e-05`, "1e-05"},
		{"-0.0", `-0.0`, "-0.0"},
		{"1e400", `"inf"`, "inf"},
		{"-.nan", `"-.nan"`, "-.nan"},
		{"tRUE", `"tRUE"`, "tRUE"},
		{"false", `false`, "False"},
		{"NULL", `null`, ""},
		{"2026-1-1", `"2026-1-1"`, "2026-1-1"},
		{"2026-01-01t10:00:00", `"2026-01-01T10:00:00"`, "2026-01-01T10:00:00"},
		{"2026-01-01    10:00:00.5", `"2026-01-01 10:00:00.500000"`, "2026-01-01 10:00:00.500000"},
		{"2026-01-01 10:00:00.1234565", `"2026-01-01 10:00:00.123457"`, "2026-01-01 10:00:00.123457"},
		{"2026-01-01 23:59:59.9999996", `"2026-01-02 00:00:00"`, "2026-01-02 00:00:00"},
		{"2026-01-01 10:00:00 Z", `"2026-01-01 10:00:00+00:00"`, "2026-01-01 10:00:00+00:00"},
		{"2026-01-01 1:00:00+5:30", `"2026-01-01 01:00:00+05:30"`, "2026-01-01 01:00:00+05:30"},
		{"2026-01-01T10:00:00-00:30", `"2026-01-01T10:00:00-00:30"`, "2026-01-01T10:00:00-00:30"},
		{"2026-01-01T10:00:00+0530", `"2026-01-01T10:00:00+0530"`, "2026-01-01T10:00:00+0530"},
		{"&a true", `1`, "1"},
		{"!!int \"12\"", `12`, "12"},
		{"!!float 1_0", `10.0`, "10.0"},
		{"!!str 12", `"12"`, "12"},
		{"! 12", `12`, "12"},
		{"[1, 'a', true, null, 1.5, {k: [x]}]", `[1, "a", true, null, 1.5, {"k": ["x"]}]`, "[1, 'a', True, None, 1.5, {'k': ['x']}]"},
		{`["it's", 'say "hi"', "both ' and \"", "é\x01\x7f` + "\u00a0" + `\U0001F389"]`, `["it's", "say \"hi\"", "both ' and \"", "é\u0001\u007f` + "\u00a0" + `🎉"]`, `["it's", 'say "hi"', 'both \' and "', 'é\x01\x7f\xa0🎉']`},
	} {
		t.Run(c.yaml, func(t *testing.T) {
			v, err := Parse("---\nv: "+c.yaml+"\n---\n", "p")
			if err != nil {
				t.Fatal(err)
			}
			fm, err := json.Marshal(finite(v.Frontmatter))
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"v": ` + c.json + `}`; !jsonEqualOrdered(fm, []byte(want)) {
				t.Errorf("json %s, python %s", fm, want)
			}
			p, err := Parse("---\ntype: "+c.yaml+"\n---\n", "p")
			if err != nil {
				t.Fatal(err)
			}
			if p.Type != c.str {
				t.Errorf("str %q, python %q", p.Type, c.str)
			}
		})
	}
}

func TestInvalidScalarsAreRefusedAsRuamelDoes(t *testing.T) {
	for _, y := range []string{"-_", "2026-13-01", "2026-02-30", "0000-01-01", "2026-01-01 24:00:00", "2026-01-01 10:60:00",
		"2026-01-01 10:00:60", "2026-01-01 10:00:00+24", "!!int 1.5", "`x`", "{a: 1, a: 2}", "{<<: {a: 1}, <<: {b: 1}}", "{<<: 1}"} {
		if _, err := Parse("---\nv: "+y+"\n---\n", "p"); err == nil {
			t.Errorf("%s: accepted", y)
		}
	}
}

func TestMergeKeysFollowRuamelOrder(t *testing.T) {
	c, err := Parse("---\nb1: &b1 {a: 1, c: 3}\nb2: &b2 {a: 2, d: 4}\nv:\n  x: 0\n  <<: [*b1, *b2]\n  c: 9\nw: {<<: {a: 1}, a: 2}\n---\n", "p")
	if err != nil {
		t.Fatal(err)
	}
	fm, _ := json.Marshal(c.Frontmatter)
	want := `{"b1": {"a": 1, "c": 3}, "b2": {"a": 2, "d": 4}, "v": {"x": 0, "c": 9, "a": 1, "d": 4}, "w": {"a": 2}}`
	if !jsonEqualOrdered(fm, []byte(want)) {
		t.Fatalf("%s", fm)
	}
}

func TestAliasBombIsRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString("---\na0: &a0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i < 10; i++ {
		b.WriteString("a" + string(rune('0'+i)) + ": &a" + string(rune('0'+i)) + " [")
		for j := range 10 {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString("*a" + string(rune('0'+i-1)))
		}
		b.WriteString("]\n")
	}
	b.WriteString("---\n")
	if _, err := Parse(b.String(), "p"); err == nil {
		t.Fatal("a billion-laughs document was expanded")
	}
}

func TestRecursiveStructuresAreRefusedNotOverflowed(t *testing.T) {
	for name, fm := range map[string]string{
		"self-merge":    "v: &v\n  <<: *v\n",
		"sequence":      "v: &a [1, *a]\n",
		"mapping":       "v: &a {k: *a}\n",
		"deep nesting":  "v: " + strings.Repeat("[", 5000) + strings.Repeat("]", 5000) + "\n",
		"deep mappings": "v: " + strings.Repeat("{a: ", 5000) + "1" + strings.Repeat("}", 5000) + "\n",
	} {
		if _, err := Parse("---\n"+fm+"---\n", "p"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestASecondDocumentInTheFrontmatterIsRefused(t *testing.T) {
	if _, err := Parse("---\na: 1\n--- \nb: 2\n---\nbody\n", "p"); err == nil {
		t.Fatal("the second document was dropped silently")
	}
}

// ruamel reads YAML 1.2, where a flow plain scalar may start with ':' and contain '?'. libyaml's
// YAML 1.1 scanner refuses both, so an exported bundle would not re-import.
func TestFlowPlainScalarsWithIndicatorsRoundTrip(t *testing.T) {
	fm := NewMap()
	fm.Set("l", []any{":x", json.Number("1")})
	if got, _ := Serialize(Concept{Type: "C", Frontmatter: fm}); got != "---\ntype: C\nl: [:x, 1]\n---\n" {
		t.Fatalf("the emitter no longer writes this class plain: %q", got)
	}
	for _, v := range []string{":x", "::x", ":?", ":#", ":-", ":x:a", `:"`, ":'a", "a?", "a?x", "a ?", "$?", "-?", "x?:y", `a?"`} {
		shapes := []string{
			`{"l": [%s, 1]}`,
			`{"n": [[%s]], "o": [{"k": %[1]s, "j": [1]}]}`,
			`{"m": {"k": %s}}`,
		}
		// Python's own export of a flow mapping key led by ':' does not re-import, so it is no contract.
		if v[0] != ':' {
			shapes = append(shapes, `{"m": {%s: 1}}`)
		}
		q, _ := json.Marshal(v)
		for _, shape := range shapes {
			want := []byte(fmt.Sprintf(shape, q))
			var in Map
			if err := json.Unmarshal(want, &in); err != nil {
				t.Fatal(err)
			}
			text, err := Serialize(Concept{Type: "C", Frontmatter: &in})
			if err != nil {
				t.Fatal(err)
			}
			c, err := Parse(text, "p")
			if err != nil {
				t.Errorf("%q: %v", text, err)
				continue
			}
			if got, _ := json.Marshal(c.Frontmatter); !jsonEqualOrdered(got, want) {
				t.Errorf("%q: got %s", text, got)
			}
		}
	}
}

func TestAnEmptyFlowKeyIsNull(t *testing.T) {
	c, err := Parse("---\nm: {:\": :\"}\n---\n", "p")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(c.Frontmatter); string(got) != `{"m":{"null":": :"}}` {
		t.Fatalf("%s", got)
	}
}
