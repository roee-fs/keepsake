package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestToolCallsAreCountedByOutcome(t *testing.T) {
	session := connect(t, newTools(t))
	calls := []struct {
		tool, outcome string
		args          map[string]any
	}{
		{"okf_create", "ok", map[string]any{"path": "a", "type": "Concept"}},
		{"okf_update", "conflict", map[string]any{"path": "a", "expected_version": 9, "body": "x"}},
		{"okf_create", "tool_error", map[string]any{"path": "a", "type": "Concept"}},
		{"okf_read", "tool_error", map[string]any{}},
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

func TestMetricsAreServedOnlyByTheMetricsHandler(t *testing.T) {
	t.Setenv("KEEPSAKE_ADMIN_PASSWORD", adminPassword)
	h, metrics, closeApp, err := BuildApp(ctx, Config{DSN: db.AppDSN, Auth: FixedTenant(uuid.New()), Schema: "okf"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeApp)

	rec := httptest.NewRecorder()
	metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{`keepsake_tool_calls_total{outcome="ok",tool="okf_read"}`, "keepsake_db_pool_max_connections", "go_goroutines"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "keepsake_tool_calls_total") {
		t.Error("the app handler serves metrics")
	}
}
