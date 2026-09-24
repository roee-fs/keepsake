// Ported from 8f2af2e:tests/test_verify.py: every rejection here misconfigures the shared
// database and restores it in t.Cleanup, so later tests see a clean database. None of
// these tests run in parallel with each other or with isolation_test.go's.
package store_test

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/roee-fs/keepsake/internal/pgtest"
	"github.com/roee-fs/keepsake/internal/store"
)

// Restated from the migration: the tests that rewrite these policies restore them.
const (
	tenantQual = "tenant_id = current_setting('okf.current_tenant')::uuid"
	adminQual  = "tenant_id >= CASE WHEN current_setting('okf.admin', true) = 'on' " +
		"THEN '00000000-0000-0000-0000-000000000000'::uuid END"
)

var restoreTenantPolicy = fmt.Sprintf(
	"CREATE POLICY tenant_isolation ON okf.concept USING (%s) WITH CHECK (%s)", tenantQual, tenantQual)

// verifyDSN opens a store on dsn and runs Verify, without leaking the pool it opens.
func verifyDSN(t *testing.T, dsn, schema string) error {
	t.Helper()
	s, err := store.Open(ctx, dsn, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	return store.Verify(ctx, s, schema)
}

// asRole swaps the credentials in dsn for role's, keeping the same host, port and database.
func asRole(t *testing.T, dsn, role, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}

func assertMisconfigured(t *testing.T, err error, want string) {
	t.Helper()
	var md *store.MisconfiguredDatabase
	if !errors.As(err, &md) {
		t.Fatalf("err = %v, want *store.MisconfiguredDatabase", err)
	}
	if md.Msg != want {
		t.Fatalf("Msg = %q, want %q", md.Msg, want)
	}
}

// decoyDDL is DDL for a table shadowing the catalog table name, itself passing every check.
func decoyDDL(name, columns, values string) []string {
	return []string{
		fmt.Sprintf("CREATE TABLE okf.%s (%s, tenant_id uuid)", name, columns),
		fmt.Sprintf("INSERT INTO okf.%s VALUES (%s, gen_random_uuid())", name, values),
		fmt.Sprintf("ALTER TABLE okf.%s ENABLE ROW LEVEL SECURITY", name),
		fmt.Sprintf("ALTER TABLE okf.%s FORCE ROW LEVEL SECURITY", name),
		fmt.Sprintf("CREATE POLICY tenant_isolation ON okf.%s USING (%s)", name, tenantQual),
		fmt.Sprintf("GRANT SELECT ON okf.%s TO okf_app", name),
	}
}

// One per catalog table the check reads: a superuser role, an okf schema the app role
// owns, and an unprotected table in okf.
var decoys = []struct{ name, columns, values string }{
	{"pg_roles", "rolname name, rolsuper bool, rolbypassrls bool", "'okf_app', true, true"},
	{"pg_namespace", "nspname name, nspowner oid", "'okf', 'okf_app'::regrole::oid"},
	{
		"pg_class",
		`oid oid, relname name, relrowsecurity bool, relforcerowsecurity bool, ` +
			`relowner oid, relnamespace oid, relkind "char"`,
		`0, 'evil', false, false, 'okf_app'::regrole::oid, ` +
			`(SELECT oid FROM pg_catalog.pg_namespace WHERE nspname = 'okf'), 'r'`,
	},
}

func TestVerifyPassesForTheUnprivilegedRole(t *testing.T) {
	if err := verifyDSN(t, db.AppDSN, "okf"); err != nil {
		t.Fatal(err)
	}
}

// A correct install would crash-loop if alembic's table were held to the rule.
func TestVerifyPassesAlthoughTheBookkeepingTableHasNoForcedRLS(t *testing.T) {
	conn, err := pgx.Connect(ctx, db.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var enabled, forced bool
	err = conn.QueryRow(ctx,
		"SELECT relrowsecurity, relforcerowsecurity FROM pg_class c "+
			"JOIN pg_namespace n ON n.oid = c.relnamespace "+
			"WHERE n.nspname = 'okf' AND c.relname = 'alembic_version'",
	).Scan(&enabled, &forced)
	if err != nil {
		t.Fatal(err)
	}
	if enabled || forced {
		t.Fatalf("alembic_version no longer exercises the rule: enabled=%v forced=%v", enabled, forced)
	}
	if err := verifyDSN(t, db.AppDSN, "okf"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsASuperuser(t *testing.T) {
	err := verifyDSN(t, db.AdminDSN, "okf")
	assertMisconfigured(t, err, "okf must not connect as a superuser: RLS does not apply to superusers")
}

// BYPASSRLS is what an operator grants when isolation queries start failing.
func TestVerifyRejectsABypassrlsRole(t *testing.T) {
	const role, password = "okf_bypassrls", "bypassrls"
	pgtest.Exec(t, db.AdminDSN,
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER BYPASSRLS PASSWORD '%s'", role, password))
	t.Cleanup(func() { pgtest.Exec(t, db.AdminDSN, fmt.Sprintf("DROP ROLE %s", role)) })

	err := verifyDSN(t, asRole(t, db.AppDSN, role, password), "okf")
	assertMisconfigured(t, err,
		"okf must not connect as a role with BYPASSRLS: RLS does not apply to such a role")
}

func TestVerifyRejectsTheSchemaOwner(t *testing.T) {
	err := verifyDSN(t, db.OwnerDSN, "okf")
	assertMisconfigured(t, err,
		"okf must not connect as a role that owns the schema okf: an owner bypasses row-level security")
}

// Owning a table in someone else's schema is its own way out of RLS.
func TestVerifyRejectsATableOwner(t *testing.T) {
	const role, password, table = "okf_tableowner", "owns", "owned_elsewhere"
	pgtest.Exec(t, db.AdminDSN,
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '%s'", role, password),
		fmt.Sprintf("GRANT USAGE ON SCHEMA okf TO %s", role),
		fmt.Sprintf("CREATE TABLE okf.%s (tenant_id uuid NOT NULL)", table),
		fmt.Sprintf("ALTER TABLE okf.%s OWNER TO %s", table, role),
		fmt.Sprintf("ALTER TABLE okf.%s ENABLE ROW LEVEL SECURITY", table),
		fmt.Sprintf("ALTER TABLE okf.%s FORCE ROW LEVEL SECURITY", table),
	)
	t.Cleanup(func() {
		pgtest.Exec(t, db.AdminDSN,
			fmt.Sprintf("DROP TABLE okf.%s", table),
			fmt.Sprintf("REVOKE USAGE ON SCHEMA okf FROM %s", role),
			fmt.Sprintf("DROP ROLE %s", role),
		)
	})

	err := verifyDSN(t, asRole(t, db.AppDSN, role, password), "okf")
	assertMisconfigured(t, err,
		"okf must not connect as a role that owns okf.owned_elsewhere: "+
			"an owner may disable row-level security on its own table")
}

// A NOINHERIT member inherits nothing until it runs SET ROLE, and then it owns.
func TestVerifyRejectsANoinheritMemberOfTheSchemaOwner(t *testing.T) {
	const role, password = "okf_noinherit", "noinherit"
	pgtest.Exec(t, db.AdminDSN,
		fmt.Sprintf("CREATE ROLE %s LOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS PASSWORD '%s'", role, password),
		fmt.Sprintf("GRANT okf_owner TO %s", role),
	)
	t.Cleanup(func() {
		pgtest.Exec(t, db.AdminDSN,
			fmt.Sprintf("REVOKE okf_owner FROM %s", role),
			fmt.Sprintf("DROP ROLE %s", role),
		)
	})

	err := verifyDSN(t, asRole(t, db.AppDSN, role, password), "okf")
	assertMisconfigured(t, err,
		"okf must not connect as a role that owns the schema okf: an owner bypasses row-level security")
}

// A partitioned parent is relkind 'p', and holds the policy for its partitions.
func TestVerifyRejectsAPartitionedTableWithoutRLS(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN,
		"CREATE TABLE okf.parted (tenant_id uuid NOT NULL) PARTITION BY RANGE (tenant_id)")
	t.Cleanup(func() { pgtest.Exec(t, db.OwnerDSN, "DROP TABLE okf.parted") })

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err, "okf.parted has row-level security disabled")
}

// search_path lists pg_catalog after okf, so okf.pg_class wins an unqualified read. Each
// decoy lies in the direction that would reject a correct database, and carries RLS and a
// policy of its own so nothing but the shadowing can fail the check.
func TestVerifyShadowingTablesDoNotChangeTheVerdict(t *testing.T) {
	var create, drop []string
	for _, d := range decoys {
		create = append(create, decoyDDL(d.name, d.columns, d.values)...)
		drop = append(drop, fmt.Sprintf("DROP TABLE okf.%s", d.name))
	}
	pgtest.Exec(t, db.OwnerDSN, create...)
	t.Cleanup(func() { pgtest.Exec(t, db.OwnerDSN, drop...) })

	if err := verifyDSN(t, db.AppDSN, "okf"); err != nil {
		t.Fatal(err)
	}
}

// pg_user omits NOLOGIN roles, so this one reads as absent there, not as super.
func TestVerifyRejectsASuperuserReachedBySetRole(t *testing.T) {
	const role = "okf_masked"
	pgtest.Exec(t, db.AdminDSN,
		fmt.Sprintf("CREATE ROLE %s SUPERUSER NOLOGIN", role),
		fmt.Sprintf("GRANT %s TO okf_app", role),
	)
	t.Cleanup(func() {
		pgtest.Exec(t, db.AdminDSN,
			fmt.Sprintf("REVOKE %s FROM okf_app", role),
			fmt.Sprintf("DROP ROLE %s", role),
		)
	})

	dsn := db.AppDSN + "&options=-c%20role%3D" + role
	err := verifyDSN(t, dsn, "okf")
	assertMisconfigured(t, err, "okf must not connect as a superuser: RLS does not apply to superusers")
}

// USING (true) leaves RLS enabled and forced while serving every tenant's rows.
func TestVerifyRejectsAPermissivePolicy(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN, "ALTER POLICY tenant_isolation ON okf.concept USING (true)")
	t.Cleanup(func() {
		pgtest.Exec(t, db.OwnerDSN,
			fmt.Sprintf("ALTER POLICY tenant_isolation ON okf.concept USING (%s)", tenantQual))
	})

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err,
		"okf.concept policy tenant_isolation does not read okf.current_tenant, "+
			"so it does not restrict rows to one tenant")
}

// Each reads okf.current_tenant and still admits every tenant's rows.
func TestVerifyRejectsAWidenedTenantPolicy(t *testing.T) {
	for _, qual := range []string{
		fmt.Sprintf("%s OR true", tenantQual),
		"current_setting('okf.current_tenant') IS NOT NULL",
		"current_setting('okf.current_tenant', true) <> 'x'",
	} {
		t.Run(qual, func(t *testing.T) {
			pgtest.Exec(t, db.OwnerDSN, fmt.Sprintf("ALTER POLICY tenant_isolation ON okf.concept USING (%s)", qual))
			t.Cleanup(func() {
				pgtest.Exec(t, db.OwnerDSN,
					fmt.Sprintf("ALTER POLICY tenant_isolation ON okf.concept USING (%s)", tenantQual))
			})

			err := verifyDSN(t, db.AppDSN, "okf")
			assertMisconfigured(t, err,
				"okf.concept policy tenant_isolation does not read exactly "+
					"(tenant_id = (current_setting('okf.current_tenant'::text))::uuid)")
		})
	}
}

// A correct USING with WITH CHECK (true) reads one tenant and writes any.
func TestVerifyRejectsAPermissiveWithCheck(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN, "ALTER POLICY tenant_isolation ON okf.concept WITH CHECK (true)")
	t.Cleanup(func() {
		pgtest.Exec(t, db.OwnerDSN,
			fmt.Sprintf("ALTER POLICY tenant_isolation ON okf.concept WITH CHECK (%s)", tenantQual))
	})

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err,
		"okf.concept policy tenant_isolation does not read okf.current_tenant, "+
			"so it does not restrict rows to one tenant")
}

