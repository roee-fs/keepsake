// The read-only JSON API the admin console calls, mounted at /api. Ported from
// src/keepsake/server/api.py; frontend/openapi.json is its contract.
package server

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

//go:generate cp ../../frontend/openapi.json openapi.json
//go:embed openapi.json
var openapiJSON []byte

// docsHTML is fastapi.openapi.docs.get_swagger_ui_html(openapi_url="openapi.json",
// title="keepsake API") as FastAPI 0.141.1 rendered it.
//
//go:embed docs.html
var docsHTML []byte

// Long enough to outlast a port-forward session, short enough to bound a leaked cookie.
const sessionTTL = 12 * time.Hour

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
}

// routeTable is every route NewAPI registers. Only POST /session is unguarded.
var routeTable = []route{
	{"POST /session", (*api).login},
	{"DELETE /session", (*api).logout},
	{"GET /openapi.json", (*api).openapi},
	{"GET /docs", (*api).docs},
	{"GET /tenants", (*api).tenants},
	{"GET /stats", (*api).stats},
	{"GET /stats/timeseries", (*api).timeseries},
	{"GET /concepts", (*api).concepts},
	{"GET /concepts/{path...}", (*api).concept},
	{"GET /search", (*api).search},
	{"GET /grep", (*api).grep},
	{"GET /graph", (*api).graph},
	{"GET /activity", (*api).activity},
}

// NewAPI serves the admin API. Mount it at /api with the prefix stripped.
func NewAPI(cs *store.ConceptStore, a *Auth) http.Handler {
	h := &api{cs: cs, auth: a, proxies: loadProxies()}
	mux := http.NewServeMux()
	for _, rt := range routeTable {
		serve := rt.serve
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) {
			if rt.pattern != "POST /session" && !a.session(r) {
				writeJSON(w, http.StatusUnauthorized, detail{"Unauthorized"})
				return
			}
			serve(h, w, r)
		})
	}
	return mux
}

type detail struct {
	Detail any `json:"detail"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

func internalError(w http.ResponseWriter, err error) {
	log.Printf("api: %v", err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

// reply answers v, or maps err the way FastAPI does: a GrepError is the caller's
// mistake, anything else is a 500.
func reply(w http.ResponseWriter, v any, err error) {
	var ge *store.GrepError
	switch {
	case errors.As(err, &ge):
		writeJSON(w, http.StatusBadRequest, detail{ge.Msg})
	case err != nil:
		internalError(w, err)
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

// params reads query parameters as FastAPI does, collecting a 422 entry per bad one.
type params struct {
	q    url.Values
	errs []any
}

func query(r *http.Request) *params { return &params{q: r.URL.Query()} }

func (p *params) fail(typ, name, msg string, input any) {
	p.errs = append(p.errs, map[string]any{"type": typ, "loc": []string{"query", name}, "msg": msg, "input": input})
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
		p.fail("missing", name, "Field required", nil)
	}
	return v
}

func (p *params) tenant(required bool) *uuid.UUID {
	v, ok := p.value("tenant")
	if !ok {
		if required {
			p.fail("missing", "tenant", "Field required", nil)
		}
		return nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		p.fail("uuid_parsing", "tenant", "Input should be a valid UUID", v)
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
		p.fail("int_parsing", name, "Input should be a valid integer, unable to parse string as an integer", v)
	case n < lo:
		p.fail("greater_than_equal", name, fmt.Sprintf("Input should be greater than or equal to %d", lo), v)
	case hi >= 0 && n > hi:
		p.fail("less_than_equal", name, fmt.Sprintf("Input should be less than or equal to %d", hi), v)
	}
	return n
}

// invalid answers 422 if any parameter failed.
func (p *params) invalid(w http.ResponseWriter) bool {
	if len(p.errs) == 0 {
		return false
	}
	writeJSON(w, http.StatusUnprocessableEntity, detail{p.errs})
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

// credentials reads the login body. FastAPI only parses a JSON content type, and
// anything but an object with a string password fails validation.
func credentials(r *http.Request) (string, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !(mt == "application/json" || strings.HasPrefix(mt, "application/") && strings.HasSuffix(mt, "+json")) {
		return "", false
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", false
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return "", false
	}
	pw, ok := body["password"].(string)
	return pw, ok
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	password, ok := credentials(r)
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, detail{[]any{map[string]any{
			"type": "model_attributes_type", "loc": []string{"body"}, "msg": "Input should be a valid dictionary or object to extract fields from", "input": nil,
		}}})
		return
	}
	if !a.auth.CheckPassword(password) {
		writeJSON(w, http.StatusUnauthorized, detail{"Unauthorized"})
		return
	}
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

// proxies is uvicorn's forwarded_allow_ips: the peers whose X-Forwarded-Proto
// sets the scheme Python's Secure decision reads.
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
	// Starlette's delete_cookie attributes.
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Path: "/", MaxAge: -1, Expires: time.Unix(0, 0), SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) openapi(w http.ResponseWriter, r *http.Request) {
	// Compact, as FastAPI's JSONResponse renders app.openapi().
	var b bytes.Buffer
	if err := json.Compact(&b, openapiJSON); err != nil {
		internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b.Bytes())
}

func (a *api) docs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(docsHTML)
}

func (a *api) tenants(w http.ResponseWriter, r *http.Request) {
	ts, err := a.cs.Tenants(r.Context())
	out := []tenantCount{}
	for _, t := range ts {
		out = append(out, tenantCount{t.TenantID, t.Count})
	}
	reply(w, out, err)
}

func (a *api) stats(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(false)
	if p.invalid(w) {
		return
	}
	t, err := a.cs.Totals(r.Context(), tenant)
	reply(w, totalsOut{t.Concepts, t.ByType, t.Revisions, t.Links, t.Orphans}, err)
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
	reply(w, out, err)
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
	items, err := a.cs.Page(r.Context(), tenant, prefix, limit, offset)
	if err != nil {
		reply(w, nil, err)
		return
	}
	total, err := a.cs.Count(r.Context(), tenant, prefix)
	page := conceptPage{Items: []summaryOut{}, Total: total}
	for _, s := range items {
		page.Items = append(page.Items, summaryOut{s.Path, s.Type, s.Title, s.Description, s.Version, pyTime(s.UpdatedAt), s.TenantID})
	}
	reply(w, page, err)
}

func (a *api) concept(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(true)
	if p.invalid(w) {
		return
	}
	path := r.PathValue("path")
	c, backlinks, err := a.cs.ReadWithBacklinks(r.Context(), *tenant, path)
	if err != nil {
		reply(w, nil, err)
		return
	}
	if c == nil {
		writeJSON(w, http.StatusNotFound, detail{"Not Found"})
		return
	}
	history, err := a.cs.RevisionsFor(r.Context(), tenant, path, historyLimit)
	reply(w, conceptDetail{
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
	out := []searchHit{}
	for _, h := range hits {
		out = append(out, searchHit{h.Path, h.Type, h.Title, h.Description, h.Score})
	}
	reply(w, out, err)
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
	out := []grepHit{}
	for _, h := range hits {
		out = append(out, grepHit{h.Path, h.Snippet})
	}
	reply(w, out, err)
}

func (a *api) graph(w http.ResponseWriter, r *http.Request) {
	p := query(r)
	tenant := p.tenant(true)
	if p.invalid(w) {
		return
	}
	rows, err := a.cs.Graph(r.Context(), *tenant, graphLimit+1)
	if err != nil {
		reply(w, nil, err)
		return
	}
	reply(w, buildGraph(rows), nil)
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
	reply(w, revisions(rs), err)
}
