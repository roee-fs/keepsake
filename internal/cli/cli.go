// Package cli is the operator commands, ported from 2de90d2:src/keepsake/cli/__init__.py.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/roee-fs/keepsake/internal/migrate"
	"github.com/roee-fs/keepsake/internal/server"
	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

const (
	defaultPort        = 8000
	defaultMetricsPort = 9090
)

const choices = "{import,export,validate,migrate,serve,token}"

// EnvPort is $KEEPSAKE_PORT, or 8000 for anything else, such as the service link kubelet injects.
func EnvPort() int { return envPort("KEEPSAKE_PORT", defaultPort) }

func envPort(name string, def int) int {
	value := os.Getenv(name)
	if !okf.IsDecimal(value) {
		return def
	}
	// `0` and `70000` are decimal and are not ports.
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return def
	}
	return port
}

// listen serves each address's handler until SIGINT, SIGTERM or one server fails, then shuts all down. Tests replace it.
var listen = func(apps map[string]http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, len(apps))
	var servers []*http.Server
	for addr, h := range apps {
		// IdleTimeout is uvicorn's keep-alive timeout; zero would keep an idle connection forever.
		srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second}
		servers = append(servers, srv)
		go func() { errc <- srv.ListenAndServe() }()
	}
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	for _, srv := range servers {
		err = errors.Join(err, srv.Shutdown(context.Background()))
	}
	return err
}

type options struct {
	dsn, tenant, host, directory, sub, ttl string
	port                                   *int
}

type command struct {
	dsn, tenant, directory bool
	run                    func(ctx context.Context, o *options, stdout, stderr io.Writer) (int, error)
}

var commands = map[string]command{
	"import":   {dsn: true, tenant: true, directory: true, run: runBundle(ImportBundle, "imported")},
	"export":   {dsn: true, tenant: true, directory: true, run: runBundle(ExportBundle, "exported")},
	"validate": {directory: true, run: runValidate},
	"migrate":  {dsn: true, run: runMigrate},
	"serve":    {dsn: true, tenant: true, run: runServe},
	"token":    {tenant: true, run: runToken},
}

// usage is the help text's first paragraph, which is argparse's usage line.
func usage(name string) string {
	h := help[name]
	return h[:strings.Index(h, "\n\n")+1]
}

// usageError prints as argparse does and exits 2. An empty name is the top-level parser.
func usageError(stderr io.Writer, name, msg string) int {
	prog := "keepsake"
	if name != "" {
		prog += " " + name
	}
	fmt.Fprintf(stderr, "%s%s: error: %s\n", usage(name), prog, msg)
	return 2
}

