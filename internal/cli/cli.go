// Package cli is the operator commands: import, export, validate, migrate, serve.
// Ported from 2de90d2:src/keepsake/cli/__init__.py.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
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

const defaultPort = 8000

const choices = "{import,export,validate,migrate,serve}"

// EnvPort is the port $KEEPSAKE_PORT names, or 8000 when it names nothing usable.
// kubelet injects `tcp://10.96.0.1:8000` as KEEPSAKE_PORT into every pod in a
// namespace holding a Service named keepsake, so this never fails. Only serve reads it.
func EnvPort() int {
	value := os.Getenv("KEEPSAKE_PORT")
	if !okf.IsDecimal(value) {
		return defaultPort
	}
	// `0` and `70000` are decimal and are not ports.
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return defaultPort
	}
	return port
}

// listen serves h until SIGINT or SIGTERM, then shuts down gracefully. Tests replace it.
var listen = func(addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return srv.Shutdown(context.Background())
	}
}

type options struct {
	dsn, tenant, host, directory string
	port                         *int
	badPort                      *string
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

// parse reads flags on either side of the positional argument, as argparse does;
// flag alone stops at the first positional.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
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
		return usageError(stderr, "", fmt.Sprintf("argument %s: invalid choice: %s (choose from 'import', 'export', 'validate', 'migrate', 'serve')",
			choices, okf.PyReprString(name)))
	}

	o := &options{}
	fs := flag.NewFlagSet("keepsake "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if cmd.dsn {
		fs.StringVar(&o.dsn, "dsn", os.Getenv("KEEPSAKE_DSN"), "")
	}
	if cmd.tenant {
		fs.StringVar(&o.tenant, "tenant", os.Getenv("KEEPSAKE_TENANT_ID"), "")
	}
	if name == "serve" {
		host, ok := os.LookupEnv("KEEPSAKE_HOST")
		if !ok {
			host = "0.0.0.0"
		}
		fs.StringVar(&o.host, "host", host, "")
		fs.Func("port", "", func(s string) error {
			// Python's int() takes surrounding whitespace and a sign, and no base prefix.
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				o.badPort = &s
				return err
			}
			o.port = &n
			return nil
		})
	}

	positional, err := parse(fs, args[1:])
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, help[name])
		return 0
	case o.badPort != nil:
		return usageError(stderr, name, "argument --port: invalid int value: "+okf.PyReprString(*o.badPort))
	case err != nil:
		msg := err.Error()
		if f, ok := strings.CutPrefix(msg, "flag needs an argument: -"); ok {
			msg = "argument --" + f + ": expected one argument"
		} else if f, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
			return usageError(stderr, "", "unrecognized arguments: "+given(args[1:], f))
		}
		return usageError(stderr, name, msg)
	}
	want := 0
	if cmd.directory {
		want = 1
		if len(positional) == 0 {
			return usageError(stderr, name, "the following arguments are required: directory")
		}
		o.directory = positional[0]
	}
	if len(positional) > want {
		return usageError(stderr, "", "unrecognized arguments: "+strings.Join(positional[want:], " "))
	}

	code, err := cmd.run(context.Background(), o, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "keepsake: %v\n", err)
		return 1
	}
	return code
}

// given is the argument as the operator typed it, dashes and all.
func given(args []string, name string) string {
	for _, a := range args {
		typed, _, _ := strings.Cut(a, "=")
		if strings.HasPrefix(a, "-") && strings.TrimLeft(typed, "-") == name {
			return a
		}
	}
	return "-" + name
}

func required(value, flag, env string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s is required, or set %s", flag, env)
	}
	return value, nil
}

// tenantID parses a required tenant as Python's UUID() does, which ignores braces, a
// URN prefix and where the hyphens fall.
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
	// Verified because RLS is the only thing keeping one tenant's concepts out of
	// another's bundle: a privileged DSN would export every tenant, silently.
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
	dsn, err := required(o.dsn, "--dsn", "KEEPSAKE_DSN")
	if err != nil {
		return 0, err
	}
	id, err := tenantID(o.tenant)
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
	h, closeApp, err := server.BuildApp(ctx, server.Config{DSN: dsn, TenantID: id, Schema: name})
	if err != nil {
		return 0, err
	}
	defer closeApp()
	addr := net.JoinHostPort(o.host, strconv.Itoa(port))
	slog.New(slog.NewTextHandler(stderr, nil)).Info("listening", "addr", addr)
	return 0, listen(addr, h)
}

// Help text copied from argparse at 2de90d2, so -h reads the same.
var help = map[string]string{
	"": `usage: keepsake [-h] {import,export,validate,migrate,serve} ...

Operate an OKF knowledge store. Import and export move a bundle of markdown
files in and out of Postgres; serve exposes the MCP tools.

positional arguments:
  {import,export,validate,migrate,serve}
    import              Load a bundle of markdown files into the store.
    export              Write the stored concepts out as a bundle.
    validate            Check a bundle on disk. Reads no database.
    migrate             Bring the database schema to head.
    serve               Serve the MCP tools over HTTP.

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

Serve the MCP tools over HTTP.

options:
  -h, --help       show this help message and exit
  --dsn DSN        PostgreSQL connection string. Defaults to $KEEPSAKE_DSN.
  --tenant TENANT  The tenant to read and write. Defaults to
                   $KEEPSAKE_TENANT_ID.
  --host HOST      The address to bind. Defaults to $KEEPSAKE_HOST, then every
                   interface.
  --port PORT      The port to bind. Defaults to $KEEPSAKE_PORT, then 8000.
`,
}
