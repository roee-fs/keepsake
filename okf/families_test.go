package okf

import (
	"encoding/json"
	"testing"
)

func families(t *testing.T, doc string) map[string][]string {
	t.Helper()
	c, err := Parse(doc, "a")
	if err != nil {
		t.Fatal(err)
	}
	return Families(c)
}

// The spec's Appendix A revenue concept, with a bare verified mapping and a team: author.
const specRevenue = `---
type: Attested Computation
status: stable
runtime: bigquery
parameters:
  - { name: year, type: integer, required: true }
executor:
  resource: references/skills/run-on-bq.md
  receipt: [job_id, executed_sql, result]
attester:
  resource: references/attesters/sql-equality.py
generated: { by: reference_agent/gemini-2.5-pro, at: 2026-06-28T14:00:00Z }
verified: { by: human:ahormati, at: 2026-06-25T09:00:00Z }
stale_after: 2026-12-31T00:00:00Z
sources:
  - id: exec-rev-dash
    resource: dashboards/exec-revenue
    author: team:finance-fpa
    usage_count: 5000
    last_modified: 2026-06-18T00:00:00Z
usage_window: { from: 2026-06-01T00:00:00Z, to: 2026-06-30T00:00:00Z }
---
`

func TestTheSpecsOwnExampleBreaksNoFamilyRule(t *testing.T) {
	if got := families(t, specRevenue); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestEachMalformedFamilyIsReportedUnderItsKey(t *testing.T) {
	cases := map[string]string{
		"status":       "status: archived",
		"stale_after":  "stale_after: 2026-09-23",
		"generated":    "generated: {by: someone, at: 2026-06-28T14:00:00Z}",
		"verified":     "verified: [{by: human:ann}]",
		"sources":      "sources: [{id: s, usage_count: -1}]",
		"usage_window": "usage_window: {from: 2026-06-01 00:00:00}",
	}
	for key, line := range cases {
		got := families(t, "---\ntype: Concept\n"+line+"\n---\n")
		if len(got) != 1 || len(got[key]) == 0 {
			t.Errorf("%s: got %q", key, got)
		}
	}
}

func TestAReversedUsageWindowIsReported(t *testing.T) {
	got := families(t, "---\ntype: Concept\nusage_window: {from: 2026-06-30T00:00:00Z, to: 2026-06-01T00:00:00Z}\n---\n")
	if len(got["usage_window"]) == 0 {
		t.Fatalf("got %q", got)
	}
}

func TestAnAttestedComputationNeedsARuntimeAndWellFormedContract(t *testing.T) {
	got := families(t, "---\ntype: Attested Computation\nparameters: [{type: integer}]\nexecutor: {receipt: [job_id]}\n---\n")
	for _, k := range []string{"runtime", "parameters", "executor"} {
		if len(got[k]) == 0 {
			t.Errorf("%s not reported: %q", k, got)
		}
	}
}

func TestComputationKeysOnAnotherTypeAreExtensions(t *testing.T) {
	if got := families(t, "---\ntype: Metric\nexecutor: 7\nparameters: x\n---\n"); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestValuesFromAToolCallAreValid(t *testing.T) {
	src := NewMap()
	src.Set("resource", "https://example.com")
	src.Set("usage_count", json.Number("5000"))
	src.Set("last_modified", "2026-06-18T00:00:00.5Z")
	fm := NewMap()
	fm.Set("sources", []any{src})
	if got := Families(Concept{Type: "Concept", Frontmatter: fm}); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestIsActor(t *testing.T) {
	for s, want := range map[string]bool{
		"support-agent/1.4": true, "human:ann": true, "process:import": true,
		"": false, "run:42": false, "human:": false, "a b/1": false, "mcp": false,
	} {
		if IsActor(s) != want {
			t.Errorf("IsActor(%q) = %v", s, !want)
		}
	}
}