// The exemption is for one policy shape, not for "any second policy".
func TestVerifyRejectsAWidenedAdminPolicy(t *testing.T) {
	for _, qual := range []string{
		"true",
		// Each reads okf.admin and still admits every tenant's rows to every read.
		"current_setting('okf.admin', true) IS NOT NULL",
		"current_setting('okf.admin', true) = 'on' OR true",
		"current_setting('okf.administrator', true) IS NULL",
	} {
		t.Run(qual, func(t *testing.T) {
			pgtest.Exec(t, db.OwnerDSN, fmt.Sprintf("ALTER POLICY admin_read ON okf.concept USING (%s)", qual))
			t.Cleanup(func() {
				pgtest.Exec(t, db.OwnerDSN, fmt.Sprintf("ALTER POLICY admin_read ON okf.concept USING (%s)", adminQual))
			})

			// The message MUST name the clause that failed, not the first one checked.
			err := verifyDSN(t, db.AppDSN, "okf")
			assertMisconfigured(t, err,
				"okf.concept policy admin_read is the admin_read exemption but does not read exactly "+
					"(tenant_id >= CASE WHEN (current_setting('okf.admin'::text, true) = 'on'::text) "+
					"THEN '00000000-0000-0000-0000-000000000000'::uuid ELSE NULL::uuid END)")
		})
	}
}

