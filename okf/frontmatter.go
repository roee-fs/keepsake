package okf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/roee-fs/keepsake/okf/internal/yaml"
)

// YAMLError is frontmatter that cannot be read. Callers prefix it with the file name.
type YAMLError struct{ Msg string }

func (e YAMLError) Error() string { return e.Msg }

// The closing fence is anchored per line so that empty frontmatter matches too.
var fence = regexp.MustCompile(`(?sm)\A---\n(.*?)^---\n(.*)\z`)

// split returns the frontmatter mapping and the body. A document without a fence has no frontmatter.
func split(text string) (*Map, string, error) {
	if !utf8.ValidString(text) {
		return nil, "", YAMLError{"not valid UTF-8"}
	}
	// LF is OKF's canonical line ending; CRLF input does not round-trip.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	m := fence.FindStringSubmatch(text)
	if m == nil {
		return NewMap(), text, nil
	}
	var doc yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(m[1]))
	switch err := dec.Decode(&doc); {
	case errors.Is(err, io.EOF):
		return NewMap(), m[2], nil
	case err != nil:
		return nil, "", YAMLError{err.Error()}
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, "", YAMLError{"expected a single document in the frontmatter"}
	}
	if len(doc.Content) == 0 {
		return NewMap(), m[2], nil
	}
	c := converter{budget: maxNodes, active: map[*yaml.Node]bool{}}
	root, err := c.value(doc.Content[0])
	if err != nil {
		return nil, "", err
	}
	if fm, ok := root.(*Map); ok {
		return fm, m[2], nil
	}
	// Python runs dict(load(...) or {}), so a falsy root is an empty mapping.
	switch root {
	case nil, false, "", json.Number("0"), json.Number("0.0"), json.Number("-0.0"):
		return NewMap(), m[2], nil
	}
	if a, ok := root.([]any); ok && len(a) == 0 {
		return NewMap(), m[2], nil
	}
	return nil, "", YAMLError{"frontmatter is not a mapping"}
}

const (
	// maxNodes bounds alias expansion, which grows exponentially in a billion-laughs document.
	maxNodes = 1_000_000
	// maxDepth is near Python's recursion limit and keeps deep nesting off the goroutine stack limit.
	maxDepth = 1000
)

// converter turns yaml.v3 nodes into the value domain with ruamel's YAML 1.2 round-trip semantics.
type converter struct {
	budget, depth int
	// active holds the collections being converted, so an alias back into one is caught.
	active map[*yaml.Node]bool
}

func (c *converter) value(n *yaml.Node) (any, error) {
	if c.budget--; c.budget < 0 {
		return nil, YAMLError{"frontmatter expands to too many values"}
	}
	if c.depth++; c.depth > maxDepth {
		return nil, YAMLError{"frontmatter nests too deeply"}
	}
	defer func() { c.depth-- }()
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if c.active[n] {
		return nil, YAMLError{"frontmatter contains itself through an alias"}
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		c.active[n] = true
		defer delete(c.active, n)
	}
	tag := ""
	if n.Style&yaml.TaggedStyle != 0 && n.Tag != "!" {
		tag = n.Tag
	}
	switch n.Kind {
	case yaml.MappingNode:
		if tag != "" && tag != "!!map" {
			return nil, unsupportedTag(tag)
		}
		return c.mapping(n)
	case yaml.SequenceNode:
		if tag != "" && tag != "!!seq" {
			return nil, unsupportedTag(tag)
		}
		out := make([]any, 0, len(n.Content))
		for _, e := range n.Content {
			v, err := c.value(e)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return scalar(n, tag)
	}
	return nil, YAMLError{fmt.Sprintf("unexpected YAML node kind %d", n.Kind)}
}

// mapping merges `<<` keys as ruamel does: own keys first, then merged keys not already present.
func (c *converter) mapping(n *yaml.Node) (*Map, error) {
	own := NewMap()
	var merged []*Map
	sawMerge := false
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMergeKey(k) {
			if sawMerge {
				return nil, YAMLError{`found duplicate merge key "<<"`}
			}
			sawMerge = true
			ms, err := c.mergeSources(v)
			if err != nil {
				return nil, err
			}
			merged = append(merged, ms...)
			continue
		}
		key, err := c.key(k)
		if err != nil {
			return nil, err
		}
		if _, dup := own.Get(key); dup {
			return nil, YAMLError{fmt.Sprintf("found duplicate key %q", key)}
		}
		val, err := c.value(v)
		if err != nil {
			return nil, err
		}
		own.Set(key, val)
	}
	for _, m := range merged {
		for _, k := range m.keys {
			if _, ok := own.Get(k); !ok {
				own.Set(k, m.vals[k])
			}
		}
	}
	return own, nil
}

