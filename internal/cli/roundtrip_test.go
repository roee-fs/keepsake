// Ported from 2de90d2:tests/test_roundtrip.py: a bundle imported into Postgres and exported
// again is the same bytes.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/roee-fs/keepsake/internal/migrate"
	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

var (
	db  *pgtest.DB
	ctx = context.Background()
)

func TestMain(m *testing.M) {
	// KEEPSAKE_UI defaults on, so BuildApp refuses to start without a password.
	os.Setenv("KEEPSAKE_ADMIN_PASSWORD", "test-admin-password")
	pgtest.Main(m, func(d *pgtest.DB) error {
		db = d
		return migrate.Up(ctx, d.OwnerDSN, "okf")
	})
}

const doc = `---
type: Concept
title: Five Layer Architecture
description: How the layers stack.
tags: [architecture, tooling]
custom_vendor_field: keep-me
---
The spec sits beneath the convention. Beneath that, the café.
`

func conceptStore(t *testing.T) *store.ConceptStore { return pgtest.ConceptStore(t, db.AppDSN) }

func writeFile(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// bundle writes text to <tmp>/in/architecture/<name> and returns <tmp>/in.
func bundle(t *testing.T, tmp, text, name string) string {
	t.Helper()
	src := filepath.Join(tmp, "in")
	writeFile(t, src, filepath.Join("architecture", name), text)
	return src
}

func mustImport(t *testing.T, cs *store.ConceptStore, tenant uuid.UUID, root string) int {
	t.Helper()
	n, err := ImportBundle(ctx, cs, tenant, root)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustExport(t *testing.T, cs *store.ConceptStore, tenant uuid.UUID, root string) {
	t.Helper()
	if _, err := ExportBundle(ctx, cs, tenant, root); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, cs *store.ConceptStore, tenant uuid.UUID, path string) *okf.Concept {
	t.Helper()
	c, err := cs.Read(ctx, tenant, path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func create(t *testing.T, cs *store.ConceptStore, tenant uuid.UUID, path string) {
	t.Helper()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: path, Type: "Concept"}, "test"); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	// serve points slog at stderr, which dies with this call.
	defer slog.SetDefault(slog.Default())
	code := Main(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRoundTripIsByteIdentical(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "layers.md"))
	out := filepath.Join(tmp, "out")
	mustExport(t, cs, tenant, out)
	if got := readFile(t, filepath.Join(out, "architecture", "layers.md")); got != doc {
		t.Fatalf("%q", got)
	}
}

func TestTheUnknownFieldsAreWhatPostgresHolds(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	mustImport(t, cs, tenant, bundle(t, t.TempDir(), doc, "layers.md"))
	stored := read(t, cs, tenant, "architecture/layers")
	got, _ := json.Marshal(stored.Frontmatter)
	if string(got) != `{"tags":["architecture","tooling"],"custom_vendor_field":"keep-me"}` {
		t.Fatalf("%s", got)
	}
}

func TestRoundTripSurvivesASeparateConnectionPool(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "layers.md"))
	out := filepath.Join(tmp, "out")
	mustExport(t, conceptStore(t), tenant, out)
	if got := readFile(t, filepath.Join(out, "architecture", "layers.md")); got != doc {
		t.Fatalf("%q", got)
	}
}

func TestExportGeneratesIndexAndLog(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "layers.md"))
	create(t, cs, tenant, "glossary")
	out := filepath.Join(tmp, "out")
	mustExport(t, cs, tenant, out)

	index := readFile(t, filepath.Join(out, "index.md"))
	// A concept at the root has no first segment to be grouped under.
	for _, want := range []string{"## architecture", "[architecture/layers](architecture/layers.md)", "## (top level)", "[glossary](glossary.md)"} {
		if !strings.Contains(index, want) {
			t.Errorf("index lacks %q:\n%s", want, index)
		}
	}
	log := readFile(t, filepath.Join(out, "log.md"))
	if !strings.Contains(log, "architecture/layers") || !strings.Contains(log, "create") {
		t.Errorf("log:\n%s", log)
	}
}

