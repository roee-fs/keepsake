package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const toolsList = `{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}`

func jwtEnv(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(strings.Repeat("s", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEEPSAKE_AUTH_MODE", "jwt")
	t.Setenv("KEEPSAKE_JWT_ISSUER", "platform")
	t.Setenv("KEEPSAKE_JWT_AUDIENCE", "keepsake")
	t.Setenv("KEEPSAKE_JWT_SECRET_FILE", path)
	t.Setenv("KEEPSAKE_TENANT_ID", "")
}

func postMCP(h http.Handler, token string) int {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(toolsList))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestServeInJWTModeAcceptsOnlyATokenFromKeepsakeToken(t *testing.T) {
	jwtEnv(t)
	served := fakeListen(t)
	if code, _, stderr := run(t, "serve", "--dsn", db.AppDSN); code != 0 {
		t.Fatalf("serve exit %d: %s", code, stderr)
	}
	code, token, stderr := run(t, "token", "--tenant", uuid.NewString(), "--sub", "alice", "--ttl", "1h")
	if code != 0 {
		t.Fatalf("token exit %d: %s", code, stderr)
	}
	if got := postMCP(served.h, ""); got != http.StatusUnauthorized {
		t.Fatalf("no token: %d", got)
	}
	if got := postMCP(served.h, strings.TrimSpace(token)); got != http.StatusOK {
		t.Fatalf("minted token: %d", got)
	}
}

func TestServeRefusesATenantInJWTMode(t *testing.T) {
	jwtEnv(t)
	fakeListen(t)
	code, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.NewString())
	if code != 1 || !strings.Contains(stderr, "--tenant") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestServeRefusesTheNilTenant(t *testing.T) {
	fakeListen(t)
	code, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.Nil.String())
	if code != 1 || !strings.Contains(stderr, "nil uuid") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestServeRefusesAnUnknownAuthMode(t *testing.T) {
	t.Setenv("KEEPSAKE_AUTH_MODE", "proxy")
	fakeListen(t)
	code, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.NewString())
	if code != 1 || !strings.Contains(stderr, "KEEPSAKE_AUTH_MODE") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestTokenNeedsATenantASubjectAndADuration(t *testing.T) {
	jwtEnv(t)
	for _, args := range [][]string{
		{"token", "--sub", "alice"},
		{"token", "--tenant", uuid.NewString()},
		{"token", "--tenant", uuid.NewString(), "--sub", "alice", "--ttl", "soon"},
		{"token", "--tenant", uuid.NewString(), "--sub", "alice", "--ttl", "500ms"},
		{"token", "--tenant", uuid.Nil.String(), "--sub", "alice"},
	} {
		if code, stdout, stderr := run(t, args...); code != 1 || stdout != "" {
			t.Errorf("%v: exit %d: %q %q", args, code, stdout, stderr)
		}
	}
}