func isMergeKey(k *yaml.Node) bool {
	if k.Kind != yaml.ScalarNode {
		return false
	}
	if k.Style&yaml.TaggedStyle != 0 {
		return k.Tag == "!!merge"
	}
	return k.Style == 0 && k.Value == "<<"
}

func (c *converter) mergeSources(v *yaml.Node) ([]*Map, error) {
	for v.Kind == yaml.AliasNode {
		v = v.Alias
	}
	nodes := []*yaml.Node{v}
	if v.Kind == yaml.SequenceNode {
		nodes = v.Content
	}
	out := make([]*Map, 0, len(nodes))
	for _, e := range nodes {
		for e.Kind == yaml.AliasNode {
			e = e.Alias
		}
		if e.Kind != yaml.MappingNode {
			return nil, YAMLError{"expected a mapping or list of mappings for merging"}
		}
		m, err := c.value(e)
		if err != nil {
			return nil, err
		}
		out = append(out, m.(*Map))
	}
	return out, nil
}

// key renders a scalar key as json.dumps renders a dict key.
func (c *converter) key(k *yaml.Node) (string, error) {
	v, err := c.value(k)
	if err != nil {
		return "", err
	}
	switch v := v.(type) {
	case string:
		return v, nil
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(v), nil
	case json.Number:
		return string(v), nil
	case NonFinite:
		return map[NonFinite]string{PyInf: "Infinity", PyNegInf: "-Infinity", PyNaN: "NaN"}[v], nil
	}
	return "", YAMLError{"a frontmatter key must be a scalar"}
}

func unsupportedTag(tag string) error { return YAMLError{"unsupported YAML tag " + tag} }

// The YAML 1.2 implicit resolvers from ruamel's resolver.py.
var (
	resolveBool  = regexp.MustCompile(`^(?:true|True|TRUE|false|False|FALSE)$`)
	resolveFloat = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+]?[0-9]+)?` +
		`|[-+]?(?:[0-9][0-9_]*)(?:[eE][-+]?[0-9]+)|[-+]?\.[0-9_]+(?:[eE][-+][0-9]+)?` +
		`|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	resolveInt       = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0o?[0-7_]+|[-+]?[0-9_]+|[-+]?0x[0-9a-fA-F_]+)$`)
	resolveNull      = regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)
	resolveTimestamp = regexp.MustCompile(`^(?:[0-9]{4}-[0-9]{2}-[0-9]{2}` +
		`|[0-9]{4}-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9]{2}:[0-9]{2}(?:\.[0-9]*)?` +
		`(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
)

func scalar(n *yaml.Node, tag string) (any, error) {
	s := n.Value
	if tag == "" {
		if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return s, nil
		}
		tag = implicitTag(s)
	}
	switch tag {
	case "!!str":
		return s, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		b, ok := map[string]bool{"yes": true, "no": false, "true": true, "false": false, "on": true, "off": false}[strings.ToLower(s)]
		if !ok {
			return nil, YAMLError{fmt.Sprintf("cannot construct a bool from %q", s)}
		}
		// ruamel keeps an anchored bool as a ScalarBoolean, an int subclass that json.dumps writes as 1 or 0.
		if n.Anchor != "" {
			return json.Number(map[bool]string{true: "1", false: "0"}[b]), nil
		}
		return b, nil
	case "!!int":
		return constructInt(s)
	case "!!float":
		return constructFloat(s)
	case "!!timestamp":
		return constructTimestamp(s)
	}
	return nil, unsupportedTag(tag)
}