func TestTheLogIsDatedAndSaysWhenItIsTruncated(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "layers.md"))
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "other.md"))

	// Three revisions: the first file, the same file replayed as an update by the
	// second import, and the second file.
	full, err := renderLog(ctx, cs, tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(full, "- `architecture/"); n != 3 {
		t.Fatalf("%d entries:\n%s", n, full)
	}
	if strings.Contains(full, "omitted") {
		t.Fatal(full)
	}
	// Oldest first: a log that reads backwards is not a log.
	if strings.Index(full, "`architecture/layers` v1") > strings.Index(full, "`architecture/layers` v2") {
		t.Fatal(full)
	}
	revisions, err := cs.Revisions(ctx, tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "## "+revisions[0].Day) {
		t.Fatal(full)
	}
	if rs, _ := cs.Revisions(ctx, tenant, -1); len(rs) != 0 {
		t.Fatal(rs)
	}
	if one, _ := renderLog(ctx, cs, tenant, 1); !strings.Contains(one, "omitted") {
		t.Fatal(one)
	}
}

func TestGeneratedFilesAreNotStoredAsConcepts(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	out := filepath.Join(t.TempDir(), "out")
	mustExport(t, cs, tenant, out)
	mustImport(t, cs, tenant, out)
	if read(t, cs, tenant, "index") != nil || read(t, cs, tenant, "log") != nil {
		t.Fatal("a generated file was stored")
	}
}

func TestOnlyTheBundleRootReservesTheGeneratedNames(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	if n := mustImport(t, cs, tenant, bundle(t, t.TempDir(), doc, "index.md")); n != 1 {
		t.Fatal(n)
	}
	if read(t, cs, tenant, "architecture/index") == nil {
		t.Fatal("architecture/index was dropped")
	}
}

func TestImportCountsTheConceptsItWrote(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	src := bundle(t, t.TempDir(), doc, "layers.md")
	writeFile(t, src, "index.md", "# Index\n")
	writeFile(t, src, "log.md", "# Log\n")
	if n := mustImport(t, cs, tenant, src); n != 1 {
		t.Fatal(n)
	}
}

func TestReimportingAPathUpdatesIt(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "layers.md"))
	mustImport(t, cs, tenant, bundle(t, tmp, strings.ReplaceAll(doc, "beneath", "above"), "layers.md"))
	stored := read(t, cs, tenant, "architecture/layers")
	if !strings.Contains(stored.Body, "above") || stored.Version != 2 {
		t.Fatalf("%+v", stored)
	}
}

func TestExportRefusesAPathThatEscapesTheBundle(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	create(t, cs, tenant, "../escape")
	_, err := ExportBundle(ctx, cs, tenant, filepath.Join(tmp, "out"))
	if err == nil || err.Error() != "refusing to write '../escape' outside the bundle" {
		t.Fatalf("err = %v", err)
	}
	if exists(filepath.Join(tmp, "escape.md")) {
		t.Fatal("escape.md was written")
	}
}

func TestExportRefusesAConceptThatCollidesWithAGeneratedFile(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "layers.md"))
	create(t, cs, tenant, "index")
	out := filepath.Join(tmp, "out")
	_, err := ExportBundle(ctx, cs, tenant, out)
	want := "the concept 'index' collides with a generated file: the bundle root reserves index.md and log.md"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
	// Refused before the first write: a half-written bundle looks like a whole one.
	if exists(filepath.Join(out, "architecture", "layers.md")) {
		t.Fatal("written before refusing")
	}
}

func TestExportRefusesADSNThatBypassesRowLevelSecurity(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	code, _, stderr := run(t, "export", out, "--dsn", db.OwnerDSN, "--tenant", uuid.NewString())
	if code != 1 || !strings.Contains(stderr, "owns the schema") || !strings.HasPrefix(stderr, "keepsake: ") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if exists(filepath.Join(out, "index.md")) {
		t.Fatal("index.md was written")
	}
}

func TestAMalformedDocumentIsNamedAndRefusesTheWholeImport(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	src := bundle(t, t.TempDir(), doc, "layers.md")
	writeFile(t, src, "architecture/zz-broken.md", "---\ntype: [unclosed\n---\nx\n")
	if _, err := ImportBundle(ctx, cs, tenant, src); err == nil || !strings.Contains(err.Error(), "zz-broken.md") {
		t.Fatalf("err = %v", err)
	}
	// Nothing was written: the bundle is parsed through before the first insert.
	if read(t, cs, tenant, "architecture/layers") != nil {
		t.Fatal("written before refusing")
	}
}

func TestAnInvalidDocumentIsNamedAndRefusesTheWholeImport(t *testing.T) {
	cs, tenant := conceptStore(t), uuid.New()
	src := bundle(t, t.TempDir(), doc, "layers.md")
	writeFile(t, src, "architecture/zz-typeless.md", "---\ntitle: No type\n---\nx\n")
	if _, err := ImportBundle(ctx, cs, tenant, src); err == nil || !strings.Contains(err.Error(), "zz-typeless.md: type is required") {
		t.Fatalf("err = %v", err)
	}
	if read(t, cs, tenant, "architecture/layers") != nil {
		t.Fatal("written before refusing")
	}
}

