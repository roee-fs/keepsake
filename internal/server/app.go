// The HTTP entry point, which verifies the database first. Ported from 8f2af2e:src/keepsake/server/app.py.
package server

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/store"
)

// The revision log records the server, since a request carries no caller identity.
const actor = "mcp"

// Where the image bakes the built console; KEEPSAKE_STATIC_DIR overrides it.
const defaultStaticDir = "/app/static"

type Config struct {
	DSN      string
	TenantID uuid.UUID
	Schema   string
}

// BuildApp serves /mcp and /readyz and returns a closer, or fails when the database does not isolate tenants.
func BuildApp(ctx context.Context, cfg Config) (http.Handler, func(), error) {
	// Read before the pool opens, so a misconfigured console holds no connections.
	password, err := AdminPassword()
	if err != nil {
		return nil, nil, err
	}
	s, err := store.OpenVerified(ctx, cfg.DSN, cfg.Schema)
	if err != nil {
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
			// The fallback after the slash redirect, and unguarded, since the login page is served from it.
			fallback = slashRedirect(mux, staticConsole(root.FS()))
		} else {
			slog.Warn("no console bundle; serving API and MCP only", "dir", dir)
		}
	}
	// ServeMux would otherwise 301 /api to /api/ where Starlette serves the fallback.
	mux.Handle("/api", fallback)
	mux.Handle("/", fallback)
	return refuseBrowsers(mux), func() { closeRoot(); s.Close() }, nil
}

// refuseBrowsers answers Starlette's bare 403 to a /mcp request with an Origin, which only a browser sends.
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

// slashRedirect 307s an unmatched path to its slash-toggled twin if that has a route, as Starlette does.
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

// staticConsole serves the console as StaticFiles(html=True) does, with index.html for any missing file.
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
	// The shell names this build's hashed assets, so a cached copy would outlive an upgrade.
	if st.Name() == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), rs)
	return true
}
