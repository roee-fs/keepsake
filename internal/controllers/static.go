// Static console serving and Starlette's routing quirks.
package controllers

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// RefuseBrowsers answers Starlette's bare 403 to a /mcp or /bundle request with an Origin, which only a browser sends.
func RefuseBrowsers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Origin"]; ok && (r.URL.Path == "/mcp" || r.URL.Path == "/bundle") {
			w.Header()["Content-Type"] = nil
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SlashRedirect 307s an unmatched path to its slash-toggled twin if that has a route, as Starlette does.
func SlashRedirect(mux *http.ServeMux, def http.Handler) http.Handler {
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

// StaticConsole serves the console as StaticFiles(html=True) does, with index.html for any missing file.
func StaticConsole(root fs.FS) http.Handler {
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
