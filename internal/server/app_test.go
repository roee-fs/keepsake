// Ported from the app tests in 2de90d2:tests/test_tools.py and 8f2af2e:tests/test_api.py.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
)

const shell = "<html>console shell</html>"

// served is the built app on a real socket, so the wire format is the one a pod serves.
func served(t *testing.T) string {
	t.Helper()
	t.Setenv("KEEPSAKE_ADMIN_PASSWORD", adminPassword)
	h, closeApp, err := BuildApp(ctx, Config{DSN: db.AppDSN, TenantID: uuid.New(), Schema: "okf"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); closeApp() })
	return srv.URL
}

func withStatic(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "static")
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(shell), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("js"), 0o644)
	t.Setenv("KEEPSAKE_STATIC_DIR", dir)
	return served(t)
}

func withoutStatic(t *testing.T) string {
	t.Helper()
	t.Setenv("KEEPSAKE_STATIC_DIR", filepath.Join(t.TempDir(), "no-such-dir"))
	return served(t)
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func send(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const toolsList = `{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}`

func wantMCP(t *testing.T, base string) {
	t.Helper()
	resp, body := send(t, http.MethodPost, base+"/mcp", toolsList)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("POST /mcp = %d %s: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	var reply struct{ Result struct{ Tools []any } }
	if err := json.Unmarshal([]byte(body), &reply); err != nil || len(reply.Result.Tools) != 7 {
		t.Fatalf("POST /mcp answered %s", body)
	}
}

func wantReady(t *testing.T, base string) {
	t.Helper()
	resp, body := send(t, http.MethodGet, base+"/readyz", "")
	if resp.StatusCode != 200 || body != `{"ready":true}` {
		t.Fatalf("readyz = %d %s", resp.StatusCode, body)
	}
}

func wantLogin(t *testing.T, base string) {
	t.Helper()
	if resp, body := send(t, http.MethodPost, base+"/api/session", `{"password": "`+adminPassword+`"}`); resp.StatusCode != 204 {
		t.Fatalf("login = %d %s", resp.StatusCode, body)
	}
}

func TestReadyzReportsReadyWhileThePoolCanReachTheDatabase(t *testing.T) {
	wantReady(t, served(t))
}

func TestBuildAppRefusesADatabaseThatDoesNotIsolate(t *testing.T) {
	t.Setenv("KEEPSAKE_ADMIN_PASSWORD", adminPassword)
	_, _, err := BuildApp(ctx, Config{DSN: db.OwnerDSN, TenantID: uuid.New(), Schema: "okf"})
	var m *store.MisconfiguredDatabase
	if !errors.As(err, &m) {
		t.Fatalf("err = %v, want MisconfiguredDatabase", err)
	}
}

func TestBuildAppRefusesAnEnabledConsoleWithoutAPassword(t *testing.T) {
	t.Setenv("KEEPSAKE_ADMIN_PASSWORD", "")
	_, _, err := BuildApp(ctx, Config{DSN: db.AppDSN, TenantID: uuid.New(), Schema: "okf"})
	var m *MisconfiguredAdmin
	if !errors.As(err, &m) {
		t.Fatalf("err = %v, want MisconfiguredAdmin", err)
	}
}

func TestBuildAppServesMCPOnAnIsolatingDatabase(t *testing.T) {
	wantMCP(t, served(t))
}

func TestUIDisabledMountsNoAPIButStillServesMCPAndReadyz(t *testing.T) {
	t.Setenv("KEEPSAKE_UI", "false")
	base := withStatic(t)
	if resp, _ := send(t, http.MethodPost, base+"/api/session", `{"password": "x"}`); resp.StatusCode != 404 {
		t.Fatalf("/api/session = %d, want 404", resp.StatusCode)
	}
	if resp, _ := send(t, http.MethodGet, base+"/", ""); resp.StatusCode != 404 {
		t.Fatalf("/ = %d: the console is part of the UI", resp.StatusCode)
	}
	wantMCP(t, base)
	wantReady(t, base)
}

func TestMissingStaticBundleSkipsTheMountButAPIAndMCPStillServe(t *testing.T) {
	base := withoutStatic(t)
	if resp, _ := send(t, http.MethodGet, base+"/", ""); resp.StatusCode != 404 {
		t.Fatalf("/ = %d, want 404", resp.StatusCode)
	}
	wantReady(t, base)
	wantLogin(t, base)
	wantMCP(t, base)
}

func TestStaticBundlePresentServesTheConsoleLastWithSPAFallback(t *testing.T) {
	base := withStatic(t)
	for _, p := range []string{"/", "/concepts/notes%2Fa.md"} {
		resp, body := send(t, http.MethodGet, base+p, "")
		if resp.StatusCode != 200 || body != shell {
			t.Fatalf("%s = %d %q", p, resp.StatusCode, body)
		}
		// The shell names this build's assets, so it MUST NOT outlive an upgrade.
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s Cache-Control = %q", p, cc)
		}
	}
	if resp, _ := send(t, http.MethodGet, base+"/assets/app.js", ""); resp.Header.Get("Cache-Control") != "" {
		t.Fatalf("a hashed asset is served with Cache-Control %q", resp.Header.Get("Cache-Control"))
	}
	// The console MUST NOT stop the router redirecting a trailing slash.
	if resp, _ := send(t, http.MethodPost, base+"/mcp/", "{}"); resp.StatusCode != 307 || resp.Header.Get("Location") != "/mcp" {
		t.Fatalf("POST /mcp/ = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	wantReady(t, base)
	wantLogin(t, base)
	wantMCP(t, base)
}

func TestStaticFallsBackToTheShellButNotForAPIAndMCP(t *testing.T) {
	base := withStatic(t)
	if resp, body := send(t, http.MethodGet, base+"/concepts/a/b", ""); resp.StatusCode != 200 || body != shell {
		t.Fatalf("deep link = %d %q", resp.StatusCode, body)
	}
	if resp, body := send(t, http.MethodGet, base+"/assets/app.js", ""); resp.StatusCode != 200 || body != "js" {
		t.Fatalf("asset = %d %q", resp.StatusCode, body)
	}
	if resp, _ := send(t, http.MethodGet, base+"/api/tenants", ""); resp.StatusCode != 401 {
		t.Fatalf("/api/tenants = %d, want 401", resp.StatusCode)
	}
	wantMCP(t, base)
}

// Starlette's behaviour at 8f2af2e: an unmatched path one slash away from a route
// redirects, and the console, as the router's fallback, answers the rest.
func TestUnmatchedPathsBehaveAsStarlettesRouter(t *testing.T) {
	type want struct {
		status   int
		location string
		body     string
	}
	cases := []struct {
		static       bool
		method, path string
		want         want
	}{
		{true, "GET", "/mcp/", want{307, "/mcp", ""}},
		{true, "POST", "/mcp/", want{307, "/mcp", ""}},
		{true, "GET", "/readyz/", want{307, "/readyz", ""}},
		{true, "GET", "/api", want{307, "/api/", ""}},
		{true, "GET", "/elsewhere", want{200, "", shell}},
		{true, "POST", "/readyz", want{405, "", ""}},
		{true, "POST", "/elsewhere", want{405, "", ""}},
		{true, "GET", "/assets", want{200, "", shell}},
		{false, "GET", "/mcp/", want{307, "/mcp", ""}},
		{false, "POST", "/mcp/", want{307, "/mcp", ""}},
		{false, "GET", "/readyz/", want{307, "/readyz", ""}},
		{false, "GET", "/api", want{307, "/api/", ""}},
		{false, "POST", "/readyz", want{405, "", ""}},
		{false, "GET", "/elsewhere", want{404, "", ""}},
	}
	bases := map[bool]string{true: withStatic(t)}
	t.Setenv("KEEPSAKE_STATIC_DIR", "/no/such/dir")
	bases[false] = served(t)
	for _, c := range cases {
		resp, body := send(t, c.method, bases[c.static]+c.path, "")
		if resp.StatusCode != c.want.status || resp.Header.Get("Location") != c.want.location ||
			(c.want.body != "" && body != c.want.body) {
			t.Errorf("static=%v %s %s = %d %q %q, want %+v", c.static, c.method, c.path,
				resp.StatusCode, resp.Header.Get("Location"), body, c.want)
		}
	}
}

func TestADirectoryWithAnIndexIsServedAsStarletteServesIt(t *testing.T) {
	base := withStatic(t)
	sub := filepath.Join(os.Getenv("KEEPSAKE_STATIC_DIR"), "sub")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "index.html"), []byte("sub"), 0o644)
	if resp, _ := send(t, http.MethodGet, base+"/sub", ""); resp.StatusCode != 307 || resp.Header.Get("Location") != "/sub/" {
		t.Fatalf("/sub = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := send(t, http.MethodGet, base+"/sub/", ""); resp.StatusCode != 200 || body != "sub" {
		t.Fatalf("/sub/ = %d %q", resp.StatusCode, body)
	}
}

// A DNS-rebound page reaches /mcp as same-origin; only its Origin header shows. The
// refusal comes before the body limit and the method checks, and /mcp/ still redirects.
func TestMCPRefusesABrowserOrigin(t *testing.T) {
	base := withStatic(t)
	for _, c := range []struct {
		method, path, origin, body string
		status                     int
	}{
		{"POST", "/mcp", "http://evil.example:8000", toolsList, 403},
		{"POST", "/mcp", "", toolsList, 403},
		{"POST", "/mcp?x=1", "x", toolsList, 403},
		{"POST", "/mcp", "x", strings.Repeat(" ", 5<<20), 403},
		{"GET", "/mcp", "x", "", 403},
		{"DELETE", "/mcp", "x", "", 403},
		{"PUT", "/mcp", "x", "", 403},
		{"POST", "/mcp/", "x", toolsList, 307},
		{"GET", "/readyz", "x", "", 200},
	} {
		req, err := http.NewRequest(c.method, base+c.path, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header["Origin"] = []string{c.origin}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s %s Origin %q = %d, want %d", c.method, c.path, c.origin, resp.StatusCode, c.status)
		}
		if c.status == 403 && (len(body) != 0 || resp.Header["Content-Type"] != nil) {
			t.Errorf("%s %s = %v, %d bytes, want Starlette's bare 403", c.method, c.path, resp.Header, len(body))
		}
	}
}
