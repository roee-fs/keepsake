package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/roee-fs/keepsake/internal/store"
	"github.com/roee-fs/keepsake/okf"
)

func TestFlagsMayComeBeforeOrAfterTheDirectory(t *testing.T) {
	cs, tenant, tmp := conceptStore(t), uuid.New(), t.TempDir()
	src := bundle(t, tmp, doc, "layers.md")
	code, stdout, stderr := run(t, "import", "--dsn", db.AppDSN, src, "--tenant", tenant.String())
	if code != 0 || stdout != "imported 1 concepts\n" {
		t.Fatalf("exit %d: %q %q", code, stdout, stderr)
	}
	if read(t, cs, tenant, "architecture/layers") == nil {
		t.Fatal("nothing imported")
	}
}

func TestFlagsDefaultToTheEnvironment(t *testing.T) {
	tenant := uuid.New()
	t.Setenv("KEEPSAKE_DSN", db.AppDSN)
	t.Setenv("KEEPSAKE_TENANT_ID", tenant.String())
	if code, _, stderr := run(t, "import", bundle(t, t.TempDir(), doc, "layers.md")); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if read(t, conceptStore(t), tenant, "architecture/layers") == nil {
		t.Fatal("nothing imported")
	}
}

// Usage errors exit 2, as argparse does, with argparse's wording where it is ours to choose.
func TestUsageErrorsExitTwo(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "keepsake: error: the following arguments are required: {import,export,validate,migrate,serve}\n"},
		{[]string{"bogus"}, "keepsake: error: argument {import,export,validate,migrate,serve}: invalid choice: 'bogus' (choose from 'import', 'export', 'validate', 'migrate', 'serve')\n"},
		{[]string{"import"}, "keepsake import: error: the following arguments are required: directory\n"},
		{[]string{"validate", "a", "b"}, "keepsake: error: unrecognized arguments: b\n"},
		{[]string{"migrate", "x"}, "keepsake: error: unrecognized arguments: x\n"},
		{[]string{"serve", "--port", "abc"}, "keepsake serve: error: argument --port: invalid int value: 'abc'\n"},
		{[]string{"import", "x", "--nope"}, "keepsake: error: unrecognized arguments: --nope\n"},
		{[]string{"import", "x", "--dsn"}, "keepsake import: error: argument --dsn: expected one argument\n"},
	} {
		code, _, stderr := run(t, c.args...)
		if code != 2 || !strings.HasPrefix(stderr, "usage: keepsake") || !strings.HasSuffix(stderr, c.want) {
			t.Errorf("%v: exit %d:\n%s", c.args, code, stderr)
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"validate", "-h"}, {"serve", "--help"}} {
		if code, stdout, _ := run(t, args...); code != 0 || !strings.HasPrefix(stdout, "usage: keepsake") {
			t.Errorf("%v: exit %d: %s", args, code, stdout)
		}
	}
}

func TestValidatePrintsEveryErrorAndExitsOne(t *testing.T) {
	src := bundle(t, t.TempDir(), strings.Replace(doc, "convention.", "convention. [gone](./gone.md)", 1), "layers.md")
	code, _, stderr := run(t, "validate", src)
	if code != 1 || stderr != "architecture/layers: link to unknown concept architecture/gone\n" {
		t.Fatalf("exit %d: %q", code, stderr)
	}
}

func TestATenantIsReadAsPythonsUUIDReadsIt(t *testing.T) {
	want := uuid.MustParse("12345678-1234-5678-1234-567812345678")
	for _, v := range []string{
		"12345678-1234-5678-1234-567812345678",
		"{12345678-1234-5678-1234-567812345678}",
		"urn:uuid:12345678-1234-5678-1234-567812345678",
		"12345678123456781234567812345678",
		"1234-5678-1234-5678-1234-5678-1234-5678",
	} {
		if got, err := tenantID(v); err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	if _, err := tenantID("1234"); err == nil || err.Error() != "not a tenant uuid: '1234'" {
		t.Errorf("err = %v", err)
	}
}

func TestFilesAreReadInPythonsPathOrder(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a-b/x.md", "a/x.md", "a.md", "b.md"} {
		writeFile(t, dir, f, "---\ntype: Concept\n---\n")
	}
	cs, err := documents(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		got = append(got, c.Path)
	}
	// pathlib compares parts, so "a" sorts before "a-b" and "a.md".
	if want := []string{"a/x", "a-b/x", "a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// Ported from 2de90d2:tests/test_schema_name.py, driving the real command as Python does.
func TestANonDefaultSchemaMigratesAndServes(t *testing.T) {
	const schema = "okf_elsewhere"
	t.Setenv("KEEPSAKE_SCHEMA", schema)
	t.Setenv("KEEPSAKE_DSN", db.OwnerDSN)
	if code, _, stderr := run(t, "migrate"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	served := fakeListen(t)
	if code, _, stderr := run(t, "serve", "--dsn", db.AppDSN, "--tenant", uuid.NewString()); code != 0 || served.h == nil {
		t.Fatalf("serve exit %d: %s", code, stderr)
	}

	owner, err := pgx.Connect(ctx, db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	rows, _ := owner.Query(ctx, `SELECT relname FROM pg_class
		WHERE relnamespace = $1::regnamespace AND relkind = 'r'
		AND relrowsecurity AND relforcerowsecurity ORDER BY relname`, schema)
	guarded, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !reflect.DeepEqual(guarded, []string{"concept", "concept_revision"}) {
		t.Fatal(guarded, err)
	}

	s, err := store.Open(ctx, db.AppDSN, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := store.Verify(ctx, s, schema); err != nil {
		t.Fatal(err)
	}
	cs, tenant := store.NewConceptStore(s), uuid.New()
	if _, _, err := cs.Create(ctx, tenant, okf.Concept{Path: "a/b", Type: "Concept", Body: "here"}, "t"); err != nil {
		t.Fatal(err)
	}
	if c := read(t, cs, tenant, "a/b"); c == nil || c.Body != "here" {
		t.Fatalf("%+v", c)
	}
	// It landed in that schema rather than in okf. As the superuser, because FORCE
	// ROW LEVEL SECURITY subjects the owner to the policy too.
	admin, err := pgx.Connect(ctx, db.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var n int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+schema+".concept WHERE tenant_id = $1", tenant).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
