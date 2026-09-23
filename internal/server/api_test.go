// Ported from tests/test_api.py. The build_app tests there (UI disabled, static
// bundle) belong to the serve wiring.
package server

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

const adminPassword = "test-admin-password"

type console struct {
	t      *testing.T
	h      http.Handler
	cs     *store.ConceptStore
	cookie string
}

func newConsole(t *testing.T) *console {
	t.Helper()
	cs := conceptStore(t)
	return &console{t: t, h: NewAPI(cs, NewAuth(adminPassword)), cs: cs}
}

func (c *console) do(r *http.Request) *httptest.ResponseRecorder {
	if c.cookie != "" {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: c.cookie})
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, r)
	return rec
}

func (c *console) request(method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return c.do(r)
}

func (c *console) get(target string, params ...string) *httptest.ResponseRecorder {
	q := url.Values{}
	for i := 0; i < len(params); i += 2 {
		q.Set(params[i], params[i+1])
	}
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	return c.request(http.MethodGet, target, "")
}

func (c *console) login() *console {
	c.t.Helper()
	rec := c.request(http.MethodPost, "/session", `{"password": "`+adminPassword+`"}`)
	if rec.Code != http.StatusNoContent {
		c.t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	c.cookie = sessionCookie(c.t, rec).Value
	return c
}

func (c *console) create(tenant uuid.UUID, path, title string, links ...string) {
	c.t.Helper()
	if _, _, err := c.cs.Create(ctx, tenant, okf.Concept{Path: path, Type: "Concept", Title: title, Links: links}, "test"); err != nil {
		c.t.Fatal(err)
	}
}

// seeded is a fresh tenant holding one concept.
func (c *console) seeded() uuid.UUID {
	tenant := uuid.New()
	c.create(tenant, "detect/dormant", "Dormant Rule")
	return tenant
}

func (c *console) otherTenant() uuid.UUID {
	tenant := uuid.New()
	c.create(tenant, "other/thing", "Other")
	return tenant
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == cookieName {
			return ck
		}
	}
	t.Fatalf("no %s cookie in %v", cookieName, rec.Header())
	return nil
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("status %d, body %q: %v", rec.Code, rec.Body, err)
	}
	return v
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d: %s", rec.Code, code, rec.Body)
	}
}

func setLimit(t *testing.T, p *int, v int) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

func TestEveryRouteExceptLoginRequiresASession(t *testing.T) {
	c := newConsole(t)
	// The floor keeps an empty route table from passing vacuously.
	if len(routeTable) < 12 {
		t.Fatalf("only %d routes registered", len(routeTable))
	}
	for _, rt := range routeTable {
		if rt.pattern == "POST /session" {
			continue
		}
		method, path, _ := strings.Cut(rt.pattern, " ")
		for _, cookie := range []string{"", "4102444800.forged"} {
			c.cookie = cookie
			rec := c.request(method, pathParam.ReplaceAllString(path, "x"), "")
			if rec.Code != http.StatusUnauthorized || rec.Body.String() != `{"detail":"Unauthorized"}` {
				t.Errorf("%s with cookie %q was not guarded: %d %s", rt.pattern, cookie, rec.Code, rec.Body)
			}
		}
	}
}

func TestABadPasswordIsRejectedAndSetsNoCookie(t *testing.T) {
	rec := newConsole(t).request(http.MethodPost, "/session", `{"password": "wrong"}`)
	wantStatus(t, rec, http.StatusUnauthorized)
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("a rejected login set %v", rec.Result().Cookies())
	}
}