// Main runs one command and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usageError(stderr, "", "the following arguments are required: "+choices)
	}
	name := args[0]
	if name == "-h" || name == "--help" {
		fmt.Fprint(stdout, help[""])
		return 0
	}
	cmd, ok := commands[name]
	if !ok {
		return usageError(stderr, "", fmt.Sprintf("argument %s: invalid choice: %s (choose from 'import', 'export', 'validate', 'migrate', 'serve', 'token')",
			choices, okf.PyReprString(name)))
	}

	o := &options{dsn: os.Getenv("KEEPSAKE_DSN"), tenant: os.Getenv("KEEPSAKE_TENANT_ID")}
	flags := map[string]*string{}
	if cmd.dsn {
		flags["--dsn"] = &o.dsn
	}
	if cmd.tenant {
		flags["--tenant"] = &o.tenant
	}
	var port string
	if name == "serve" {
		host, ok := os.LookupEnv("KEEPSAKE_HOST")
		if !ok {
			host = "0.0.0.0"
		}
		o.host = host
		flags["--host"] = &o.host
		flags["--port"] = &port
	}
	if name == "token" {
		o.ttl = "1h"
		flags["--sub"] = &o.sub
		flags["--ttl"] = &o.ttl
	}

	// argparse's order: flags anywhere, -h at once, and leftovers reported together at the end.
	var extra []string
	hasDirectory := false
	positional := func(a string) {
		if cmd.directory && !hasDirectory {
			o.directory, hasDirectory = a, true
		} else {
			extra = append(extra, a)
		}
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch flag, value, hasValue := strings.Cut(a, "="); {
		case a == "--":
			for _, p := range args[i+1:] {
				positional(p)
			}
			i = len(args)
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, help[name])
			return 0
		case !isFlag(a):
			positional(a)
		case flags[flag] == nil:
			extra = append(extra, a)
		default:
			if !hasValue {
				if i+1 == len(args) || isFlag(args[i+1]) {
					return usageError(stderr, name, "argument "+flag+": expected one argument")
				}
				i++
				value = args[i]
			}
			*flags[flag] = value
			if flag == "--port" {
				// Python's int() takes surrounding whitespace and a sign, and no base prefix.
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil {
					return usageError(stderr, name, "argument --port: invalid int value: "+okf.PyReprString(value))
				}
				o.port = &n
			}
		}
	}
	if cmd.directory && !hasDirectory {
		return usageError(stderr, name, "the following arguments are required: directory")
	}
	if len(extra) > 0 {
		return usageError(stderr, "", "unrecognized arguments: "+strings.Join(extra, " "))
	}

	code, err := cmd.run(context.Background(), o, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "keepsake: %v\n", err)
		return 1
	}
	return code
}

var negativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// isFlag is argparse's test for an option string: a dash, and not a negative number.
func isFlag(a string) bool {
	return strings.HasPrefix(a, "-") && a != "-" && !negativeNumber.MatchString(a)
}

func required(value, flag, env string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s is required, or set %s", flag, env)
	}
	return value, nil
}

// tenantID parses a tenant as Python's UUID() does, ignoring braces, a URN prefix and hyphen positions.
func tenantID(value string) (uuid.UUID, error) {
	if _, err := required(value, "--tenant", "KEEPSAKE_TENANT_ID"); err != nil {
		return uuid.UUID{}, err
	}
	hex := strings.ReplaceAll(strings.ReplaceAll(value, "urn:", ""), "uuid:", "")
	hex = strings.ReplaceAll(strings.Trim(hex, "{}"), "-", "")
	id, err := uuid.Parse(hex)
	if len(hex) != 32 || err != nil {
		return uuid.UUID{}, fmt.Errorf("not a tenant uuid: %s", okf.PyReprString(value))
	}
	return id, nil
}

// bound opens a store and parses a tenant for one command. The func closes the store.
func bound(ctx context.Context, o *options) (*store.ConceptStore, uuid.UUID, func(), error) {
	id, err := tenantID(o.tenant)
	if err != nil {
		return nil, uuid.UUID{}, nil, err
	}
	dsn, err := required(o.dsn, "--dsn", "KEEPSAKE_DSN")
	if err != nil {
		return nil, uuid.UUID{}, nil, err
	}
	name, err := store.Schema()
	if err != nil {
		return nil, uuid.UUID{}, nil, err
	}
	// Verified: RLS alone keeps another tenant's concepts out of this bundle.
	s, err := store.OpenVerified(ctx, dsn, name)
	if err != nil {
		return nil, uuid.UUID{}, nil, err
	}
	return store.NewConceptStore(s), id, s.Close, nil
}

// runBundle runs import or export against the bound store and reports the count.
func runBundle(fn func(context.Context, *store.ConceptStore, uuid.UUID, string) (int, error), verb string) func(context.Context, *options, io.Writer, io.Writer) (int, error) {
	return func(ctx context.Context, o *options, stdout, _ io.Writer) (int, error) {
		cs, tenant, closeStore, err := bound(ctx, o)
		if err != nil {
			return 0, err
		}
		defer closeStore()
		n, err := fn(ctx, cs, tenant, o.directory)
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "%s %d concepts\n", verb, n)
		return 0, nil
	}
}

