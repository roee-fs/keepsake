// Package app is the composition root: it opens the database, builds the services and mounts the controllers.
// Ported from 8f2af2e:src/keepsake/server/app.py.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/controllers"
	"github.com/roee-fs/keepsake/internal/repositories"
	"github.com/roee-fs/keepsake/internal/services"
)

// Where the image bakes the built console; KEEPSAKE_STATIC_DIR overrides it.
const defaultStaticDir = "/app/static"

type Config struct {
	DSN    string
	Schema string
	// Auth binds each /mcp and /bundle request to its tenant: FixedTenant or JWT.Middleware.
	Auth func(http.Handler) http.Handler
}

// BuildApp returns the app, its metrics handler and a closer, or fails when the database does not isolate tenants.
func BuildApp(ctx context.Context, cfg Config) (app, metrics http.Handler, closeApp func(), err error) {
	// Read before the pool opens, so a misconfigured console holds no connections.
	password, err := controllers.AdminPassword()
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := repositories.OpenVerified(ctx, cfg.DSN, cfg.Schema)
	if err != nil {
		return nil, nil, nil, err
	}

	cs := repositories.NewConceptRepository(s)
	mux := http.NewServeMux()
	// Outside the MCP handler, so a request is refused before its body is read.
	mux.Handle("/mcp", cfg.Auth(controllers.NewMCPHandler(services.NewConceptService(cs, uuid.Nil, controllers.Actor))))
	// The same auth as /mcp, so a token can replace only its own tenant's concepts.
	mux.Handle("PUT /bundle", cfg.Auth(controllers.ReplaceBundle(services.NewBundleService(cs))))
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
		controllers.WriteJSON(w, r, status, map[string]bool{"ready": ready})
	})
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	})
	var fallback http.Handler = controllers.SlashRedirect(mux, notFound)
	closeRoot := func() {}
	if controllers.UIEnabled() {
		mux.Handle("/api/", http.StripPrefix("/api", controllers.NewAPI(services.NewConsoleService(cs), controllers.NewAuth(password))))
		dir := os.Getenv("KEEPSAKE_STATIC_DIR")
		if dir == "" {
			dir = defaultStaticDir
		}
		// os.Root, not os.DirFS: a symlink out of the bundle is refused, as Starlette refuses it.
		if root, err := os.OpenRoot(dir); err == nil {
			closeRoot = func() { root.Close() }
			// The fallback after the slash redirect, and unguarded, since the login page is served from it.
			fallback = controllers.SlashRedirect(mux, controllers.StaticConsole(root.FS()))
		} else {
			slog.Warn("no console bundle; serving API and MCP only", "dir", dir)
		}
	}
	// ServeMux would otherwise 301 /api to /api/ where Starlette serves the fallback.
	mux.Handle("/api", fallback)
	mux.Handle("/", fallback)
	return controllers.RefuseBrowsers(mux), controllers.MetricsHandler(s.Stat), func() { closeRoot(); s.Close() }, nil
}