func TestAMalformedLoginIsAValidationError(t *testing.T) {
	c := newConsole(t)
	for _, body := range []string{"", "{", `[]`, `null`, `{}`, `{"password": 1}`, `{"password": null}`} {
		if rec := c.request(http.MethodPost, "/session", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("login with %q = %d, want 422", body, rec.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{"password": "`+adminPassword+`"}`))
	r.Header.Set("Content-Type", "text/plain")
	wantStatus(t, c.do(r), http.StatusUnprocessableEntity)
}

func TestAGoodPasswordSetsAnHttpOnlyStrictCookie(t *testing.T) {
	rec := newConsole(t).request(http.MethodPost, "/session", `{"password": "`+adminPassword+`"}`)
	wantStatus(t, rec, http.StatusNoContent)
	ck := sessionCookie(t, rec)
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Secure || ck.Path != "/" || ck.MaxAge != 43200 {
		t.Fatalf("cookie = %s", rec.Header().Get("Set-Cookie"))
	}
}

func TestAGoodPasswordSetsSecureOverTLS(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{"password": "`+adminPassword+`"}`))
	r.Header.Set("Content-Type", "application/json")
	r.TLS = &tls.ConnectionState{}
	rec := newConsole(t).do(r)
	wantStatus(t, rec, http.StatusNoContent)
	if !sessionCookie(t, rec).Secure {
		t.Fatalf("cookie = %s", rec.Header().Get("Set-Cookie"))
	}
}

func TestLogoutClearsTheCookie(t *testing.T) {
	rec := newConsole(t).login().request(http.MethodDelete, "/session", "")
	wantStatus(t, rec, http.StatusNoContent)
	if ck := sessionCookie(t, rec); ck.MaxAge >= 0 || ck.Value != "" {
		t.Fatalf("cookie = %s", rec.Header().Get("Set-Cookie"))
	}
}

func TestOpenAPISchemaIsServedBehindTheSessionGuard(t *testing.T) {
	rec := newConsole(t).login().get("/openapi.json")
	wantStatus(t, rec, http.StatusOK)
	if _, ok := decode[map[string]any](t, rec)["openapi"]; !ok {
		t.Fatalf("no openapi key in %s", rec.Body)
	}
}

func TestDocsUIIsServedBehindTheSessionGuard(t *testing.T) {
	rec := newConsole(t).login().get("/docs")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "url: 'openapi.json'") {
		t.Fatalf("docs = %s %s", rec.Header().Get("Content-Type"), rec.Body)
	}
}

func TestTenantsListsEveryTenantWithAConcept(t *testing.T) {
	c := newConsole(t).login()
	seeded := c.seeded()
	rec := c.get("/tenants")
	wantStatus(t, rec, http.StatusOK)
	want := map[string]any{"tenant_id": seeded.String(), "concepts": 1.0}
	if !slices.ContainsFunc(decode[[]map[string]any](t, rec), func(m map[string]any) bool { return reflect.DeepEqual(m, want) }) {
		t.Fatalf("%v not in %s", want, rec.Body)
	}
}

func TestStatsTotalsAnAbsentTenantCoversEveryTenant(t *testing.T) {
	c := newConsole(t).login()
	c.seeded()
	rec := c.get("/stats")
	wantStatus(t, rec, http.StatusOK)
	if n := decode[map[string]any](t, rec)["concepts"].(float64); n < 1 {
		t.Fatalf("concepts = %v", n)
	}
}

func TestStatsScopedToOneTenant(t *testing.T) {
	c := newConsole(t).login()
	rec := c.get("/stats", "tenant", c.seeded().String())
	wantStatus(t, rec, http.StatusOK)
	got := decode[map[string]any](t, rec)
	want := map[string]any{"concepts": 1.0, "by_type": map[string]any{"Concept": 1.0}, "revisions": 1.0, "links": 0.0, "orphans": 1.0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stats = %v", got)
	}
}

func TestStatsRejectsAMalformedTenant(t *testing.T) {
	wantStatus(t, newConsole(t).login().get("/stats", "tenant", "not-a-uuid"), http.StatusUnprocessableEntity)
}

func TestStatsTimeseriesReturnsAThirtyDayDefault(t *testing.T) {
	c := newConsole(t).login()
	rec := c.get("/stats/timeseries", "tenant", c.seeded().String())
	wantStatus(t, rec, http.StatusOK)
	days := decode[[]map[string]any](t, rec)
	if len(days) != 30 {
		t.Fatalf("%d days", len(days))
	}
	if today := time.Now().UTC().Format(time.DateOnly); days[29]["date"] != today || days[29]["count"] != 1.0 {
		t.Fatalf("last day = %v, want %s with 1 write", days[29], today)
	}
}

