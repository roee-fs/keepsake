package okf

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// AttestedComputation is the OKF §10 type whose contract Families checks.
const AttestedComputation = "Attested Computation"

var actor = regexp.MustCompile(`^([^\s/:]+/[^\s/]+|(human|process):\S+)$`)

// IsActor reports whether s is an OKF §7 actor: <producer>/<version>, human:<id> or process:<id>.
func IsActor(s string) bool { return actor.MatchString(s) }

const notInstant = "must be an ISO 8601 datetime with an offset, such as 2026-06-30T14:00:00Z"

// Families returns the OKF v0.2 §5 and §10 rules c breaks, keyed by the top-level frontmatter key at fault.
func Families(c Concept) map[string][]string {
	fm := c.Frontmatter
	if fm == nil {
		fm = NewMap()
	}
	errs := map[string][]string{}
	add := func(key string, msgs ...string) {
		if len(msgs) > 0 {
			errs[key] = append(errs[key], msgs...)
		}
	}
	if v, ok := fm.Get("status"); ok {
		if s, _ := v.(string); s != "draft" && s != "stable" && s != "deprecated" {
			add("status", "status must be draft, stable or deprecated")
		}
	}
	if v, ok := fm.Get("stale_after"); ok && !isInstant(v) {
		add("stale_after", "stale_after "+notInstant)
	}
	if v, ok := fm.Get("generated"); ok {
		add("generated", event(v, "generated", false)...)
	}
	if v, ok := fm.Get("verified"); ok {
		events, isList := v.([]any)
		if !isList {
			events = []any{v}
		}
		for i, e := range events {
			add("verified", event(e, fmt.Sprintf("verified[%d]", i), true)...)
		}
	}
	if v, ok := fm.Get("sources"); ok {
		add("sources", sources(v)...)
	}
	if v, ok := fm.Get("usage_window"); ok {
		add("usage_window", window(v, "usage_window")...)
	}
	if c.Type == AttestedComputation {
		contract(fm, c.Body, add)
	}
	return errs
}

// contract checks the §10.2 fields and the §10.3 computation. On any other type these keys are producer extensions.
func contract(fm *Map, body string, add func(string, ...string)) {
	if !nonEmpty(field(fm, "runtime")) {
		add("runtime", "runtime is required for an Attested Computation")
	}
	blocks := computationBlocks(body)
	if v, ok := fm.Get("computation"); ok {
		if !nonEmpty(v) {
			add("computation", "computation must be a path")
		} else if blocks > 0 {
			add("computation", fmt.Sprintf("computation names a file, so the body must not also hold a code block under # Computation: found %d", blocks))
		}
	} else if blocks != 1 {
		add("computation", fmt.Sprintf("the body must hold exactly one code block under # Computation, or computation must name a file: found %d", blocks))
	}
	if v, ok := fm.Get("parameters"); ok {
		params, isList := v.([]any)
		if !isList {
			add("parameters", "parameters must be a list of { name, type, required }")
		}
		for i, p := range params {
			m, _ := p.(*Map)
			if m == nil || !nonEmpty(field(m, "name")) {
				add("parameters", fmt.Sprintf("parameters[%d].name is required", i))
				continue
			}
			if r, ok := m.Get("required"); ok {
				if _, isBool := r.(bool); !isBool {
					add("parameters", fmt.Sprintf("parameters[%d].required must be true or false", i))
				}
			}
		}
	}
	for _, k := range []string{"executor", "attester"} {
		if v, ok := fm.Get(k); ok {
			if m, _ := v.(*Map); m == nil || !nonEmpty(field(m, "resource")) {
				add(k, k+".resource is required")
			}
		}
	}
}

var (
	heading            = regexp.MustCompile(`^(#{1,6})(?:[ \t]|$)`)
	computationHeading = regexp.MustCompile(`^#[ \t]+Computation(?:[ \t]+#*)?[ \t]*(?:\{[^}]*\})?[ \t]*$`)
)

