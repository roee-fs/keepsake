// The admin console's read-only JSON API at /api, ported from 8f2af2e:src/keepsake/server/api.py.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/frontend"
	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

// openapiJSON is the contract compacted once, as FastAPI's JSONResponse renders app.openapi().
var openapiJSON = func() []byte {
	var b bytes.Buffer
	if err := json.Compact(&b, frontend.OpenAPI); err != nil {
		panic(err)
	}
	return b.Bytes()
}()

// Long enough to outlast a port-forward session, short enough to bound a leaked cookie.
const sessionTTL = 12 * time.Hour

// maxAPIBody caps every /api body, so an unauthenticated login is never buffered whole.
const maxAPIBody = 64 * 1024

// historyLimit bounds a concept's revision history, since the detail route takes no limit.
const historyLimit = 50

var (
	// Past a few hundred nodes a force layout is unreadable anyway.
	graphLimit = 500
	// Separate from the node cap: one concept can link to thousands of targets.
	graphEdgeLimit = 5000
)

type api struct {
	cs      *store.ConceptStore
	auth    *Auth
	proxies proxies
}

type route struct {
	pattern string
	serve   func(*api, http.ResponseWriter, *http.Request)
	// public routes need no session and take no slot.
	public bool
}

// routeTable is every route NewAPI registers.
var routeTable = []route{
	{"POST /session", (*api).login, true},
	{"DELETE /session", (*api).logout, false},
	{"GET /openapi.json", (*api).openapi, false},
	{"GET /tenants", (*api).tenants, false},
	{"GET /stats", (*api).stats, false},
	{"GET /stats/timeseries", (*api).timeseries, false},
	{"GET /concepts", (*api).concepts, false},
	{"GET /concepts/{path...}", (*api).concept, false},
	{"GET /search", (*api).search, false},
	{"GET /grep", (*api).grep, false},
	{"GET /graph", (*api).graph, false},
	{"GET /activity", (*api).activity, false},
}

// NewAPI serves the admin API. Mount it at /api with the prefix stripped.
func NewAPI(cs *store.ConceptStore, a *Auth) http.Handler {
	h := &api{cs: cs, auth: a, proxies: loadProxies()}
	// One, so a slow console read never holds connections an agent is waiting on.
	slot := make(chan struct{}, 1)
	mux := http.NewServeMux()
	for _, rt := range routeTable {
		serve := rt.serve
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) {
			if rt.public {
				serve(h, w, r)
				return
			}
			// Checked first, so a request refused with a 401 never queues for the slot.
			if !a.session(r) {
				writeJSON(w, r, http.StatusUnauthorized, detail{"Unauthorized"})
				return
			}
			release, ok := acquire(r.Context(), slot)
			if !ok {
				return
			}
			defer release()
			serve(h, w, r)
		})
	}
	return limitBody(maxAPIBody, mux)
}

// limitBody buffers the body before next runs, as RequestBodyLimitMiddleware does, and answers 413 past n bytes.
func limitBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > n {
			tooLarge(w)
			return
		}
		if r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, n))
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			tooLarge(w)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

type detail struct {
	Detail any `json:"detail"`
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

// internalError logs the route pattern, not the path, since a path names a concept.
func internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("api", "method", r.Method, "route", r.Pattern, "err", err)
	// Starlette's PlainTextResponse: no trailing newline, unlike http.Error.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, "Internal Server Error")
}

// reply answers v, or maps err as FastAPI does: a GrepError is a 400 and anything else a 500.
func reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	var ge *store.GrepError
	switch {
	case errors.As(err, &ge):
		writeJSON(w, r, http.StatusBadRequest, detail{ge.Msg})
	case err != nil:
		internalError(w, r, err)
	default:
		writeJSON(w, r, http.StatusOK, v)
	}
}

// params reads query parameters as FastAPI does, collecting a 422 entry per bad one.
type params struct {
	r    *http.Request
	q    url.Values
	errs []any
}