// ANDed with tenant_isolation, it would hide every row from every tenant.
func TestVerifyRejectsARestrictiveAdminPolicy(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN,
		"DROP POLICY admin_read ON okf.concept",
		fmt.Sprintf("CREATE POLICY admin_read ON okf.concept AS RESTRICTIVE FOR SELECT USING (%s)", adminQual),
	)
	t.Cleanup(func() {
		pgtest.Exec(t, db.OwnerDSN,
			"DROP POLICY admin_read ON okf.concept",
			fmt.Sprintf("CREATE POLICY admin_read ON okf.concept FOR SELECT USING (%s)", adminQual),
		)
	})

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err,
		"okf.concept policy admin_read is the admin_read exemption but is RESTRICTIVE, "+
			"so it hides every row from every tenant")
}

// FOR ALL on the admin GUC would let an admin write into any tenant. Recreated rather
// than altered: ALTER POLICY cannot change which commands a policy applies to.
func TestVerifyRejectsAnAdminPolicyThatCoversWrites(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN,
		"DROP POLICY admin_read ON okf.concept_revision",
		fmt.Sprintf("CREATE POLICY admin_read ON okf.concept_revision USING (%s)", adminQual),
	)
	t.Cleanup(func() {
		pgtest.Exec(t, db.OwnerDSN,
			"DROP POLICY admin_read ON okf.concept_revision",
			fmt.Sprintf("CREATE POLICY admin_read ON okf.concept_revision FOR SELECT USING (%s)", adminQual),
		)
	})

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err,
		"okf.concept_revision policy admin_read is the admin_read exemption but is not FOR SELECT, "+
			"so it would admit another tenant's rows to a write")
}

