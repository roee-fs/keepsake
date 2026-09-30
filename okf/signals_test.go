package okf

import (
	"testing"
	"time"
)

func TestDerive(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, frontmatter string
		want              Signals
	}{
		{"absent", "", Signals{Status: "stable", Trust: Unverified}},
		{"draft", "status: draft", Signals{Status: "draft", Trust: Unverified}},
		{"deprecated", "status: deprecated", Signals{Status: "deprecated", Trust: Unverified}},
		{"non-string status", "status: 3", Signals{Status: "stable", Trust: Unverified}},
		{"stale at the instant", "stale_after: 2026-09-23T00:00:00Z", Signals{Status: "stable", Stale: true, Trust: Unverified}},
		{"stale in the past", "stale_after: 2026-09-22 23:59:59+00:00", Signals{Status: "stable", Stale: true, Trust: Unverified}},
		{"fresh", "stale_after: '2026-09-23T00:00:01Z'", Signals{Status: "stable", Trust: Unverified}},
		{"offset", "stale_after: 2026-09-23T01:00:00+02:00", Signals{Status: "stable", Stale: true, Trust: Unverified}},
		{"date only", "stale_after: 2026-09-23", Signals{Status: "stable", Stale: true, Trust: Unverified}},
		{"malformed stale_after", "stale_after: soon", Signals{Status: "stable", Trust: Unverified}},
		{"numeric stale_after", "stale_after: 5", Signals{Status: "stable", Trust: Unverified}},
		{"bare human mapping", "verified: {by: 'human:ann', at: 2026-06-25T09:00:00Z}", Signals{Status: "stable", Trust: HumanReviewed}},
		{"bare machine mapping", "verified: {by: process:nightly}", Signals{Status: "stable", Trust: MachineConfirmed}},
		{"machine then human", "verified:\n  - {by: process:nightly}\n  - {by: 'human:ann'}", Signals{Status: "stable", Trust: HumanReviewed}},
		{"machines only", "verified:\n  - {by: agent/v1}\n  - {by: process:nightly}", Signals{Status: "stable", Trust: MachineConfirmed}},
		{"empty list", "verified: []", Signals{Status: "stable", Trust: Unverified}},
		{"malformed verified", "verified: 'yes'", Signals{Status: "stable", Trust: Unverified}},
		{"entry without by", "verified: [{at: 2026-06-25T09:00:00Z}]", Signals{Status: "stable", Trust: Unverified}},
		{"generated", "generated: {by: agent/v1, at: 2026-06-20T22:53:05Z}", Signals{Status: "stable", Trust: Unverified, GeneratedAt: "2026-06-20T22:53:05+00:00"}},
		{"legacy timestamp", "timestamp: '2026-01-01T00:00:00Z'", Signals{Status: "stable", Trust: Unverified, GeneratedAt: "2026-01-01T00:00:00Z"}},
		{"generated wins", "timestamp: old\ngenerated: {by: a/1, at: new}", Signals{Status: "stable", Trust: Unverified, GeneratedAt: "new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse("---\ntype: T\n"+tc.frontmatter+"\n---\n", "p")
			if err != nil {
				t.Fatal(err)
			}
			if got := Derive(c.Frontmatter, now); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := Derive(nil, now); got != (Signals{Status: "stable", Trust: Unverified}) {
		t.Fatalf("nil frontmatter: %+v", got)
	}
}