func query(r *http.Request) *params { return &params{r: r, q: r.URL.Query()} }

// fieldError is one pydantic error in FastAPI's key order; ctx only where pydantic sets it.
type fieldError struct {
	Type  string         `json:"type"`
	Loc   []any          `json:"loc"`
	Msg   string         `json:"msg"`
	Input any            `json:"input"`
	Ctx   map[string]any `json:"ctx,omitempty"`
}

func (p *params) fail(typ, name, msg string, input any, ctx map[string]any) {
	p.errs = append(p.errs, fieldError{typ, []any{"query", name}, msg, input, ctx})
}

// value takes the last of repeated values, as Starlette's QueryParams does.
func (p *params) value(name string) (string, bool) {
	vs := p.q[name]
	if len(vs) == 0 {
		return "", false
	}
	return vs[len(vs)-1], true
}

func (p *params) required(name string) string {
	v, ok := p.value(name)
	if !ok {
		p.fail("missing", name, "Field required", nil, nil)
	}
	return v
}

func (p *params) tenant(required bool) *uuid.UUID {
	v, ok := p.value("tenant")
	if !ok {
		if required {
			p.fail("missing", "tenant", "Field required", nil, nil)
		}
		return nil
	}
	id, bad := pyUUID(v)
	if bad != "" {
		p.fail("uuid_parsing", "tenant", "Input should be a valid UUID, "+bad, v, map[string]any{"error": bad})
		return nil
	}
	return &id
}

// int reads an integer in [lo, hi]; hi < 0 means no upper bound.
func (p *params) int(name string, def, lo, hi int) int {
	v, ok := p.value(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil:
		p.fail("int_parsing", name, "Input should be a valid integer, unable to parse string as an integer", v, nil)
	case n < lo:
		p.fail("greater_than_equal", name, fmt.Sprintf("Input should be greater than or equal to %d", lo), v, map[string]any{"ge": lo})
	case hi >= 0 && n > hi:
		p.fail("less_than_equal", name, fmt.Sprintf("Input should be less than or equal to %d", hi), v, map[string]any{"le": hi})
	}
	return n
}

// pyUUID parses s as pydantic does, returning the Rust uuid crate's error text on failure.
func pyUUID(s string) (uuid.UUID, string) {
	shaped := len(s) == 32 || len(s) == 36 ||
		len(s) == 38 && s[0] == '{' && s[37] == '}' ||
		len(s) == 45 && strings.HasPrefix(s, "urn:uuid:")
	if id, err := uuid.Parse(s); shaped && err == nil {
		return id, ""
	}
	body, offset, simple := s, 0, true
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		body, offset, simple = s[1:len(s)-1], 1, false
	} else if strings.HasPrefix(s, "urn:uuid:") {
		body, offset, simple = s[9:], 9, false
	}
	hyphens := 0
	var bounds [4]int
	for i, r := range body {
		switch {
		case r == '-':
			if hyphens < 4 {
				bounds[hyphens] = i
			}
			hyphens++
		case r >= 0x80 || !strings.ContainsRune("0123456789abcdefABCDEF", r):
			return uuid.Nil, fmt.Sprintf("invalid character: found `%c` at %d", r, i+offset+1)
		}
	}
	if hyphens == 0 && simple {
		return uuid.Nil, fmt.Sprintf("invalid length: expected length 32 for simple format, found %d", len(s))
	}
	if hyphens != 4 {
		return uuid.Nil, fmt.Sprintf("invalid group count: expected 5, found %d", hyphens+1)
	}
	starts, lengths := [5]int{0, 9, 14, 19, 24}, [5]int{8, 4, 4, 4, 12}
	for i := range 4 {
		if bounds[i] != starts[i+1]-1 {
			return uuid.Nil, fmt.Sprintf("invalid group length in group %d: expected %d, found %d", i, lengths[i], bounds[i]-starts[i])
		}
	}
	return uuid.Nil, fmt.Sprintf("invalid group length in group 4: expected 12, found %d", len(s)-starts[4])
}