// computationBlocks counts the fenced and indented code blocks under the body's top-level # Computation headings.
// ponytail: line-based, not CommonMark; a list item or footnote continued four spaces in reads as code, and a setext heading is not seen.
func computationBlocks(body string) int {
	n, in, openFence, indented, blank := 0, false, "", false, true
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		lead := len(line) - len(trimmed)
		if openFence != "" {
			// Only a bare fence at least as long as the opening one closes it.
			if rest := strings.TrimLeft(trimmed, openFence[:1]); lead < 4 && len(trimmed)-len(rest) >= len(openFence) && strings.TrimSpace(rest) == "" {
				openFence, blank = "", true
			}
			continue
		}
		switch h := heading.FindStringSubmatch(trimmed); {
		case strings.TrimSpace(line) == "":
			blank = true
			continue
		case lead < 4 && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")):
			openFence = strings.Repeat(trimmed[:1], len(trimmed)-len(strings.TrimLeft(trimmed, trimmed[:1])))
			if in {
				n++
			}
			indented = false
		// A tab after up to three spaces reaches column four.
		case lead >= 4 || strings.HasPrefix(trimmed, "\t"):
			// An indented line right after a paragraph continues it; after a blank line it starts code.
			if !indented && blank {
				indented = true
				if in {
					n++
				}
			}
		case h != nil:
			// A subheading stays inside the section; the next top-level heading ends it.
			if len(h[1]) == 1 {
				in = computationHeading.MatchString(trimmed)
			}
			// Code MAY follow a heading without a blank line between.
			indented, blank = false, true
			continue
		default:
			indented = false
		}
		blank = false
	}
	return n
}

func sources(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return []string{"sources must be a list"}
	}
	var errs []string
	for i, s := range list {
		name := fmt.Sprintf("sources[%d]", i)
		m, _ := s.(*Map)
		if m == nil {
			errs = append(errs, name+" must be a mapping")
			continue
		}
		if !nonEmpty(field(m, "resource")) {
			errs = append(errs, name+".resource is required")
		}
		if n, ok := m.Get("usage_count"); ok && !isCount(n) {
			errs = append(errs, name+".usage_count must be a whole number, at least 0")
		}
		if t, ok := m.Get("last_modified"); ok && !isInstant(t) {
			errs = append(errs, name+".last_modified "+notInstant)
		}
		if w, ok := m.Get("usage_window"); ok {
			errs = append(errs, window(w, name+".usage_window")...)
		}
	}
	return errs
}

func window(v any, name string) []string {
	m, _ := v.(*Map)
	if m == nil {
		return []string{name + " must be a { from, to } mapping"}
	}
	var errs []string
	var at [2]time.Time
	for i, k := range []string{"from", "to"} {
		var ok bool
		if at[i], ok = strictInstant(field(m, k)); !ok {
			errs = append(errs, name+"."+k+" "+notInstant)
		}
	}
	if len(errs) == 0 && at[1].Before(at[0]) {
		errs = append(errs, name+".to must not precede its from")
	}
	return errs
}

// event checks a §5.2 { by, at } mapping. at is required in verified and optional in generated.
func event(v any, name string, needAt bool) []string {
	m, _ := v.(*Map)
	if m == nil {
		return []string{name + " must be a { by, at } mapping"}
	}
	var errs []string
	if by, _ := field(m, "by").(string); !IsActor(by) {
		errs = append(errs, name+".by must be an actor: <producer>/<version>, human:<id> or process:<id>")
	}
	if at, ok := m.Get("at"); ok && !isInstant(at) || !ok && needAt {
		errs = append(errs, name+".at "+notInstant)
	}
	return errs
}

func nonEmpty(v any) bool {
	s, _ := v.(string)
	return strings.TrimSpace(s) != ""
}

// isCount accepts an int set from Go and any json.Number spelling of a whole number, such as 5000.0 or 5e3.
func isCount(v any) bool {
	switch n := v.(type) {
	case int:
		return n >= 0
	case json.Number:
		f, err := n.Float64()
		return err == nil && f >= 0 && f == math.Trunc(f)
	}
	return false
}

// rfc3339 bounds what time.Parse accepts, which includes a one-digit hour and a +24:00 offset.
var rfc3339 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$`)

// strictInstant holds a value to §5, unlike Derive, which reads naive and date-only values leniently.
// RFC 3339 allows a space for the T, and the YAML parser keeps one.
func strictInstant(v any) (time.Time, bool) {
	s, _ := v.(string)
	if !rfc3339.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.Replace(s, " ", "T", 1))
	return t, err == nil
}

func isInstant(v any) bool {
	_, ok := strictInstant(v)
	return ok
}
