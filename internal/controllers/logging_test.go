package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/roee-fs/keepsake/internal/services"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// logLine returns the first line whose msg is msg and whose fields include want.
func logLine(t *testing.T, buf *bytes.Buffer, msg string, want map[string]any) map[string]any {
	t.Helper()
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) != nil || line["msg"] != msg {
			continue
		}
		matched := true
		for k, v := range want {
			matched = matched && line[k] == v
		}
		if matched {
			return line
		}
	}
	t.Fatalf("no %q line with %v in:\n%s", msg, want, buf)
	return nil
}

func TestEachToolCallIsLoggedWithoutItsArguments(t *testing.T) {
	logs := captureLogs(t)
	tools := newTools(t)
	session := connect(t, tools)
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create", Arguments: map[string]any{
		"path": "a", "type": "Concept", "body": secret,
	}}); err != nil {
		t.Fatal(err)
	}
	line := logLine(t, logs, "tool call", map[string]any{
		"level": "INFO", "tool": "create", "outcome": "ok", "tenant": tools.Tenant().String(), "actor": "mcp",
	})
	if _, ok := line["duration_ms"].(float64); !ok {
		t.Errorf("no duration_ms in %v", line)
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("a concept body reached the logs:\n%s", logs)
	}
}

func callDirect(t *testing.T, tools *services.ConceptService, call handler) (*mcp.CallToolResult, error) {
	t.Helper()
	h := toolHandler(tools, "list", compile(map[string]any{"type": "object"}), call, make(chan struct{}, 1))
	return h(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{}`)}})
}

func TestADefectIsLoggedAndHiddenFromTheClient(t *testing.T) {
	logs := captureLogs(t)
	_, err := callDirect(t, newTools(t), func(context.Context, *services.ConceptService, map[string]any) (any, error) {
		return nil, errors.New("role keepsake_app on db-7")
	})
	if !errors.Is(err, errInternal) {
		t.Fatalf("err = %v", err)
	}
	logLine(t, logs, "tool call failed", map[string]any{"level": "ERROR", "tool": "list", "err": "role keepsake_app on db-7"})
}

func TestAPanickingToolIsRecoveredAndLogged(t *testing.T) {
	logs := captureLogs(t)
	_, err := callDirect(t, newTools(t), func(context.Context, *services.ConceptService, map[string]any) (any, error) {
		panic("boom")
	})
	if !errors.Is(err, errInternal) {
		t.Fatalf("err = %v", err)
	}
	line := logLine(t, logs, "tool call panicked", map[string]any{"level": "ERROR", "tool": "list", "panic": "boom"})
	if s, _ := line["stack"].(string); !strings.Contains(s, "runtime/debug.Stack") {
		t.Errorf("no stack in %v", line)
	}
	logLine(t, logs, "tool call", map[string]any{"outcome": "error"})
}

func TestACallWithoutATenantIsLogged(t *testing.T) {
	logs := captureLogs(t)
	tools := services.NewConceptService(conceptStore(t), uuid.Nil, "mcp")
	if _, err := callDirect(t, tools, nil); err == nil {
		t.Fatal("a nil tenant was served")
	}
	logLine(t, logs, "tool call without a tenant", map[string]any{"level": "ERROR", "tool": "list"})
}

func TestAnUnavailableDatabaseIsLogged(t *testing.T) {
	logs := captureLogs(t)
	session := connect(t, newTools(t))
	db.LockOut(t)
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	logLine(t, logs, "database unavailable", map[string]any{"level": "WARN", "tool": "list"})
}

func TestConsoleLoginsAreLoggedWithoutThePassword(t *testing.T) {
	logs := captureLogs(t)
	c := newConsole(t)
	c.request(http.MethodPost, "/session", `{"password": "wrong-zqxjkpass"}`)
	logLine(t, logs, "console login refused", map[string]any{"level": "WARN", "remote_addr": "192.0.2.1:1234"})
	c.login()
	logLine(t, logs, "console login", map[string]any{"level": "INFO"})
	if strings.Contains(logs.String(), "zqxjkpass") || strings.Contains(logs.String(), adminPassword) {
		t.Errorf("a password reached the logs:\n%s", logs)
	}
}

func TestAnAPIFailureNamesItsRoute(t *testing.T) {
	logs := captureLogs(t)
	r := httptest.NewRequest(http.MethodGet, "/concepts/a", nil)
	r.Pattern = "GET /concepts/{path...}"
	internalError(httptest.NewRecorder(), r, errors.New("boom"))
	logLine(t, logs, "api", map[string]any{"level": "ERROR", "method": "GET", "route": "GET /concepts/{path...}", "err": "boom"})
}

func TestAnUnencodableResponseNamesItsRoute(t *testing.T) {
	logs := captureLogs(t)
	// The 422 echoes NaN, which JSON cannot hold.
	newConsole(t).request(http.MethodPost, "/session", `{"password": NaN}`)
	logLine(t, logs, "api", map[string]any{"level": "ERROR", "method": "POST", "route": "POST /session"})
}