// invalid answers 422 if any parameter failed.
func (p *params) invalid(w http.ResponseWriter) bool {
	if len(p.errs) == 0 {
		return false
	}
	writeJSON(w, p.r, http.StatusUnprocessableEntity, detail{p.errs})
	return true
}

// pyTime renders a timestamp as pydantic does: UTC as Z, microseconds only when nonzero.
type pyTime time.Time

func (t pyTime) MarshalJSON() ([]byte, error) {
	u := time.Time(t).UTC()
	s := u.Format("2006-01-02T15:04:05")
	if us := u.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return json.Marshal(s + "Z")
}

type pyDate time.Time

func (d pyDate) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(d).Format(time.DateOnly))
}

type tenantCount struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Concepts int       `json:"concepts"`
}

type summaryOut struct {
	Path        string    `json:"path"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Version     int       `json:"version"`
	UpdatedAt   pyTime    `json:"updated_at"`
	TenantID    uuid.UUID `json:"tenant_id"`
}

type conceptPage struct {
	Items []summaryOut `json:"items"`
	Total int          `json:"total"`
}

type totalsOut struct {
	Concepts  int            `json:"concepts"`
	ByType    map[string]int `json:"by_type"`
	Revisions int            `json:"revisions"`
	Links     int            `json:"links"`
	Orphans   int            `json:"orphans"`
}

type revisionOut struct {
	Path      string    `json:"path"`
	Version   int       `json:"version"`
	Op        string    `json:"op"`
	UpdatedBy string    `json:"updated_by"`
	CreatedAt pyTime    `json:"created_at"`
	TenantID  uuid.UUID `json:"tenant_id"`
}

type conceptDetail struct {
	Path        string        `json:"path"`
	Type        string        `json:"type"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Body        string        `json:"body"`
	Frontmatter *okf.Map      `json:"frontmatter"`
	Links       []string      `json:"links"`
	Version     int           `json:"version"`
	Backlinks   []string      `json:"backlinks"`
	Revisions   []revisionOut `json:"revisions"`
}

type graphNode struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Title string `json:"title"`
	// Missing marks a link target no concept holds: named by an agent, never written.
	Missing bool `json:"missing"`
}

type graphOut struct {
	Nodes     []graphNode `json:"nodes"`
	Edges     [][2]string `json:"edges"`
	Truncated bool        `json:"truncated"`
}

type dailyWrite struct {
	Date  pyDate `json:"date"`
	Count int    `json:"count"`
}

func revisions(rs []store.Revision) []revisionOut {
	out := []revisionOut{}
	for _, r := range rs {
		out = append(out, revisionOut{r.Path, r.Version, r.Op, r.UpdatedBy, pyTime(r.CreatedAt), r.TenantID})
	}
	return out
}

