package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Every catalog read below is schema-qualified: search_path names pg_catalog explicitly,
// so it is searched in listed order and a table named okf.pg_class would shadow it.

// roleQuery reads pg_roles, not pg_user: pg_user omits NOLOGIN roles, which SET ROLE can reach.
const roleQuery = `SELECT rolsuper, rolbypassrls FROM pg_catalog.pg_roles WHERE rolname = current_user`

// schemaOwnerQuery checks MEMBER, not USAGE: a NOINHERIT member holds none of the owner's
// privileges until it runs SET ROLE, and may run it at any time.
const schemaOwnerQuery = `SELECT pg_catalog.pg_has_role(nspowner, 'MEMBER') FROM pg_catalog.pg_namespace WHERE nspname = $1`

// tablesQuery looks for a tenant_id column, not a name: the rule is "every table holding
// tenant data", and an exemption list is how a table that does hold it gets waved through.
// Alembic's bookkeeping table has no such column, so it stays out without being named.
const tablesQuery = `
	SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
	       pg_catalog.pg_has_role(c.relowner, 'MEMBER')
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
	  AND EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
	              WHERE a.attrelid = c.oid AND a.attname = 'tenant_id'
	                AND NOT a.attisdropped)`

// policiesQuery reads both expressions: USING alone leaves WITH CHECK (true) free to admit
// another tenant's inserts, and an INSERT-only policy carries no USING at all.
const policiesQuery = `
	SELECT c.relname, p.polname, p.polcmd,
	       pg_catalog.pg_get_expr(p.polqual, p.polrelid),
	       pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid)
	FROM pg_catalog.pg_policy p
	JOIN pg_catalog.pg_class c ON c.oid = p.polrelid
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = $1`

// selectOnly is pg_policy.polcmd for a policy applying to SELECT alone. '*' is ALL, and a
// policy reaching a write is not the exemption.
const selectOnly = "r"

// MisconfiguredDatabase is raised at startup. Crashing loudly beats serving cross-tenant reads.
type MisconfiguredDatabase struct{ Msg string }

func (e *MisconfiguredDatabase) Error() string { return e.Msg }

func misconfigured(format string, args ...any) error {
	return &MisconfiguredDatabase{Msg: fmt.Sprintf(format, args...)}
}

func containsAll(expressions []string, guc string) bool {
	for _, e := range expressions {
		if !strings.Contains(e, guc) {
			return false
		}
	}
	return true
}

// policyFault says why a policy fails to confine the rows it admits, or "" if it does.
//
// The sentence is read off a crash-looping pod, so each case names the clause that
// actually failed.
func policyFault(policy, cmd string, expressions []string) string {
	if len(expressions) == 0 {
		return "applies no expression, so it admits every row"
	}
	if containsAll(expressions, TenantGUC) {
		return ""
	}
	if policy != AdminPolicy {
		return fmt.Sprintf("does not read %s, so it does not restrict rows to one tenant", TenantGUC)
	}
	// The single exemption, for the admin console's cross-tenant read. Pinned to the
	// name, the command and the GUC: widen any one and a policy that admits another
	// tenant's rows to a write starts passing this check.
	if cmd != selectOnly {
		return fmt.Sprintf("is the %s exemption but is not FOR SELECT, so it would "+
			"admit another tenant's rows to a write", AdminPolicy)
	}
	if !containsAll(expressions, AdminGUC) {
		return fmt.Sprintf("is the %s exemption but reads neither %s nor "+
			"%s, so nothing gates the rows it admits", AdminPolicy, TenantGUC, AdminGUC)
	}
	return ""
}

type policyRow struct {
	name  string
	cmd   string
	exprs []string
}

// Verify raises unless row-level security actually constrains the connected role.
func Verify(ctx context.Context, s *Store, schema string) error {
	return s.Raw(ctx, func(tx pgx.Tx) error {
		var super, bypass bool
		if err := tx.QueryRow(ctx, roleQuery).Scan(&super, &bypass); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return misconfigured("okf cannot verify the connected role: it is absent from pg_roles")
			}
			return err
		}
		if super {
			return misconfigured("okf must not connect as a superuser: RLS does not apply to superusers")
		}
		if bypass {
			return misconfigured("okf must not connect as a role with BYPASSRLS: " +
				"RLS does not apply to such a role")
		}

		var ownsSchema bool
		if err := tx.QueryRow(ctx, schemaOwnerQuery, schema).Scan(&ownsSchema); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return misconfigured("okf found no schema named %s", schema)
			}
			return err
		}
		if ownsSchema {
			return misconfigured("okf must not connect as a role that owns the schema %s: "+
				"an owner bypasses row-level security", schema)
		}

		policies := map[string][]policyRow{}
		polRows, err := tx.Query(ctx, policiesQuery, schema)
		if err != nil {
			return err
		}
		for polRows.Next() {
			var table, name, cmd string
			var qual, check *string
			if err := polRows.Scan(&table, &name, &cmd, &qual, &check); err != nil {
				polRows.Close()
				return err
			}
			// A null expression is one Postgres does not apply, not an empty one.
			var exprs []string
			if qual != nil {
				exprs = append(exprs, *qual)
			}
			if check != nil {
				exprs = append(exprs, *check)
			}
			policies[table] = append(policies[table], policyRow{name, cmd, exprs})
		}
		polRows.Close()
		if err := polRows.Err(); err != nil {
			return err
		}

		tblRows, err := tx.Query(ctx, tablesQuery, schema)
		if err != nil {
			return err
		}
		defer tblRows.Close()
		for tblRows.Next() {
			var name string
			var enabled, forced, owned bool
			if err := tblRows.Scan(&name, &enabled, &forced, &owned); err != nil {
				return err
			}
			if owned {
				return misconfigured("okf must not connect as a role that owns %s.%s: "+
					"an owner may disable row-level security on its own table", schema, name)
			}
			if !enabled {
				return misconfigured("%s.%s has row-level security disabled", schema, name)
			}
			if !forced {
				return misconfigured("%s.%s does not FORCE row-level security", schema, name)
			}
			// A tenant policy, not merely a policy: admin_read on its own leaves the
			// table unreadable by every tenant while the console still reads all of it.
			hasTenantPolicy := false
			for _, p := range policies[name] {
				if hasTenantPolicy {
					break
				}
				for _, e := range p.exprs {
					if strings.Contains(e, TenantGUC) {
						hasTenantPolicy = true
						break
					}
				}
			}
			if !hasTenantPolicy {
				return misconfigured("%s.%s has no row-level security policy scoping it"+
					" to one tenant", schema, name)
			}
			// Permissive policies are ORed, so one that ignores the GUC opens the table
			// however strict its siblings are. Every expression it does apply must
			// read a GUC: reads and writes are gated by different ones.
			for _, p := range policies[name] {
				if fault := policyFault(p.name, p.cmd, p.exprs); fault != "" {
					return misconfigured("%s.%s policy %s %s", schema, name, p.name, fault)
				}
			}
		}
		return tblRows.Err()
	})
}
