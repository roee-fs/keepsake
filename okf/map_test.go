package okf

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestMapKeepsInsertionOrderAndSetKeepsPosition(t *testing.T) {
	m := NewMap()
	m.Set("zz", 1)
	m.Set("a", 2)
	m.Set("zz", 3)
	if got := m.Keys(); !slices.Equal(got, []string{"zz", "a"}) {
		t.Fatalf("keys %q", got)
	}
	b, _ := json.Marshal(m)
	if string(b) != `{"zz":3,"a":2}` {
		t.Fatalf("json %s", b)
	}
}

func TestMapUnmarshalKeepsDocumentOrderAndNumberText(t *testing.T) {
	var m Map
	if err := json.Unmarshal([]byte(`{"b": 1.50, "a": {"y": [1, null], "x": true}}`), &m); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Keys(), []string{"b", "a"}) {
		t.Fatalf("keys %q", m.Keys())
	}
	if v, _ := m.Get("b"); v != json.Number("1.50") {
		t.Fatalf("b = %#v", v)
	}
	inner, _ := m.Get("a")
	if !slices.Equal(inner.(*Map).Keys(), []string{"y", "x"}) {
		t.Fatal("nested order lost")
	}
}

func TestMapUnmarshalDuplicateKeyKeepsFirstPositionAndLastValue(t *testing.T) {
	var m Map
	if err := json.Unmarshal([]byte(`{"a": 1, "b": 2, "a": 3}`), &m); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(&m)
	if string(b) != `{"a":3,"b":2}` {
		t.Fatalf("json %s", b)
	}
}

func TestMapDelete(t *testing.T) {
	m := NewMap()
	m.Set("a", 1)
	m.Set("b", 2)
	m.Delete("a")
	m.Delete("missing")
	if _, ok := m.Get("a"); ok || m.Len() != 1 || !slices.Equal(m.Keys(), []string{"b"}) {
		t.Fatalf("keys %q", m.Keys())
	}
}

func TestMapRefusesToMarshalNaNOrInfinity(t *testing.T) {
	m := NewMap()
	m.Set("v", []any{PyNaN})
	if _, err := json.Marshal(m); err == nil {
		t.Fatal("NaN marshalled")
	}
}