func runValidate(_ context.Context, o *options, _, stderr io.Writer) (int, error) {
	errs, err := ValidateBundle(o.directory)
	if err != nil {
		return 0, err
	}
	for _, e := range errs {
		fmt.Fprintln(stderr, e)
	}
	if len(errs) > 0 {
		return 1, nil
	}
	return 0, nil
}

// runMigrate brings the schema to head. It runs as the owner role, never as the app role.
func runMigrate(ctx context.Context, o *options, _, _ io.Writer) (int, error) {
	dsn, err := required(o.dsn, "--dsn", "KEEPSAKE_DSN")
	if err != nil {
		return 0, err
	}
	name, err := store.Schema()
	if err != nil {
		return 0, err
	}
	return 0, migrate.Up(ctx, dsn, name)
}

func runServe(ctx context.Context, o *options, _, stderr io.Writer) (int, error) {
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, nil)))
	dsn, err := required(o.dsn, "--dsn", "KEEPSAKE_DSN")
	if err != nil {
		return 0, err
	}
	auth, err := authFromEnv(o.tenant)
	if err != nil {
		return 0, err
	}
	name, err := store.Schema()
	if err != nil {
		return 0, err
	}
	port := EnvPort()
	if o.port != nil {
		port = *o.port
	}
	h, metrics, closeApp, err := server.BuildApp(ctx, server.Config{DSN: dsn, Auth: auth, Schema: name})
	if err != nil {
		return 0, err
	}
	defer closeApp()
	addr := net.JoinHostPort(o.host, strconv.Itoa(port))
	metricsAddr := net.JoinHostPort(o.host, strconv.Itoa(envPort("KEEPSAKE_METRICS_PORT", defaultMetricsPort)))
	if metricsAddr == addr {
		return 0, fmt.Errorf("KEEPSAKE_METRICS_PORT must differ from the serve port %d", port)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	slog.Info("listening", "addr", addr, "metrics", metricsAddr)
	return 0, listen(map[string]http.Handler{addr: h, metricsAddr: mux})
}

// authFromEnv picks how /mcp finds its tenant from KEEPSAKE_AUTH_MODE, none when unset.
func authFromEnv(tenant string) (func(http.Handler) http.Handler, error) {
	switch mode := os.Getenv("KEEPSAKE_AUTH_MODE"); mode {
	case "", "none":
		id, err := callerTenant(tenant)
		if err != nil {
			return nil, err
		}
		return server.FixedTenant(id), nil
	case "jwt":
		// A fixed tenant beside jwt would read as a fallback, and there is none.
		if tenant != "" {
			return nil, errors.New("--tenant and KEEPSAKE_TENANT_ID are refused in jwt mode; the token names the tenant")
		}
		j, err := server.JWTFromEnv()
		if err != nil {
			return nil, err
		}
		return j.Middleware, nil
	default:
		return nil, fmt.Errorf("KEEPSAKE_AUTH_MODE must be none or jwt, not %s", okf.PyReprString(mode))
	}
}

// callerTenant is tenantID without the nil uuid, which the MCP tools refuse to serve.
func callerTenant(value string) (uuid.UUID, error) {
	id, err := tenantID(value)
	if err == nil && id == uuid.Nil {
		return id, errors.New("the nil uuid is not a tenant")
	}
	return id, err
}

// runToken prints a token for one tenant, signed with the first secret jwt mode verifies.
func runToken(_ context.Context, o *options, stdout, _ io.Writer) (int, error) {
	id, err := callerTenant(o.tenant)
	if err != nil {
		return 0, err
	}
	if o.sub == "" {
		return 0, errors.New("--sub is required")
	}
	// exp is whole seconds, so a shorter ttl would mint a token that is already expired.
	ttl, err := time.ParseDuration(o.ttl)
	if err != nil || ttl < time.Second {
		return 0, fmt.Errorf("--ttl must be a duration of at least 1s, such as 1h, not %s", okf.PyReprString(o.ttl))
	}
	j, err := server.JWTFromEnv()
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(stdout, j.Mint(id, o.sub, ttl))
	return 0, nil
}

