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

// Handlers are what each port serves.
type Handlers struct {
	// App serves /mcp, PUT /bundle and /readyz.
	App http.Handler
	// Console serves the admin console and its /api, or is nil when the console is off.
	Console http.Handler
	Metrics http.Handler
}

// BuildApp returns the handlers and a closer, or fails when the database does not isolate tenants.
func BuildApp(ctx context.Context, cfg Config) (Handlers, func(), error) {
	// Read before the pool opens, so a misconfigured console holds no connections.
	password, err := controllers.AdminPassword()
	if err != nil {
		return Handlers{}, nil, err
	}
	s, err := repositories.OpenVerified(ctx, cfg.DSN, cfg.Schema)
	if err != nil {
		return Handlers{}, nil, err
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
	mux.Handle("/", controllers.SlashRedirect(mux, notFound))
	h := Handlers{App: controllers.AccessLog(controllers.RefuseBrowsers(mux)), Metrics: controllers.MetricsHandler(s.Stat)}
	closeRoot := func() {}
	if controllers.UIEnabled() {
		console := http.NewServeMux()
		console.Handle("/api/", http.StripPrefix("/api", controllers.NewAPI(services.NewConsoleService(cs), controllers.NewAuth(password))))
		var static http.Handler = notFound
		dir := os.Getenv("KEEPSAKE_STATIC_DIR")
		if dir == "" {
			dir = defaultStaticDir
		}
		// os.Root, not os.DirFS: a symlink out of the bundle is refused, as Starlette refuses it.
		if root, err := os.OpenRoot(dir); err == nil {
			closeRoot = func() { root.Close() }
			// Unguarded, since the login page is served from it.
			static = controllers.StaticConsole(root.FS())
		} else {
			slog.Warn("no console bundle; serving the console API only", "dir", dir)
		}
		fallback := controllers.SlashRedirect(console, static)
		// ServeMux would otherwise 301 /api to /api/ where Starlette serves the fallback.
		console.Handle("/api", fallback)
		console.Handle("/", fallback)
		h.Console = controllers.AccessLog(console)
	}
	return h, func() { closeRoot(); s.Close() }, nil
}