func TestValidateReportsAMalformedDocumentByName(t *testing.T) {
	src := bundle(t, t.TempDir(), doc, "layers.md")
	writeFile(t, src, "architecture/broken.md", "---\ntype: [unclosed\n---\nx\n")
	if _, err := ValidateBundle(src); err == nil || !strings.Contains(err.Error(), "broken.md") {
		t.Fatalf("err = %v", err)
	}
}

func roundTrip(t *testing.T, text string) (string, *okf.Concept) {
	t.Helper()
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, text, "layers.md"))
	out := filepath.Join(tmp, "out")
	mustExport(t, cs, tenant, out)
	return readFile(t, filepath.Join(out, "architecture", "layers.md")), read(t, cs, tenant, "architecture/layers")
}

func TestCRLFIsNormalisedToLF(t *testing.T) {
	if got, _ := roundTrip(t, strings.ReplaceAll(doc, "\n", "\r\n")); got != doc {
		t.Fatalf("%q", got)
	}
}

func TestAFrontmatterCommentIsNotPreserved(t *testing.T) {
	if got, _ := roundTrip(t, strings.Replace(doc, "type: Concept\n", "type: Concept\n# why this exists\n", 1)); got != doc {
		t.Fatalf("%q", got)
	}
}

func TestAnUnquotedYAMLDateComesBackAsAString(t *testing.T) {
	got, stored := roundTrip(t, strings.Replace(doc, "custom_vendor_field: keep-me", "created: 2026-01-01", 1))
	if v, _ := stored.Frontmatter.Get("created"); v != "2026-01-01" {
		t.Fatalf("created = %#v", v)
	}
	if !strings.Contains(got, "2026-01-01") || strings.Contains(got, "created: 2026-01-01\n") {
		t.Fatalf("%q", got)
	}
}

func TestValidateAcceptsABundleWhoseLinksResolve(t *testing.T) {
	src := bundle(t, t.TempDir(), strings.Replace(doc, "convention.", "convention. [see](./other.md)", 1), "layers.md")
	writeFile(t, src, "architecture/other.md", "---\ntype: Concept\n---\nOther.\n")
	if errs, err := ValidateBundle(src); err != nil || len(errs) != 0 {
		t.Fatal(errs, err)
	}
}

func TestValidateReportsADanglingLinkAndAMissingType(t *testing.T) {
	src := bundle(t, t.TempDir(), strings.Replace(doc, "convention.", "convention. [gone](./gone.md)", 1), "layers.md")
	writeFile(t, src, "architecture/typeless.md", "---\ntitle: No type\n---\nx\n")
	errs, err := ValidateBundle(src)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(errs, "\n")
	if !strings.Contains(all, "architecture/layers: link to unknown concept architecture/gone") ||
		!strings.Contains(all, "architecture/typeless: type is required") {
		t.Fatal(all)
	}
}

func TestMainExportsABundle(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "layers.md"))
	out := filepath.Join(tmp, "out")
	code, stdout, stderr := run(t, "export", out, "--dsn", db.AppDSN, "--tenant", tenant.String())
	if code != 0 || stdout != "exported 1 concepts\n" {
		t.Fatalf("exit %d: %q %q", code, stdout, stderr)
	}
	if got := readFile(t, filepath.Join(out, "architecture", "layers.md")); got != doc {
		t.Fatalf("%q", got)
	}
}

type servedApp struct {
	addr, metricsAddr string
	h, metrics        http.Handler
}

// fakeListen replaces the listener the way the Python tests monkeypatch uvicorn.run.
func fakeListen(t *testing.T) *servedApp {
	t.Helper()
	got := &servedApp{}
	orig := listen
	listen = func(apps map[string]http.Handler) error {
		for addr, h := range apps {
			if strings.HasSuffix(addr, ":9123") || strings.HasSuffix(addr, ":8000") {
				got.addr, got.h = addr, h
			} else {
				got.metricsAddr, got.metrics = addr, h
			}
		}
		return nil
	}
	t.Cleanup(func() { listen = orig })
	return got
}

func TestServeServesMetricsOnTheirOwnPort(t *testing.T) {
	t.Setenv("KEEPSAKE_METRICS_PORT", "9124")
	served := fakeListen(t)
	if code, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.NewString(), "--port", "9123"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if served.metricsAddr != "0.0.0.0:9124" {
		t.Fatal(served.metricsAddr)
	}
	rec := httptest.NewRecorder()
	served.metrics.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "keepsake_tool_calls_total") {
		t.Fatalf("%d: %.200s", rec.Code, rec.Body.String())
	}
}