// credentials validates the login body as FastAPI validates Credentials, and answers any error itself.
func credentials(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		internalError(w, r, err)
		return "", false
	}
	invalid := func(typ string, loc []any, msg string, input any, ctx map[string]any) (string, bool) {
		writeJSON(w, r, http.StatusUnprocessableEntity, detail{[]fieldError{{typ, loc, msg, input, ctx}}})
		return "", false
	}
	if len(raw) == 0 {
		return invalid("missing", []any{"body"}, "Field required", nil, nil)
	}
	var body any = string(raw)
	// email.message's get_content_type, which FastAPI asks.
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if main, sub, _ := strings.Cut(ct, "/"); main == "application" && !strings.Contains(sub, "/") && (sub == "json" || strings.HasSuffix(sub, "+json")) {
		raw = bytes.TrimPrefix(raw, []byte("\uFEFF"))
		if !utf8.Valid(raw) {
			writeJSON(w, r, http.StatusBadRequest, detail{"There was an error parsing the body"})
			return "", false
		}
		v, msg, pos := pyJSON([]rune(string(raw)))
		if msg != "" {
			return invalid("json_invalid", []any{"body", pos}, "JSON decode error", okf.NewMap(), map[string]any{"error": msg})
		}
		// FastAPI treats a JSON null like no body at all.
		if v == nil {
			return invalid("missing", []any{"body"}, "Field required", nil, nil)
		}
		body = v
	}
	m, ok := body.(*okf.Map)
	if !ok {
		return invalid("model_attributes_type", []any{"body"}, "Input should be a valid dictionary or object to extract fields from", body, nil)
	}
	pw, ok := m.Get("password")
	if !ok {
		return invalid("missing", []any{"body", "password"}, "Field required", m, nil)
	}
	password, ok := pw.(string)
	if !ok {
		return invalid("string_type", []any{"body", "password"}, "Input should be a valid string", pw, nil)
	}
	return password, true
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	password, ok := credentials(w, r)
	if !ok {
		return
	}
	// The peer, not X-Forwarded-For, which any client can set.
	if !a.auth.CheckPassword(password) {
		slog.Warn("console login refused", "remote_addr", r.RemoteAddr)
		writeJSON(w, r, http.StatusUnauthorized, detail{"Unauthorized"})
		return
	}
	slog.Info("console login", "remote_addr", r.RemoteAddr)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    a.auth.Issue(sessionTTL),
		Path:     "/",
		MaxAge:   int(sessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Always-on Secure would break the port-forward flow this console exists for.
		Secure: a.proxies.https(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// proxies is uvicorn's forwarded_allow_ips: the peers trusted to set the scheme with X-Forwarded-Proto.
type proxies struct {
	all      bool
	addrs    map[netip.Addr]bool
	networks []netip.Prefix
	literals map[string]bool
}

// loadProxies parses FORWARDED_ALLOW_IPS as uvicorn 0.53's _TrustedHosts does.
func loadProxies() proxies {
	v, ok := os.LookupEnv("FORWARDED_ALLOW_IPS")
	if !ok {
		v = "127.0.0.1,::1"
	}
	p := proxies{all: v == "*", addrs: map[netip.Addr]bool{}, literals: map[string]bool{}}
	for _, h := range strings.Split(v, ",") {
		h = strings.TrimSpace(h)
		if strings.Contains(h, "/") {
			// Python's ip_network is strict: a network with host bits set is a literal.
			if n, err := netip.ParsePrefix(h); err == nil && n == n.Masked() {
				p.networks = append(p.networks, n)
				continue
			}
		} else if a, err := netip.ParseAddr(h); err == nil {
			p.addrs[a] = true
			continue
		}
		p.literals[h] = true
	}
	return p
}

func (p proxies) trusts(host string) bool {
	if p.all {
		return true
	}
	if host == "" {
		return false
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return p.literals[host]
	}
	return p.addrs[a] || slices.ContainsFunc(p.networks, func(n netip.Prefix) bool { return n.Contains(a) })
}

// https reports whether the request's scheme is https after ProxyHeadersMiddleware.
func (p proxies) https(r *http.Request) bool {
	https := r.TLS != nil
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if vs := r.Header.Values("X-Forwarded-Proto"); len(vs) > 0 && p.trusts(host) {
		switch proto := strings.TrimSpace(vs[len(vs)-1]); proto {
		case "http", "https", "ws", "wss":
			https = proto == "https"
		}
	}
	return https
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	// The login cookie's attributes, so the deletion is as locked down as the cookie.
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.proxies.https(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openapiJSON)
}

func (a *api) tenants(w http.ResponseWriter, r *http.Request) {
	ts, err := a.cs.Tenants(r.Context())
	out := []tenantCount{}
	for _, t := range ts {
		out = append(out, tenantCount{t.TenantID, t.Count})
	}
	reply(w, r, out, err)
}

func (a *api) stats(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(false)
	if p.invalid(w) {
		return
	}
	t, err := a.cs.Totals(r.Context(), tenant)
	reply(w, r, totalsOut{t.Concepts, t.ByType, t.Revisions, t.Links, t.Orphans}, err)
}

func (a *api) timeseries(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(false)
	days := p.int("days", 30, 1, 365)
	if p.invalid(w) {
		return
	}
	ds, err := a.cs.DailyWrites(r.Context(), tenant, days)
	out := []dailyWrite{}
	for _, d := range ds {
		out = append(out, dailyWrite{pyDate(d.Date), d.Count})
	}
	reply(w, r, out, err)
}

func (a *api) concepts(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(false)
	prefix, _ := p.value("prefix")
	limit := p.int("limit", 50, 1, 200)
	offset := p.int("offset", 0, 0, -1)
	if p.invalid(w) {
		return
	}
	items, total, err := a.cs.Page(r.Context(), tenant, prefix, limit, offset)
	page := conceptPage{Items: []summaryOut{}, Total: total}
	for _, s := range items {
		page.Items = append(page.Items, summaryOut{s.Path, s.Type, s.Title, s.Description, s.Version, pyTime(s.UpdatedAt), s.TenantID})
	}
	reply(w, r, page, err)
}

func (a *api) concept(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(true)
	if p.invalid(w) {
		return
	}
	path := r.PathValue("path")
	c, backlinks, history, err := a.cs.Detail(r.Context(), *tenant, path, historyLimit)
	if err != nil {
		reply(w, r, nil, err)
		return
	}
	if c == nil {
		writeJSON(w, r, http.StatusNotFound, detail{"Not Found"})
		return
	}
	reply(w, r, conceptDetail{
		c.Path, c.Type, c.Title, c.Description, c.Body, c.Frontmatter, c.Links, c.Version, backlinks, revisions(history),
	}, err)
}

func (a *api) search(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(true)
	q := p.required("q")
	limit := p.int("limit", 20, 1, 100)
	if p.invalid(w) {
		return
	}
	hits, err := a.cs.Search(r.Context(), *tenant, q, limit, nil)
	reply(w, r, convert(hits, func(h store.Hit) searchHit { return searchHit(h) }), err)
}

func (a *api) grep(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(true)
	pattern := p.required("pattern")
	limit := p.int("limit", 20, 1, 100)
	if p.invalid(w) {
		return
	}
	hits, err := a.cs.Grep(r.Context(), *tenant, pattern, limit)
	reply(w, r, convert(hits, func(h store.GrepHit) grepHit { return grepHit(h) }), err)
}

func (a *api) graph(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(true)
	if p.invalid(w) {
		return
	}
	rows, err := a.cs.Graph(r.Context(), *tenant, graphLimit+1)
	if err != nil {
		reply(w, r, nil, err)
		return
	}
	reply(w, r, buildGraph(rows), nil)
}

func buildGraph(rows []store.GraphRow) graphOut {
	g := graphOut{Nodes: []graphNode{}, Edges: [][2]string{}, Truncated: len(rows) > graphLimit}
	rows = rows[:min(len(rows), graphLimit)]
	known := map[string]bool{}
	for _, row := range rows {
		known[row.Path] = true
		g.Nodes = append(g.Nodes, graphNode{Path: row.Path, Type: row.Type, Title: row.Title})
	}
	// Past the row cap, an absent target may just be a concept that did not fit.
	rowsCut := g.Truncated
links:
	for _, row := range rows {
		for _, target := range row.Links {
			if len(g.Edges) >= graphEdgeLimit {
				g.Truncated = true
				break links
			}
			if !known[target] {
				if rowsCut {
					continue
				}
				if len(g.Nodes) >= graphLimit {
					g.Truncated = true
					continue
				}
				known[target] = true
				g.Nodes = append(g.Nodes, graphNode{Path: target, Missing: true})
			}
			g.Edges = append(g.Edges, [2]string{row.Path, target})
		}
	}
	return g
}

func (a *api) activity(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(false)
	limit := p.int("limit", 50, 1, 200)
	if p.invalid(w) {
		return
	}
	rs, err := a.cs.Activity(r.Context(), tenant, limit)
	reply(w, r, revisions(rs), err)
}
