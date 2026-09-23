// The HTTP entry point. Verify runs here, before anything can bind a port.
// Ported from 8f2af2e:src/keepsake/server/app.py.
package server

import (
	"context"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
)

// Every request is the one configured tenant, so the revision log records the server
// rather than a caller it has no way to identify.
const actor = "mcp"

// Where the image bakes the built console. Overridable so a source checkout can point
// it at a local frontend/dist.
const defaultStaticDir = "/app/static"

type Config struct {
	DSN      string
	TenantID uuid.UUID
	Schema   string
}

// BuildApp returns the MCP app at /mcp with a readiness probe at /readyz, and a func
// that closes its pool. It fails with *store.MisconfiguredDatabase when the database
// does not isolate tenants: crashing is the check.
func BuildApp(ctx context.Context, cfg Config) (http.Handler, func(), error) {
	// Read before the pool opens, so a misconfigured console fails before the
	// database is holding connections open.
	password, err := AdminPassword()
	if err != nil {
		return nil, nil, err
	}
	s, err := store.Open(ctx, cfg.DSN, cfg.Schema)
	if err != nil {
		return nil, nil, err
	}
	if err := store.Verify(ctx, s, cfg.Schema); err != nil {
		s.Close()
		return nil, nil, err
	}

	cs := store.NewConceptStore(s)
	mux := http.NewServeMux()
	mux.Handle("/mcp", NewMCPHandler(NewTools(cs, cfg.TenantID, actor)))
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		ready := s.Healthy(r.Context())
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]bool{"ready": ready})
	})
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	})
	var fallback http.Handler = slashRedirect(mux, notFound)
	closeRoot := func() {}
	if UIEnabled() {
		mux.Handle("/api/", http.StripPrefix("/api", NewAPI(cs, NewAuth(password))))
		dir := os.Getenv("KEEPSAKE_STATIC_DIR")
		if dir == "" {
			dir = defaultStaticDir
		}
		// os.Root, not os.DirFS: a symlink out of the bundle is refused, as Starlette refuses it.
		if root, err := os.OpenRoot(dir); err == nil {
			closeRoot = func() { root.Close() }
			// The router's fallback, after the slash redirect, so /mcp/ still redirects
			// to /mcp. Unguarded because gating it would break the login page.
			fallback = slashRedirect(mux, staticConsole(root.FS()))
		} else {
			log.Printf("no console bundle at %s; serving API and MCP only", dir)
		}
	}
	// ServeMux would otherwise 301 /api to /api/ where Starlette serves the fallback.
	mux.Handle("/api", fallback)
	mux.Handle("/", fallback)
	return refuseBrowsers(mux), func() { closeRoot(); s.Close() }, nil
}

// refuseBrowsers answers Starlette's bare 403 to any /mcp request carrying an Origin
// header. A browser sends Origin on every POST and an MCP client sends none, so this
// stops a DNS-rebound page without a Host allowlist of the cluster's Service names.
func refuseBrowsers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Origin"]; ok && r.URL.Path == "/mcp" {
			w.Header()["Content-Type"] = nil
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// slashRedirect answers an unmatched path as Starlette's router does: with a 307 to
// the same path with its trailing slash toggled if that path has a route, and with
// the router's default otherwise.
func slashRedirect(mux *http.ServeMux, def http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alt := r.URL.Path + "/"
		if strings.HasSuffix(r.URL.Path, "/") {
			alt = strings.TrimSuffix(r.URL.Path, "/")
		}
		probe := r.Clone(r.Context())
		probe.URL.Path = alt
		if _, pattern := mux.Handler(probe); alt != "" && pattern != "/" && pattern != "/api" {
			u := *r.URL
			u.Path = alt
			http.Redirect(w, r, u.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		def.ServeHTTP(w, r)
	})
}

// staticConsole serves the built console as Starlette's StaticFiles(html=True) does, falling
// back to index.html for a missing file so a client-side route reloaded as a deep link
// gets the SPA shell. A stale hashed asset URL also 200s as the shell.
func staticConsole(root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		if st, err := fs.Stat(root, name); err == nil && st.IsDir() {
			name = path.Join(name, "index.html")
			if _, err := fs.Stat(root, name); err == nil && !strings.HasSuffix(r.URL.Path, "/") {
				u := *r.URL
				u.Path += "/"
				http.Redirect(w, r, u.RequestURI(), http.StatusTemporaryRedirect)
				return
			}
		}
		if !serveFile(w, r, root, name) && !serveFile(w, r, root, "index.html") {
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	})
}

// serveFile writes name if it is a regular file, and writes nothing otherwise.
func serveFile(w http.ResponseWriter, r *http.Request, root fs.FS, name string) bool {
	f, err := root.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	rs, ok := f.(io.ReadSeeker)
	if err != nil || !st.Mode().IsRegular() || !ok {
		return false
	}
	// The shell names this build's hashed assets, so a cached copy outlives an
	// upgrade and loads chunks the new image no longer has.
	if st.Name() == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), rs)
	return true
}