func TestServeRefusesAMetricsPortEqualToTheServePort(t *testing.T) {
	t.Setenv("KEEPSAKE_METRICS_PORT", "9123")
	fakeListen(t)
	code, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.NewString(), "--port", "9123")
	if code != 1 || !strings.Contains(stderr, "KEEPSAKE_METRICS_PORT must differ") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestServeHandsTheListenerTheVerifiedAppAndTheParsedPort(t *testing.T) {
	served := fakeListen(t)
	code, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.NewString(), "--port", "9123")
	// Every interface, not loopback: the chart's probes reach the pod from outside it.
	if code != 0 || served.addr != "0.0.0.0:9123" || served.h == nil {
		t.Fatalf("exit %d, addr %q: %s", code, served.addr, stderr)
	}
}

func TestServeLogsTheListenAddress(t *testing.T) {
	fakeListen(t)
	_, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.NewString(), "--port", "9123")
	if !strings.Contains(stderr, "addr=0.0.0.0:9123") {
		t.Fatalf("stderr %q does not name the listen address", stderr)
	}
}

func TestServeFallsBackWhenThePortVariableIsAServiceLink(t *testing.T) {
	t.Setenv("KEEPSAKE_PORT", "tcp://10.96.0.1:8000")
	served := fakeListen(t)
	if code, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.NewString()); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if served.addr != "0.0.0.0:8000" {
		t.Fatal(served.addr)
	}
}

