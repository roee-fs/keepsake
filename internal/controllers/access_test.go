package controllers

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestARejectedMCPRequestLeavesAnAccessLine(t *testing.T) {
	logs := captureLogs(t)
	mux := http.NewServeMux()
	mux.Handle("/mcp", NewMCPHandler(newTools(t)))
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	AccessLog(mux).ServeHTTP(w, r)
	if w.Code < http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
	logLine(t, logs, "request", map[string]any{"level": "WARN", "method": "POST", "route": "/mcp", "status": float64(w.Code)})
}

// A flush sends 200 when no status was set, so a later WriteHeader reaches no client.
func TestAFlushBeforeAStatusLogsTheStatusSent(t *testing.T) {
	logs := captureLogs(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		w.WriteHeader(http.StatusInternalServerError)
	})
	w := httptest.NewRecorder()
	AccessLog(mux).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("the client got %d", w.Code)
	}
	if strings.Contains(logs.String(), `"status":500`) {
		t.Fatalf("logged a status the client never got:\n%s", logs)
	}
}

func TestASuccessfulRequestLogsAtDebugWithoutItsPath(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /concepts/{path...}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	AccessLog(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/concepts/secret-runbook", nil))
	logLine(t, &buf, "request", map[string]any{"level": "DEBUG", "route": "GET /concepts/{path...}", "status": float64(200)})
	if strings.Contains(buf.String(), "secret-runbook") {
		t.Errorf("a concept path reached the logs:\n%s", &buf)
	}
}
