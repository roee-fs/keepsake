package okf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// Map is a JSON object that keeps its keys in insertion order, as a Python dict does.
// Its values are string, bool, nil, json.Number, int, NonFinite, []any or *Map.
type Map struct {
	keys []string
	vals map[string]any
}

func NewMap() *Map { return &Map{vals: map[string]any{}} }

// Set adds or replaces a value. An existing key keeps its position.
func (m *Map) Set(k string, v any) {
	if m.vals == nil {
		m.vals = map[string]any{}
	}
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

func (m *Map) Get(k string) (any, bool) {
	v, ok := m.vals[k]
	return v, ok
}

func (m *Map) Delete(k string) {
	if _, ok := m.vals[k]; !ok {
		return
	}
	delete(m.vals, k)
	m.keys = slices.DeleteFunc(m.keys, func(s string) bool { return s == k })
}

func (m *Map) Keys() []string { return slices.Clone(m.keys) }

func (m *Map) Len() int { return len(m.keys) }

func (m *Map) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(m.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON walks tokens so that key order survives; a map[string]any would lose it.
func (m *Map) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	t, err := d.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return fmt.Errorf("okf: frontmatter JSON is %v, not an object", t)
	}
	*m = Map{}
	return decodeObject(d, m)
}

func decodeObject(d *json.Decoder, m *Map) error {
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return err
		}
		v, err := decodeValue(d)
		if err != nil {
			return err
		}
		m.Set(t.(string), v)
	}
	_, err := d.Token()
	return err
}

func decodeValue(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := NewMap()
		return m, decodeObject(d, m)
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := d.Token()
		return a, err
	}
	return t, nil
}