// FOR INSERT carries no USING, and rejecting it would crash-loop a correct install.
func TestVerifyAcceptsAPolicyWithOnlyAWithCheckClause(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN,
		fmt.Sprintf("CREATE POLICY insert_only ON okf.concept FOR INSERT WITH CHECK (%s)", tenantQual))
	t.Cleanup(func() { pgtest.Exec(t, db.OwnerDSN, "DROP POLICY insert_only ON okf.concept") })

	if err := verifyDSN(t, db.AppDSN, "okf"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsATableWithNoPolicy(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN, "DROP POLICY tenant_isolation ON okf.concept")
	t.Cleanup(func() { pgtest.Exec(t, db.OwnerDSN, restoreTenantPolicy) })

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err, "okf.concept has no row-level security policy scoping it to one tenant")
}

func TestVerifyRejectsAMissingSchema(t *testing.T) {
	err := verifyDSN(t, db.AppDSN, "okf_absent")
	assertMisconfigured(t, err, "okf found no schema named okf_absent")
}

func TestVerifyRejectsATableWithRLSDisabled(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN, "ALTER TABLE okf.concept DISABLE ROW LEVEL SECURITY")
	t.Cleanup(func() { pgtest.Exec(t, db.OwnerDSN, "ALTER TABLE okf.concept ENABLE ROW LEVEL SECURITY") })

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err, "okf.concept has row-level security disabled")
}

func TestVerifyRejectsATableWithoutForcedRLS(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN, "ALTER TABLE okf.concept NO FORCE ROW LEVEL SECURITY")
	t.Cleanup(func() { pgtest.Exec(t, db.OwnerDSN, "ALTER TABLE okf.concept FORCE ROW LEVEL SECURITY") })

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err, "okf.concept does not FORCE row-level security")
}

// A restrictive tenant policy is ANDed with admin_read, so no tenant can read a row.
func TestVerifyRejectsARestrictiveTenantPolicy(t *testing.T) {
	pgtest.Exec(t, db.OwnerDSN,
		"DROP POLICY tenant_isolation ON okf.concept",
		fmt.Sprintf("CREATE POLICY tenant_isolation ON okf.concept AS RESTRICTIVE USING (%s) WITH CHECK (%s)",
			tenantQual, tenantQual),
	)
	t.Cleanup(func() {
		pgtest.Exec(t, db.OwnerDSN, "DROP POLICY tenant_isolation ON okf.concept", restoreTenantPolicy)
	})

	err := verifyDSN(t, db.AppDSN, "okf")
	assertMisconfigured(t, err,
		"okf.concept has no row-level security policy scoping it to one tenant")
}