func TestConceptsPageListsASeededConcept(t *testing.T) {
	c := newConsole(t).login()
	seeded := c.seeded()
	rec := c.get("/concepts", "tenant", seeded.String())
	wantStatus(t, rec, http.StatusOK)
	page := decode[struct {
		Items []map[string]any
		Total int
	}](t, rec)
	if page.Total != 1 || page.Items[0]["path"] != "detect/dormant" || page.Items[0]["tenant_id"] != seeded.String() {
		t.Fatalf("page = %s", rec.Body)
	}
}

func TestQueryBoundsAreValidationErrors(t *testing.T) {
	c := newConsole(t).login()
	tenant := c.seeded().String()
	for _, q := range [][]string{
		{"/concepts", "offset", "-1"},
		{"/concepts", "limit", "-1"},
		{"/concepts", "limit", "201"},
		{"/concepts", "limit", "ten"},
		{"/stats/timeseries", "days", "1000000"},
		{"/stats/timeseries", "days", "0"},
		{"/activity", "limit", "0"},
		{"/search", "limit", "101", "q", "x"},
		{"/grep", "limit", "0", "pattern", "x"},
	} {
		rec := c.get(q[0], append([]string{"tenant", tenant}, q[1:]...)...)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%v = %d, want 422", q, rec.Code)
		}
		if _, ok := decode[map[string]any](t, rec)["detail"].([]any); !ok {
			t.Errorf("%v: detail is not a list: %s", q, rec.Body)
		}
	}
}

func TestConceptDetailCarriesBacklinksAndHistory(t *testing.T) {
	c := newConsole(t).login()
	rec := c.get("/concepts/detect/dormant", "tenant", c.seeded().String())
	wantStatus(t, rec, http.StatusOK)
	body := decode[map[string]any](t, rec)
	revisions := body["revisions"].([]any)
	if body["path"] != "detect/dormant" || len(body["backlinks"].([]any)) != 0 || len(revisions) != 1 ||
		revisions[0].(map[string]any)["op"] != "create" {
		t.Fatalf("detail = %s", rec.Body)
	}
}

func TestConceptDetailUnescapesAnEncodedPath(t *testing.T) {
	c := newConsole(t).login()
	wantStatus(t, c.get("/concepts/detect%2Fdormant", "tenant", c.seeded().String()), http.StatusOK)
}

