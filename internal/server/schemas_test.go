package server

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestAdvertisedToolsAreByteIdentical(t *testing.T) {
	want, _ := os.ReadFile("testdata/tools.json")
	got, _ := json.MarshalIndent(toolDefinitions(), "", "  ")
	if !jsonEqualOrdered(got, want) {
		t.Fatalf("tool definitions drifted:\n%s", got)
	}
}

// jsonEqualOrdered compares two tool listings. Within each tool's fields, object key
// order matters; the order of a tool's own top-level fields is go-sdk's struct order.
func jsonEqualOrdered(got, want []byte) bool {
	var g, w []map[string]json.RawMessage
	if json.Unmarshal(got, &g) != nil || json.Unmarshal(want, &w) != nil || len(g) != len(w) {
		return false
	}
	for i := range g {
		if !slices.Equal(slices.Sorted(maps.Keys(g[i])), slices.Sorted(maps.Keys(w[i]))) {
			return false
		}
		for k := range g[i] {
			if !reflect.DeepEqual(tokens(g[i][k]), tokens(w[i][k])) {
				return false
			}
		}
	}
	return true
}

func tokens(b []byte) []any {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out []any
	for {
		tok, err := d.Token()
		if err != nil {
			return out
		}
		out = append(out, tok)
	}
}

func TestJSONEqualOrderedSeesKeyOrder(t *testing.T) {
	a := []byte(`[{"s": {"a": 1, "b": 2}}]`)
	b := []byte(`[{"s": {"b": 2, "a": 1}}]`)
	if jsonEqualOrdered(a, b) {
		t.Fatal("reordered schema keys compared equal")
	}
	if !jsonEqualOrdered(a, []byte(`[{"s":{"a":1,"b":2}}]`)) {
		t.Fatal("whitespace alone made listings unequal")
	}
}
