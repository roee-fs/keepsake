package controllers

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestToolCallsAreCountedByOutcome(t *testing.T) {
	session := connect(t, newTools(t))
	calls := []struct {
		tool, outcome string
		args          map[string]any
	}{
		{"create", "ok", map[string]any{"path": "a", "type": "Concept"}},
		{"update", "conflict", map[string]any{"path": "a", "expected_version": 9, "body": "x"}},
		{"create", "tool_error", map[string]any{"path": "a", "type": "Concept"}},
		{"read", "tool_error", map[string]any{}},
	}
	for _, c := range calls {
		counter := toolCalls.WithLabelValues(c.tool, c.outcome)
		before := testutil.ToFloat64(counter)
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: c.args}); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(counter) - before; got != 1 {
			t.Errorf("%s %v: %s counted %v times, want 1", c.tool, c.args, c.outcome, got)
		}
	}
}
