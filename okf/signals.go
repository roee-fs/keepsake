package okf

import (
	"strings"
	"time"
)

// Signals are the trust and lifecycle facts a consumer derives from OKF v0.2 §5 frontmatter.
type Signals struct {
	Status string `json:"status"`
	Stale  bool   `json:"stale"`
	Trust  string `json:"trust"`
	// VerifiedStale is true when the content changed after the latest verification that sets Trust.
	VerifiedStale bool   `json:"verified_stale"`
	GeneratedAt   string `json:"generated_at"`
}

// The §5.3 trust tiers, lowest first.
const (
	Unverified       = "unverified"
	MachineConfirmed = "machine-confirmed"
	HumanReviewed    = "human-reviewed"
)

// Derive reads Signals from fm as of now. A missing or malformed field reads as absent, never as an error.
func Derive(fm *Map, now time.Time) Signals {
	if fm == nil {
		fm = NewMap()
	}
	s := Signals{Status: "stable"}
	var reviewed time.Time
	s.Trust, reviewed = trust(field(fm, "verified"))
	if status, _ := field(fm, "status").(string); status != "" {
		s.Status = status
	}
	if at, ok := instant(field(fm, "stale_after")); ok {
		s.Stale = !now.Before(at)
	}
	if g, _ := field(fm, "generated").(*Map); g != nil {
		s.GeneratedAt, _ = field(g, "at").(string)
	}
	if s.GeneratedAt == "" {
		// §13.1: a v0.1 concept records its last change as `timestamp`.
		s.GeneratedAt, _ = field(fm, "timestamp").(string)
	}
	// Unlike stale_after, an instant without an offset reads as unknown here, so it never claims staleness.
	if changed, err := time.Parse(time.RFC3339, s.GeneratedAt); err == nil && !reviewed.IsZero() {
		s.VerifiedStale = changed.After(reviewed)
	}
	return s
}

func field(m *Map, k string) any {
	v, _ := m.Get(k)
	return v
}

// Events reads a §5.2 event field as a list, taking a bare mapping as a one-element list.
func Events(v any) []any {
	switch v := v.(type) {
	case nil:
		return nil
	case []any:
		return v
	}
	return []any{v}
}

// trust returns the tier and the latest instant among the events that set it, zero if none parses.
func trust(verified any) (string, time.Time) {
	events := Events(verified)
	tier := Unverified
	var human, machine time.Time
	for _, e := range events {
		m, _ := e.(*Map)
		if m == nil {
			continue
		}
		by, _ := field(m, "by").(string)
		at, _ := field(m, "at").(string)
		t, _ := time.Parse(time.RFC3339, at)
		switch {
		case strings.HasPrefix(by, "human:"):
			tier = HumanReviewed
			if t.After(human) {
				human = t
			}
		case by != "":
			if tier == Unverified {
				tier = MachineConfirmed
			}
			if t.After(machine) {
				machine = t
			}
		}
	}
	if tier == HumanReviewed {
		return tier, human
	}
	return tier, machine
}

// instant reads an OKF timestamp as the YAML parser or a tool call stores it. No offset reads as UTC.
func instant(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	s = strings.Replace(strings.TrimSpace(s), " ", "T", 1)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
