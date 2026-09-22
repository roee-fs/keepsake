package okf

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// NonFinite is a YAML .inf, -.inf or .nan. JSON cannot hold it, so it refuses to marshal.
type NonFinite string

const (
	PyInf    NonFinite = "inf"
	PyNegInf NonFinite = "-inf"
	PyNaN    NonFinite = "nan"
)

func (n NonFinite) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("okf: frontmatter holds %s, which JSON cannot store", string(n))
}

// pyStr is Python's str() over the value domain.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

func pyRepr(v any) string {
	switch v := v.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return PyReprString(v)
	case int:
		return strconv.Itoa(v)
	case json.Number:
		if isIntLiteral(string(v)) {
			return string(v)
		}
		f, _ := strconv.ParseFloat(string(v), 64)
		return pyFloatRepr(f)
	case NonFinite:
		return string(v)
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Map:
		parts := make([]string, 0, v.Len())
		for _, k := range v.keys {
			parts = append(parts, PyReprString(k)+": "+pyRepr(v.vals[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	panic(fmt.Sprintf("okf: %T is outside the frontmatter value domain", v))
}

func isIntLiteral(s string) bool {
	return !strings.ContainsAny(s, ".eE")
}

// pyFloatRepr is Python's repr(float): the shortest round-trip digits, in exponent form
// when the decimal exponent is below -4 or at least 16.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(s[strings.IndexByte(s, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return s
	}
	s = strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// PyReprString is Python's repr(str): single quotes unless only double quotes avoid escaping.
func PyReprString(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(q)
	return b.String()
}
