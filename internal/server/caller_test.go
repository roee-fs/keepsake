package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	key     = []byte(strings.Repeat("k", 32))
	rotated = []byte(strings.Repeat("r", 32))
	issuer  = &JWT{Issuer: "platform", Audience: "keepsake", Secrets: [][]byte{key, rotated}}
)

// echoCaller answers with the caller the middleware bound, so a test sees exactly what a tool would.
var echoCaller = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	c, ok := r.Context().Value(callerKey{}).(caller)
	if !ok {
		http.Error(w, "no caller", http.StatusInternalServerError)
		return
	}
	w.Write([]byte(c.tenant.String() + " " + c.actor))
})

func sign(secret []byte, header, claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	unsigned := enc(header) + "." + enc(claims)
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func claims(tenant string, mutate func(map[string]any)) map[string]any {
	now := time.Now().Unix()
	c := map[string]any{"iss": "platform", "aud": "keepsake", "sub": "run:1", "iat": now, "exp": now + 60,
		"tctx": map[string]any{"tenant": tenant}}
	if mutate != nil {
		mutate(c)
	}
	return c
}

var hs256 = map[string]any{"alg": "HS256", "typ": "JWT"}

func call(t *testing.T, authorization ...string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	for _, a := range authorization {
		req.Header.Add("Authorization", a)
	}
	rec := httptest.NewRecorder()
	issuer.Middleware(echoCaller).ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

func TestAValidTokenBindsItsTenantAndSubject(t *testing.T) {
	tenant := uuid.New()
	code, _, body := call(t, "Bearer "+issuer.Mint(tenant, "run:42", time.Minute))
	if code != 200 || body != tenant.String()+" run:42" {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestEverySecretInTheFileVerifies(t *testing.T) {
	tenant := uuid.New()
	code, _, body := call(t, "Bearer "+sign(rotated, hs256, claims(tenant.String(), nil)))
	if code != 200 || body != tenant.String()+" run:1" {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestAnAudienceListContainingKeepsakeIsAccepted(t *testing.T) {
	tok := sign(key, hs256, claims(uuid.NewString(), func(c map[string]any) { c["aud"] = []string{"other", "keepsake"} }))
	if code, _, body := call(t, "Bearer "+tok); code != 200 {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestAMinterClockAFewSecondsOffIsTolerated(t *testing.T) {
	now := time.Now().Unix()
	for name, mutate := range map[string]func(map[string]any){
		"minter behind": func(c map[string]any) { c["exp"] = now - 10 },
		"minter ahead":  func(c map[string]any) { c["nbf"] = now + 10 },
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, body := call(t, "Bearer "+sign(key, hs256, claims(uuid.NewString(), mutate))); code != 200 {
				t.Fatalf("got %d %q", code, body)
			}
		})
	}
}

func TestAnInvalidTokenIsUnauthorized(t *testing.T) {
	tenant := uuid.NewString()
	valid := sign(key, hs256, claims(tenant, nil))
	parts := strings.Split(valid, ".")
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"platform","aud":"keepsake","sub":"run:1","exp":9999999999,"tctx":{"tenant":"`+uuid.NewString()+`"}}`)) + "." + parts[2]
	without := func(k string) func(map[string]any) { return func(c map[string]any) { delete(c, k) } }
	set := func(k string, v any) func(map[string]any) { return func(c map[string]any) { c[k] = v } }
	cases := map[string][]string{
		"no header":             nil,
		"not bearer":            {"Basic " + valid},
		"empty bearer":          {"Bearer "},
		"two headers":           {"Bearer " + valid, "Bearer " + valid},
		"two segments":          {"Bearer " + parts[0] + "." + parts[1]},
		"wrong secret":          {"Bearer " + sign([]byte(strings.Repeat("x", 32)), hs256, claims(tenant, nil))},
		"tampered payload":      {"Bearer " + tampered},
		"alg none":              {"Bearer " + base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."},
		"alg HS512":             {"Bearer " + sign(key, map[string]any{"alg": "HS512"}, claims(tenant, nil))},
		"wrong issuer":          {"Bearer " + sign(key, hs256, claims(tenant, set("iss", "other")))},
		"wrong audience":        {"Bearer " + sign(key, hs256, claims(tenant, set("aud", "other")))},
		"no audience":           {"Bearer " + sign(key, hs256, claims(tenant, without("aud")))},
		"expired":               {"Bearer " + sign(key, hs256, claims(tenant, set("exp", time.Now().Unix()-120)))},
		"no expiry":             {"Bearer " + sign(key, hs256, claims(tenant, without("exp")))},
		"not yet valid":         {"Bearer " + sign(key, hs256, claims(tenant, set("nbf", time.Now().Unix()+60)))},
		"no subject":            {"Bearer " + sign(key, hs256, claims(tenant, without("sub")))},
		"non-string subject":    {"Bearer " + sign(key, hs256, claims(tenant, set("sub", 7)))},
		"non-numeric expiry":    {"Bearer " + sign(key, hs256, claims(tenant, set("exp", "tomorrow")))},
		"payload not an object": {"Bearer " + parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`[]`)) + "." + parts[2]},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			code, h, body := call(t, headers...)
			if code != http.StatusUnauthorized || h.Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("got %d %q %q", code, h.Get("WWW-Authenticate"), body)
			}
			if strings.Contains(body, tenant) {
				t.Fatalf("the 401 names the tenant: %q", body)
			}
		})
	}
}

func TestAVerifiedCallerWithoutAUsableTenantIsForbidden(t *testing.T) {
	cases := map[string]func(map[string]any){
		"no tctx":         func(c map[string]any) { delete(c, "tctx") },
		"no tenant":       func(c map[string]any) { c["tctx"] = map[string]any{} },
		"not a uuid":      func(c map[string]any) { c["tctx"] = map[string]any{"tenant": "team-a"} },
		"not a string":    func(c map[string]any) { c["tctx"] = map[string]any{"tenant": 7} },
		"the nil uuid":    func(c map[string]any) { c["tctx"] = map[string]any{"tenant": uuid.Nil.String()} },
		"tctx not object": func(c map[string]any) { c["tctx"] = "x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if code, _, body := call(t, "Bearer "+sign(key, hs256, claims("", mutate))); code != http.StatusForbidden {
				t.Fatalf("got %d %q", code, body)
			}
		})
	}
}

func TestFixedTenantBindsEveryRequestToOneTenantAsMCP(t *testing.T) {
	tenant := uuid.New()
	rec := httptest.NewRecorder()
	FixedTenant(tenant)(echoCaller).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != 200 || rec.Body.String() != tenant.String()+" mcp" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func secretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJWTFromEnvReadsEverySecretLine(t *testing.T) {
	t.Setenv("KEEPSAKE_JWT_ISSUER", "platform")
	t.Setenv("KEEPSAKE_JWT_AUDIENCE", "keepsake")
	t.Setenv("KEEPSAKE_JWT_SECRET_FILE", secretFile(t, string(key)+"\n\n"+string(rotated)+"\n"))
	j, err := JWTFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if j.Issuer != "platform" || j.Audience != "keepsake" || len(j.Secrets) != 2 || string(j.Secrets[1]) != string(rotated) {
		t.Fatalf("got %+v", j)
	}
}

func TestJWTFromEnvRefusesAnIncompleteConfiguration(t *testing.T) {
	good := map[string]string{"KEEPSAKE_JWT_ISSUER": "platform", "KEEPSAKE_JWT_AUDIENCE": "keepsake"}
	cases := map[string]struct {
		unset  string
		secret string
	}{
		"no issuer":    {unset: "KEEPSAKE_JWT_ISSUER", secret: string(key)},
		"no audience":  {unset: "KEEPSAKE_JWT_AUDIENCE", secret: string(key)},
		"empty file":   {secret: "\n"},
		"short secret": {secret: string(key) + "\nshort\n"},
		"no file":      {},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range good {
				if k != c.unset {
					t.Setenv(k, v)
				} else {
					t.Setenv(k, "")
				}
			}
			path := filepath.Join(t.TempDir(), "missing")
			if c.secret != "" || name == "empty file" {
				path = secretFile(t, c.secret)
			}
			t.Setenv("KEEPSAKE_JWT_SECRET_FILE", path)
			if _, err := JWTFromEnv(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// asTenant connects to one shared server as tenant, the way a platform run would.
func asTenant(t *testing.T, url string, tenant uuid.UUID, sub string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "keepsake-test"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer(issuer.Mint(tenant, sub, time.Minute))}}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestOneServerKeepsConcurrentTenantsApart(t *testing.T) {
	cs := conceptStore(t)
	srv := httptest.NewServer(issuer.Middleware(NewMCPHandler(NewTools(cs, uuid.Nil, actor))))
	t.Cleanup(srv.Close)
	tenants := []uuid.UUID{uuid.New(), uuid.New()}
	sessions := []*mcp.ClientSession{asTenant(t, srv.URL, tenants[0], "run:0"), asTenant(t, srv.URL, tenants[1], "run:1")}
	var wg sync.WaitGroup
	for i, tenant := range tenants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			say := func(p *mcp.CallToolParams) string {
				res, err := sessions[i].CallTool(ctx, p)
				if err != nil || res.IsError {
					t.Errorf("tenant %d %s: %v %+v", i, p.Name, err, res)
					return ""
				}
				return res.Content[0].(*mcp.TextContent).Text
			}
			body := fmt.Sprintf("owned by %s", tenant)
			say(&mcp.CallToolParams{Name: "okf_create", Arguments: map[string]any{"path": "shared/note", "type": "Concept", "body": body}})
			if got := say(&mcp.CallToolParams{Name: "okf_read", Arguments: map[string]any{"path": "shared/note"}}); !strings.Contains(got, body) {
				t.Errorf("tenant %d read %s", i, got)
			}
			other := tenants[1-i].String()
			for _, tool := range []mcp.CallToolParams{
				{Name: "okf_search", Arguments: map[string]any{"query": "owned", "limit": 10}},
				{Name: "okf_grep", Arguments: map[string]any{"pattern": "owned", "limit": 10}},
				{Name: "okf_list", Arguments: map[string]any{}},
			} {
				if got := say(&tool); strings.Contains(got, other) {
					t.Errorf("tenant %d saw the other tenant through %s: %s", i, tool.Name, got)
				}
			}
		}()
	}
	wg.Wait()
	for i, tenant := range tenants {
		revs, err := cs.Activity(ctx, &tenant, 10)
		if err != nil || len(revs) != 1 || revs[0].UpdatedBy != fmt.Sprintf("run:%d", i) {
			t.Fatalf("tenant %d revisions %+v, %v", i, revs, err)
		}
	}
}

func TestAToolCallWithNoTenantWritesNothing(t *testing.T) {
	srv := httptest.NewServer(NewMCPHandler(NewTools(conceptStore(t), uuid.Nil, actor)))
	t.Cleanup(srv.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "keepsake-test"}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "okf_create", Arguments: map[string]any{"path": "orphan", "type": "Concept"}}); err == nil {
		t.Fatal("a tool ran with no tenant")
	}
}