// Help text copied from argparse at 2de90d2, so -h reads the same.
var help = map[string]string{
	"": `usage: keepsake [-h] {import,export,validate,migrate,serve,token} ...

Operate an OKF knowledge store. Import and export move a bundle of markdown
files in and out of Postgres; serve exposes the MCP tools.

positional arguments:
  {import,export,validate,migrate,serve,token}
    import              Load a bundle of markdown files into the store.
    export              Write the stored concepts out as a bundle.
    validate            Check a bundle on disk. Reads no database.
    migrate             Bring the database schema to head.
    serve               Serve the MCP tools over HTTP.
    token               Print a jwt-mode token for one tenant.

options:
  -h, --help            show this help message and exit
`,
	"import": `usage: keepsake import [-h] [--dsn DSN] [--tenant TENANT] directory

Load a bundle of markdown files into the store.

positional arguments:
  directory        The bundle to read.

options:
  -h, --help       show this help message and exit
  --dsn DSN        PostgreSQL connection string. Defaults to $KEEPSAKE_DSN.
  --tenant TENANT  The tenant to read and write. Defaults to
                   $KEEPSAKE_TENANT_ID.
`,
	"export": `usage: keepsake export [-h] [--dsn DSN] [--tenant TENANT] directory

Write the stored concepts out as a bundle.

positional arguments:
  directory        The directory to write.

options:
  -h, --help       show this help message and exit
  --dsn DSN        PostgreSQL connection string. Defaults to $KEEPSAKE_DSN.
  --tenant TENANT  The tenant to read and write. Defaults to
                   $KEEPSAKE_TENANT_ID.
`,
	"validate": `usage: keepsake validate [-h] directory

Check a bundle on disk. Reads no database.

positional arguments:
  directory   The bundle to check.

options:
  -h, --help  show this help message and exit
`,
	"migrate": `usage: keepsake migrate [-h] [--dsn DSN]

Bring the database schema to head.

options:
  -h, --help  show this help message and exit
  --dsn DSN   PostgreSQL connection string. Defaults to $KEEPSAKE_DSN.
`,
	"serve": `usage: keepsake serve [-h] [--dsn DSN] [--tenant TENANT] [--host HOST]
                      [--port PORT]

Serve the MCP tools over HTTP. $KEEPSAKE_AUTH_MODE is none (one tenant, set by
--tenant) or jwt (the tenant comes from each request's bearer token).
Prometheus metrics are at /metrics on $KEEPSAKE_METRICS_PORT, then 9090.

options:
  -h, --help       show this help message and exit
  --dsn DSN        PostgreSQL connection string. Defaults to $KEEPSAKE_DSN.
  --tenant TENANT  The tenant to read and write, in none mode only. Defaults
                   to $KEEPSAKE_TENANT_ID.
  --host HOST      The address to bind. Defaults to $KEEPSAKE_HOST, then every
                   interface.
  --port PORT      The port to bind. Defaults to $KEEPSAKE_PORT, then 8000.
`,
	"token": `usage: keepsake token [-h] [--tenant TENANT] [--sub SUB] [--ttl TTL]

Print a jwt-mode token for one tenant, signed with the first secret in
$KEEPSAKE_JWT_SECRET_FILE. Hand it to a client that must not hold the secret.

options:
  -h, --help       show this help message and exit
  --tenant TENANT  The tenant the token reads and writes. Defaults to
                   $KEEPSAKE_TENANT_ID.
  --sub SUB        Who the token acts as, recorded as updated_by.
  --ttl TTL        How long the token lives, such as 30m or 720h. Defaults
                   to 1h.
`,
}