func TestAServiceLinkPortDoesNotBreakASubcommandThatBindsNothing(t *testing.T) {
	t.Setenv("KEEPSAKE_PORT", "tcp://10.96.0.1:8000")
	if code, _, stderr := run(t, "validate", bundle(t, t.TempDir(), doc, "layers.md")); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestServeRefusesADatabaseThatDoesNotIsolate(t *testing.T) {
	fakeListen(t)
	code, _, stderr := run(t, "serve", "--dsn", db.AdminDSN, "--tenant", uuid.NewString())
	if code != 1 || !strings.Contains(stderr, "must not connect as a superuser") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestMainReportsABadTenantWithoutATraceback(t *testing.T) {
	code, _, stderr := run(t, "export", t.TempDir(), "--dsn", db.AppDSN, "--tenant", "not-a-uuid")
	if code != 1 || stderr != "keepsake: not a tenant uuid: 'not-a-uuid'\n" {
		t.Fatalf("exit %d: %q", code, stderr)
	}
}

func TestMainReportsAMissingDSNWithoutATraceback(t *testing.T) {
	t.Setenv("KEEPSAKE_DSN", "")
	code, _, stderr := run(t, "export", t.TempDir(), "--tenant", uuid.NewString())
	if code != 1 || stderr != "keepsake: --dsn is required, or set KEEPSAKE_DSN\n" {
		t.Fatalf("exit %d: %q", code, stderr)
	}
}

func TestEnvPort(t *testing.T) {
	for value, want := range map[string]int{
		"8080":  8080,
		"1":     1,
		"65535": 65535,
		// Decimal, and not a port.
		"0":            8000,
		"65536":        8000,
		"70000":        8000,
		"999999999999": 8000,
		// Never a port number to begin with.
		"tcp://10.96.0.1:8000": 8000,
		"":                     8000,
		"²":                    8000,
	} {
		t.Setenv("KEEPSAKE_PORT", value)
		if got := EnvPort(); got != want {
			t.Errorf("EnvPort(%q) = %d, want %d", value, got, want)
		}
	}
}

type exportGolden struct {
	Name     string
	Exported string
}

func TestExportMatchesPythonForEveryGolden(t *testing.T) {
	raw, err := os.ReadFile("../../okf/testdata/goldens/export.json")
	if err != nil {
		t.Fatal(err)
	}
	var gs []exportGolden
	if err := json.Unmarshal(raw, &gs); err != nil || len(gs) < 81 {
		t.Fatalf("%d goldens: %v", len(gs), err)
	}
	cs := conceptStore(t)
	for _, g := range gs {
		t.Run(g.Name, func(t *testing.T) {
			tenant, tmp := uuid.New(), t.TempDir()
			writeFile(t, filepath.Join(tmp, "in"), "doc.md", readFile(t, "../../okf/testdata/corpus/"+g.Name+".md"))
			_, err := ImportBundle(ctx, cs, tenant, filepath.Join(tmp, "in"))
			if err != nil && strings.Contains(err.Error(), ": unsupported YAML tag ") {
				t.Skip("deliberate: Python accepts this tag and Go refuses it")
			}
			if err != nil {
				t.Fatal(err)
			}
			mustExport(t, cs, tenant, filepath.Join(tmp, "out"))
			if got := readFile(t, filepath.Join(tmp, "out", "doc.md")); got != g.Exported {
				t.Errorf("go:\n%s\npython:\n%s", got, g.Exported)
			}
		})
	}
}

func TestImportRefusesNaNAndWritesNothing(t *testing.T) {
	cs, tenant, dir := conceptStore(t), uuid.New(), t.TempDir()
	writeFile(t, dir, "a.md", "---\ntype: Concept\n---\nok\n")
	writeFile(t, dir, "b.md", "---\ntype: Concept\nv: [1, .nan]\n---\nx\n")
	var stderr bytes.Buffer
	code := Main([]string{"import", dir, "--dsn", db.AppDSN, "--tenant", tenant.String()}, io.Discard, &stderr)
	want := "keepsake: " + filepath.Join(dir, "b.md") + ": frontmatter holds NaN or Infinity, which JSON cannot store\n"
	if code != 1 || stderr.String() != want {
		t.Fatalf("exit %d: %q", code, stderr.String())
	}
	if read(t, cs, tenant, "a") != nil {
		t.Fatal("a.md was written: import MUST be all-or-nothing")
	}
}

// Python's validate never serialises frontmatter, so NaN is no error there.
func TestValidateAcceptsNaN(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.md", "---\ntype: Concept\nv: [1, .nan]\n---\nx\n")
	if errs, err := ValidateBundle(dir); err != nil || len(errs) != 0 {
		t.Fatal(errs, err)
	}
}

func TestImportAndValidateFollowASymlinkedRoot(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	real := bundle(t, tmp, doc, "layers.md")
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if n := mustImport(t, cs, tenant, link); n != 1 {
		t.Fatalf("imported %d", n)
	}
	writeFile(t, real, "architecture/typeless.md", "---\ntitle: No type\n---\nx\n")
	if errs, err := ValidateBundle(link); err != nil || len(errs) != 1 {
		t.Fatal(errs, err)
	}
	// Files are named by the path the operator typed, not the one it resolves to.
	_, err := ImportBundle(ctx, cs, tenant, link)
	if want := filepath.Join(link, "architecture", "typeless.md") + ": type is required"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
}

func TestExportWritesThroughASymlinkedRoot(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, bundle(t, tmp, doc, "layers.md"))
	real, link := filepath.Join(tmp, "real"), filepath.Join(tmp, "link")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	mustExport(t, cs, tenant, link)
	if got := readFile(t, filepath.Join(real, "architecture", "layers.md")); got != doc {
		t.Fatalf("%q", got)
	}
}

// Python dates the log in the session TimeZone, so a Kiritimati (UTC+14) session
// puts an evening UTC revision on the next day.
func TestTheLogIsDatedInTheSessionTimeZone(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	create(t, cs, tenant, "a")
	admin, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "UPDATE okf.concept_revision SET created_at = '2026-01-01 20:00:00+00' WHERE tenant_id = $1", tenant); err != nil {
		t.Fatal(err)
	}
	for dsn, want := range map[string]string{
		db.AppDSN: "## 2026-01-01",
		db.AppDSN + "&options=-c%20TimeZone%3DPacific/Kiritimati": "## 2026-01-02",
	} {
		s, err := store.Open(ctx, dsn, "okf")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		out := filepath.Join(tmp, strings.TrimPrefix(want, "## "))
		mustExport(t, store.NewConceptStore(s), tenant, out)
		if log := readFile(t, filepath.Join(out, "log.md")); !strings.Contains(log, want+"\n") {
			t.Errorf("want %s:\n%s", want, log)
		}
	}
}

// The demo's first diff is empty only while this holds.
func TestTheDemoBundleRoundTripsAndLinksOnlyToItself(t *testing.T) {
	src := filepath.Join("..", "..", "demo", "bundle")
	if errs, err := ValidateBundle(src); err != nil || len(errs) > 0 {
		t.Fatal(err, errs)
	}
	cs, tenant, out := conceptStore(t), uuid.New(), t.TempDir()
	mustImport(t, cs, tenant, src)
	mustExport(t, cs, tenant, out)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if got := readFile(t, filepath.Join(out, rel)); got != readFile(t, p) {
			t.Errorf("%s came back as:\n%s", rel, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