func TestConceptDetailMissingPathIs404(t *testing.T) {
	c := newConsole(t).login()
	rec := c.get("/concepts/no/such", "tenant", c.seeded().String())
	wantStatus(t, rec, http.StatusNotFound)
	if rec.Body.String() != `{"detail":"Not Found"}` {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestConceptDetailRequiresATenant(t *testing.T) {
	wantStatus(t, newConsole(t).login().get("/concepts/detect/dormant"), http.StatusUnprocessableEntity)
}

func TestSearchFindsASeededConcept(t *testing.T) {
	c := newConsole(t).login()
	rec := c.get("/search", "tenant", c.seeded().String(), "q", "dormant")
	wantStatus(t, rec, http.StatusOK)
	if hits := decode[[]map[string]any](t, rec); hits[0]["path"] != "detect/dormant" {
		t.Fatalf("hits = %s", rec.Body)
	}
}

func TestSearchAndGrepRequireTheirQuery(t *testing.T) {
	c := newConsole(t).login()
	tenant := c.seeded().String()
	wantStatus(t, c.get("/search", "tenant", tenant), http.StatusUnprocessableEntity)
	wantStatus(t, c.get("/grep", "tenant", tenant), http.StatusUnprocessableEntity)
	wantStatus(t, c.get("/search", "q", "dormant"), http.StatusUnprocessableEntity)
}

func TestGrepFindsASeededConcept(t *testing.T) {
	c := newConsole(t).login()
	rec := c.get("/grep", "tenant", c.seeded().String(), "pattern", "Dormant")
	wantStatus(t, rec, http.StatusOK)
	if hits := decode[[]map[string]any](t, rec); hits[0]["path"] != "detect/dormant" {
		t.Fatalf("hits = %s", rec.Body)
	}
}

func TestGrepRejectsAnUnusablePattern(t *testing.T) {
	c := newConsole(t).login()
	rec := c.get("/grep", "tenant", c.seeded().String(), "pattern", "(")
	wantStatus(t, rec, http.StatusBadRequest)
	if got := decode[map[string]any](t, rec)["detail"]; got != "unusable regular expression: '('" {
		t.Fatalf("detail = %v", got)
	}
}

func TestActivityListsTheSeededRevision(t *testing.T) {
	c := newConsole(t).login()
	rec := c.get("/activity", "tenant", c.seeded().String())
	wantStatus(t, rec, http.StatusOK)
	if revs := decode[[]map[string]any](t, rec); revs[0]["path"] != "detect/dormant" {
		t.Fatalf("activity = %s", rec.Body)
	}
}

func TestAdminScopeMixesEveryTenantWhenNoneIsNamed(t *testing.T) {
	c := newConsole(t).login()
	c.seeded()
	c.otherTenant()
	for route, items := range map[string]func(*httptest.ResponseRecorder) []map[string]any{
		"/concepts": func(rec *httptest.ResponseRecorder) []map[string]any {
			return decode[struct{ Items []map[string]any }](t, rec).Items
		},
		"/activity": func(rec *httptest.ResponseRecorder) []map[string]any { return decode[[]map[string]any](t, rec) },
	} {
		rec := c.get(route, "limit", "200")
		wantStatus(t, rec, http.StatusOK)
		var paths []string
		for _, it := range items(rec) {
			paths = append(paths, it["path"].(string))
		}
		if !slices.Contains(paths, "detect/dormant") || !slices.Contains(paths, "other/thing") {
			t.Errorf("%s did not mix tenants: %v", route, paths)
		}
	}
}

func TestStatsTimeseriesAdminScopeMixesEveryTenant(t *testing.T) {
	c := newConsole(t).login()
	seeded := c.seeded()
	c.otherTenant()
	sum := func(rec *httptest.ResponseRecorder) (n float64) {
		for _, d := range decode[[]map[string]any](t, rec) {
			n += d["count"].(float64)
		}
		return n
	}
	if admin, scoped := sum(c.get("/stats/timeseries")), sum(c.get("/stats/timeseries", "tenant", seeded.String())); admin <= scoped {
		t.Fatalf("admin %v <= scoped %v", admin, scoped)
	}
}

type graphBody struct {
	Nodes []struct {
		Path    string
		Missing bool
	}
	Edges     [][2]string
	Truncated bool
}

func (g graphBody) paths() []string {
	var out []string
	for _, n := range g.Nodes {
		out = append(out, n.Path)
	}
	return out
}

func (c *console) graph(tenant uuid.UUID) graphBody {
	rec := c.get("/graph", "tenant", tenant.String())
	wantStatus(c.t, rec, http.StatusOK)
	return decode[graphBody](c.t, rec)
}

func TestGraphReturnsEdgesAndMarksAMissingTarget(t *testing.T) {
	c := newConsole(t).login()
	tenant := uuid.New()
	c.otherTenant()
	c.create(tenant, "a", "a", "b", "ghost")
	c.create(tenant, "b", "b", "a")

	g := c.graph(tenant)
	nodes := map[string]bool{}
	for _, n := range g.Nodes {
		nodes[n.Path] = n.Missing
	}
	// other/thing belongs to another tenant and MUST NOT appear.
	if !reflect.DeepEqual(nodes, map[string]bool{"a": false, "b": false, "ghost": true}) {
		t.Fatalf("nodes = %v", nodes)
	}
	if !reflect.DeepEqual(g.Edges, [][2]string{{"a", "b"}, {"a", "ghost"}, {"b", "a"}}) || g.Truncated {
		t.Fatalf("graph = %+v", g)
	}
}

func TestGraphPastTheCapDropsEdgesItCannotPlace(t *testing.T) {
	setLimit(t, &graphLimit, 1)
	c := newConsole(t).login()
	tenant := uuid.New()
	c.create(tenant, "a", "a", "b")
	c.create(tenant, "b", "b")

	g := c.graph(tenant)
	// "b" exists past the cap, so it MUST NOT come back as a missing node.
	if !reflect.DeepEqual(g.paths(), []string{"a"}) || len(g.Edges) != 0 || !g.Truncated {
		t.Fatalf("graph = %+v", g)
	}
}

func TestGraphCapsMissingTargetsAtTheNodeLimit(t *testing.T) {
	setLimit(t, &graphLimit, 3)
	c := newConsole(t).login()
	tenant := uuid.New()
	c.create(tenant, "a", "a", "g1", "g2", "g3", "g4")

	g := c.graph(tenant)
	if !reflect.DeepEqual(g.paths(), []string{"a", "g1", "g2"}) ||
		!reflect.DeepEqual(g.Edges, [][2]string{{"a", "g1"}, {"a", "g2"}}) || !g.Truncated {
		t.Fatalf("graph = %+v", g)
	}
}

func TestGraphCapsEdges(t *testing.T) {
	setLimit(t, &graphEdgeLimit, 2)
	c := newConsole(t).login()
	tenant := uuid.New()
	c.create(tenant, "a", "a", "b", "c")
	c.create(tenant, "b", "b", "a", "c")
	c.create(tenant, "c", "c")

	if g := c.graph(tenant); len(g.Edges) != 2 || !g.Truncated {
		t.Fatalf("graph = %+v", g)
	}
}

func TestGraphRequiresATenant(t *testing.T) {
	wantStatus(t, newConsole(t).login().get("/graph"), http.StatusUnprocessableEntity)
}

func TestTimestampsAreSerializedAsPydanticDoes(t *testing.T) {
	for in, want := range map[time.Time]string{
		time.Date(2026, 1, 2, 3, 4, 5, 12000, time.UTC):                     "2026-01-02T03:04:05.000012Z",
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC):                         "2026-01-02T03:04:05Z",
		time.Date(2026, 1, 2, 5, 4, 5, 999999999, time.FixedZone("", 7200)): "2026-01-02T03:04:05.999999Z",
	} {
		if got, _ := json.Marshal(pyTime(in)); string(got) != `"`+want+`"` {
			t.Errorf("pyTime(%v) = %s, want %s", in, got, want)
		}
	}
}

// Python's Secure decision reads the scheme uvicorn's ProxyHeadersMiddleware sets.
func TestSecureFollowsUvicornsProxyHeaderRule(t *testing.T) {
	for _, tc := range []struct {
		name, allow, peer, proto string
		tls, secure              bool
	}{
		{"loopback proxy says https", "", "127.0.0.1:5000", "https", false, true},
		{"ipv6 loopback proxy says https", "", "[::1]:5000", "https", false, true},
		{"untrusted peer says https", "", "10.1.2.3:5000", "https", false, false},
		{"everyone trusted", "*", "10.1.2.3:5000", "https", false, true},
		{"trusted network", "192.168.0.0/16, 10.0.0.0/8", "10.1.2.3:5000", "https", false, true},
		{"non-strict network is a literal", "10.0.0.1/8", "10.1.2.3:5000", "https", false, false},
		{"last header wins and is stripped", "", "127.0.0.1:5000", "http| https ", false, true},
		{"a list is not a scheme", "", "127.0.0.1:5000", "https, http", false, false},
		{"trusted proxy downgrades tls", "", "127.0.0.1:5000", "http", true, false},
		{"plain request", "", "127.0.0.1:5000", "", false, false},
		{"tls without a proxy", "", "10.1.2.3:5000", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.allow == "" {
				t.Setenv("FORWARDED_ALLOW_IPS", "")
				os.Unsetenv("FORWARDED_ALLOW_IPS")
			} else {
				t.Setenv("FORWARDED_ALLOW_IPS", tc.allow)
			}
			r := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(`{"password": "`+adminPassword+`"}`))
			r.Header.Set("Content-Type", "application/json")
			r.RemoteAddr = tc.peer
			for _, p := range strings.Split(tc.proto, "|") {
				if p != "" {
					r.Header.Add("X-Forwarded-Proto", p)
				}
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			rec := newConsole(t).do(r)
			wantStatus(t, rec, http.StatusNoContent)
			if got := sessionCookie(t, rec).Secure; got != tc.secure {
				t.Fatalf("Secure = %v, want %v: %s", got, tc.secure, rec.Header().Get("Set-Cookie"))
			}
		})
	}
}