func implicitTag(s string) string {
	switch {
	case resolveNull.MatchString(s):
		return "!!null"
	case resolveBool.MatchString(s):
		return "!!bool"
	case resolveFloat.MatchString(s):
		return "!!float"
	// The int regex alone would take `_1`, which ruamel never tries because `_` is not a first character it lists.
	case s != "" && strings.IndexByte("-+0123456789", s[0]) >= 0 && resolveInt.MatchString(s):
		return "!!int"
	case resolveTimestamp.MatchString(s):
		return "!!timestamp"
	}
	return "!!str"
}

func constructInt(s string) (any, error) {
	digits := strings.ReplaceAll(s, "_", "")
	neg := false
	if digits != "" && (digits[0] == '-' || digits[0] == '+') {
		neg = digits[0] == '-'
		digits = digits[1:]
	}
	base := 10
	if len(digits) > 1 && digits[0] == '0' {
		switch digits[1] {
		case 'b':
			base, digits = 2, digits[2:]
		case 'x':
			base, digits = 16, digits[2:]
		case 'o':
			base, digits = 8, digits[2:]
		}
	}
	i, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return nil, YAMLError{fmt.Sprintf("cannot construct an int from %q", s)}
	}
	if neg {
		i.Neg(i)
	}
	return json.Number(i.String()), nil
}

func constructFloat(s string) (any, error) {
	v := strings.ToLower(strings.ReplaceAll(s, "_", ""))
	switch strings.TrimLeft(v, "+-") {
	case ".inf":
		if strings.HasPrefix(v, "-") {
			return PyNegInf, nil
		}
		return PyInf, nil
	case ".nan":
		return PyNaN, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, YAMLError{fmt.Sprintf("cannot construct a float from %q", s)}
	}
	switch {
	case math.IsInf(f, 1):
		return PyInf, nil
	case math.IsInf(f, -1):
		return PyNegInf, nil
	}
	return json.Number(pyFloatRepr(f)), nil
}

var timestampParts = regexp.MustCompile(`^([0-9]{4})-([0-9][0-9]?)-([0-9][0-9]?)` +
	`(?:(?:([Tt])|[ \t]+)([0-9][0-9]?):([0-9]{2}):([0-9]{2})(?:\.([0-9]*))?` +
	`(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9]{2}))?))?)?$`)

// constructTimestamp is Python's str() of ruamel's date, naive datetime or TimeStamp.
func constructTimestamp(s string) (any, error) {
	m := timestampParts.FindStringSubmatch(s)
	if m == nil {
		return nil, YAMLError{fmt.Sprintf("failed to construct timestamp from %q", s)}
	}
	atoi := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
	year, month, day := atoi(1), atoi(2), atoi(3)
	invalid := YAMLError{fmt.Sprintf("invalid timestamp %q", s)}
	if year < 1 || month < 1 || month > 12 || day < 1 {
		return nil, invalid
	}
	if m[5] == "" {
		d := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		if d.Day() != day {
			return nil, invalid
		}
		return d.Format("2006-01-02"), nil
	}
	hour, minute, second := atoi(5), atoi(6), atoi(7)
	if hour > 23 || minute > 59 || second > 59 {
		return nil, invalid
	}
	// ruamel keeps six digits and rounds on the seventh only.
	frac := 0
	if f := m[8]; f != "" {
		f6 := (f + "000000")[:6]
		frac, _ = strconv.Atoi(f6)
		if len(f) > 6 && f[6] > '4' {
			frac++
		}
	}
	zone := time.UTC
	if m[10] != "" {
		off := atoi(11)*3600 + atoi(12)*60
		if off >= 24*3600 {
			return nil, invalid
		}
		if m[10] == "-" {
			off = -off
		}
		zone = time.FixedZone("", off)
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, zone)
	if t.Day() != day {
		return nil, invalid
	}
	if frac > 999999 {
		t = t.Add(time.Second)
	} else {
		t = t.Add(time.Duration(frac) * time.Microsecond)
	}
	sep := " "
	if m[4] != "" {
		sep = "T"
	}
	out := t.Format("2006-01-02" + sep + "15:04:05")
	if t.Nanosecond() != 0 {
		out += fmt.Sprintf(".%06d", t.Nanosecond()/1000)
	}
	if m[9] != "" {
		out += t.Format("-07:00")
	}
	return out, nil
}
