package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_create", Arguments: map[string]any{
		"path": "a", "type": "Concept", "body": secret,
	}}); err != nil {
		t.Fatal(err)
	}
	line := logLine(t, logs, "tool call", map[string]any{
		"level": "INFO", "tool": "okf_create", "outcome": "ok", "tenant": tools.t.String(), "actor": "mcp",
	})
	if _, ok := line["duration_ms"].(float64); !ok {
		t.Errorf("no duration_ms in %v", line)
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("a concept body reached the logs:\n%s", logs)
	}
}

func TestAnUnavailableDatabaseIsLogged(t *testing.T) {
	logs := captureLogs(t)
	session := connect(t, newTools(t))
	db.LockOut(t)
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "okf_list", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	logLine(t, logs, "database unavailable", map[string]any{"level": "WARN", "tool": "okf_list"})
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
