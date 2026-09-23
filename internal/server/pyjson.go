package server

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/roee-fs/keepsake/okf"
)

// pyJSON decodes s as CPython's json.loads does. On failure it returns the
// JSONDecodeError's msg and pos, which FastAPI puts in a json_invalid error.
func pyJSON(s []rune) (v any, msg string, pos int) {
	d := &pyDecoder{s: s}
	i := d.ws(0)
	v, end, err := d.value(i)
	if err != nil {
		return nil, err.msg, err.pos
	}
	if end = d.ws(end); end != len(s) {
		return nil, "Extra data", end
	}
	return v, "", 0
}

type pyDecoder struct{ s []rune }

type pyJSONError struct {
	msg string
	pos int
}

func (d *pyDecoder) ws(i int) int {
	for i < len(d.s) && (d.s[i] == ' ' || d.s[i] == '\t' || d.s[i] == '\n' || d.s[i] == '\r') {
		i++
	}
	return i
}

func (d *pyDecoder) at(i int) rune {
	if i < len(d.s) {
		return d.s[i]
	}
	return 0
}

func (d *pyDecoder) has(i int, word string) bool {
	return i+len(word) <= len(d.s) && string(d.s[i:i+len(word)]) == word
}

// value is the scanner's scan_once; its StopIteration is "Expecting value".
func (d *pyDecoder) value(i int) (any, int, *pyJSONError) {
	switch c := d.at(i); {
	case i >= len(d.s):
	case c == '"':
		return d.str(i + 1)
	case c == '{':
		return d.object(i + 1)
	case c == '[':
		return d.array(i + 1)
	case d.has(i, "null"):
		return nil, i + 4, nil
	case d.has(i, "true"):
		return true, i + 4, nil
	case d.has(i, "false"):
		return false, i + 5, nil
	case d.has(i, "NaN"):
		return okf.PyNaN, i + 3, nil
	case d.has(i, "Infinity"):
		return okf.PyInf, i + 8, nil
	case d.has(i, "-Infinity"):
		return okf.PyNegInf, i + 9, nil
	default:
		if end := d.number(i); end > i {
			text := string(d.s[i:end])
			// Python's float() of an overflowing literal is inf, as NaN is a float.
			if f, err := strconv.ParseFloat(text, 64); strings.ContainsAny(text, ".eE") && math.IsInf(f, 0) && err != nil {
				return map[bool]okf.NonFinite{true: okf.PyInf, false: okf.PyNegInf}[f > 0], end, nil
			}
			return json.Number(text), end, nil
		}
	}
	return nil, 0, &pyJSONError{"Expecting value", i}
}

// number matches -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)? and returns its end, or i.
func (d *pyDecoder) number(i int) int {
	digits := func(j int) int {
		for j < len(d.s) && d.s[j] >= '0' && d.s[j] <= '9' {
			j++
		}
		return j
	}
	j := i
	if d.at(j) == '-' {
		j++
	}
	switch c := d.at(j); {
	case c == '0':
		j++
	case c >= '1' && c <= '9':
		j = digits(j)
	default:
		return i
	}
	if d.at(j) == '.' && digits(j+1) > j+1 {
		j = digits(j + 1)
	}
	if c := d.at(j); c == 'e' || c == 'E' {
		k := j + 1
		if c := d.at(k); c == '+' || c == '-' {
			k++
		}
		if digits(k) > k {
			j = digits(k)
		}
	}
	return j
}

func (d *pyDecoder) object(i int) (any, int, *pyJSONError) {
	m := okf.NewMap()
	i = d.ws(i)
	if d.at(i) == '}' && i < len(d.s) {
		return m, i + 1, nil
	}
	for {
		if i >= len(d.s) || d.s[i] != '"' {
			return nil, 0, &pyJSONError{"Expecting property name enclosed in double quotes", i}
		}
		k, end, err := d.str(i + 1)
		if err != nil {
			return nil, 0, err
		}
		if i = d.ws(end); d.at(i) != ':' || i >= len(d.s) {
			return nil, 0, &pyJSONError{"Expecting ':' delimiter", i}
		}
		v, end, err := d.value(d.ws(i + 1))
		if err != nil {
			return nil, 0, err
		}
		m.Set(k.(string), v)
		i = d.ws(end)
		if i < len(d.s) && d.s[i] == '}' {
			return m, i + 1, nil
		}
		if i >= len(d.s) || d.s[i] != ',' {
			return nil, 0, &pyJSONError{"Expecting ',' delimiter", i}
		}
		comma := i
		if i = d.ws(i + 1); d.at(i) == '}' && i < len(d.s) {
			return nil, 0, &pyJSONError{"Illegal trailing comma before end of object", comma}
		}
	}
}

func (d *pyDecoder) array(i int) (any, int, *pyJSONError) {
	a := []any{}
	i = d.ws(i)
	if d.at(i) == ']' && i < len(d.s) {
		return a, i + 1, nil
	}
	for {
		v, end, err := d.value(i)
		if err != nil {
			return nil, 0, err
		}
		a = append(a, v)
		i = d.ws(end)
		if i < len(d.s) && d.s[i] == ']' {
			return a, i + 1, nil
		}
		if i >= len(d.s) || d.s[i] != ',' {
			return nil, 0, &pyJSONError{"Expecting ',' delimiter", i}
		}
		comma := i
		if i = d.ws(i + 1); d.at(i) == ']' && i < len(d.s) {
			return nil, 0, &pyJSONError{"Illegal trailing comma before end of array", comma}
		}
	}
}

// str is _json.c's scanstring: i is just past the opening quote.
func (d *pyDecoder) str(i int) (any, int, *pyJSONError) {
	begin := i - 1
	var out []rune
	for {
		if i >= len(d.s) {
			return nil, 0, &pyJSONError{"Unterminated string starting at", begin}
		}
		c := d.s[i]
		switch {
		case c == '"':
			return string(out), i + 1, nil
		case c < 0x20:
			return nil, 0, &pyJSONError{"Invalid control character at", i}
		case c != '\\':
			out = append(out, c)
			i++
			continue
		}
		if i+1 >= len(d.s) {
			return nil, 0, &pyJSONError{"Unterminated string starting at", begin}
		}
		esc := d.s[i+1]
		if esc != 'u' {
			r, ok := map[rune]rune{'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t'}[esc]
			if !ok {
				return nil, 0, &pyJSONError{"Invalid \\escape", i}
			}
			out = append(out, r)
			i += 2
			continue
		}
		u, ok := d.hex4(i + 2)
		if !ok {
			return nil, 0, &pyJSONError{"Invalid \\uXXXX escape", i + 1}
		}
		i += 6
		if u >= 0xd800 && u <= 0xdbff && d.has(i, "\\u") {
			if u2, ok := d.hex4(i + 2); !ok {
				return nil, 0, &pyJSONError{"Invalid \\uXXXX escape", i + 1}
			} else if u2 >= 0xdc00 && u2 <= 0xdfff {
				u = 0x10000 + (u-0xd800)<<10 | (u2 - 0xdc00)
				i += 6
			}
		}
		// A lone surrogate, which Python keeps, has no UTF-8 form.
		if !utf8.ValidRune(u) {
			u = utf8.RuneError
		}
		out = append(out, u)
	}
}

func (d *pyDecoder) hex4(i int) (rune, bool) {
	if i+4 > len(d.s) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(d.s[i:i+4]), 16, 32)
	return rune(n), err == nil
}
