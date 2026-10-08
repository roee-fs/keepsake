package okf

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// AttestedComputation is the OKF §10 type whose contract Families checks.
const AttestedComputation = "Attested Computation"

// ContractFields are the frontmatter keys of an Attested Computation's §10.2 contract.
var ContractFields = []string{"computation", "runtime", "parameters", "executor", "attester"}

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
		contract(fm, add)
	}
	return errs
}

// contract checks the §10.2 fields. On any other type these keys are producer extensions.
func contract(fm *Map, add func(string, ...string)) {
	if !nonEmpty(field(fm, "runtime")) {
		add("runtime", "runtime is required for an Attested Computation")
	}
	if v, ok := fm.Get("computation"); ok && !nonEmpty(v) {
		add("computation", "computation must be a path")
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
	for _, k := range []string{"from", "to"} {
		if !isInstant(field(m, k)) {
			errs = append(errs, name+"."+k+" "+notInstant)
		}
	}
	if len(errs) == 0 {
		from, _ := time.Parse(time.RFC3339, field(m, "from").(string))
		to, _ := time.Parse(time.RFC3339, field(m, "to").(string))
		if to.Before(from) {
			errs = append(errs, name+".to must not precede its from")
		}
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

// isCount accepts the json.Number the YAML parser and a tool call yield, and a Go int.
func isCount(v any) bool {
	switch n := v.(type) {
	case int:
		return n >= 0
	case json.Number:
		i, err := n.Int64()
		return err == nil && i >= 0
	}
	return false
}

// isInstant holds a value to §5, unlike Derive, which reads naive and date-only values leniently.
func isInstant(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}
