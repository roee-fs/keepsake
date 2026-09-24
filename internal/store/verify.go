package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Catalog reads are qualified: search_path lists okf first, so okf.pg_class would shadow pg_catalog's.

// roleQuery reads pg_roles, not pg_user: pg_user omits NOLOGIN roles, which SET ROLE can reach.
const roleQuery = `SELECT rolsuper, rolbypassrls FROM pg_catalog.pg_roles WHERE rolname = current_user`

// schemaOwnerQuery checks MEMBER, not USAGE: a NOINHERIT member may SET ROLE to the owner at any time.
const schemaOwnerQuery = `SELECT pg_catalog.pg_has_role(nspowner, 'MEMBER') FROM pg_catalog.pg_namespace WHERE nspname = $1`

// tablesQuery finds tenant tables by their tenant_id column, never by name, which leaves alembic_version out.
const tablesQuery = `
	SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
	       pg_catalog.pg_has_role(c.relowner, 'MEMBER')
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
	  AND EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
	              WHERE a.attrelid = c.oid AND a.attname = 'tenant_id'
	                AND NOT a.attisdropped)`

// policiesQuery reads USING and WITH CHECK: either alone can admit another tenant's rows.
const policiesQuery = `
	SELECT c.relname, p.polname, p.polcmd, p.polpermissive,
	       pg_catalog.pg_get_expr(p.polqual, p.polrelid),
	       pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid)
	FROM pg_catalog.pg_policy p
	JOIN pg_catalog.pg_class c ON c.oid = p.polrelid
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = $1`

// selectOnly is pg_policy.polcmd for SELECT alone; a policy reaching a write is not the exemption.
const selectOnly = "r"

// adminQual is admin_read as migration 0004 writes it, whitespace collapsed; nothing looser passes.
var adminQual = "(tenant_id >= CASE WHEN (current_setting('" + AdminGUC + "'::text, true) = " +
	"'on'::text) THEN '" + nilTenant + "'::uuid ELSE NULL::uuid END)"

// MisconfiguredDatabase is raised at startup. Crashing loudly beats serving cross-tenant reads.
type MisconfiguredDatabase struct{ Msg string }

func (e *MisconfiguredDatabase) Error() string { return e.Msg }

func misconfigured(format string, args ...any) error {
	return &MisconfiguredDatabase{Msg: fmt.Sprintf(format, args...)}
}

// tenantExpr is tenant_isolation as migration 0001 writes it, whitespace collapsed.
var tenantExpr = "(tenant_id = (current_setting('" + TenantGUC + "'::text))::uuid)"

func readsTenant(expr string) bool { return strings.Contains(expr, TenantGUC) }

func collapse(expr string) string { return strings.Join(strings.Fields(expr), " ") }

// policyFault names the clause through which a policy fails to confine its rows, or returns "".
func policyFault(p policyRow) string {
	if len(p.exprs) == 0 {
		return "applies no expression, so it admits every row"
	}
	if p.name != AdminPolicy {
		if slices.ContainsFunc(p.exprs, func(e string) bool { return !readsTenant(e) }) {
			return fmt.Sprintf("does not read %s, so it does not restrict rows to one tenant", TenantGUC)
		}
		// Permissive policies are ORed, so any looser expression widens every read.
		if p.permissive && slices.ContainsFunc(p.exprs, func(e string) bool { return collapse(e) != tenantExpr }) {
			return fmt.Sprintf("does not read exactly %s", tenantExpr)
		}
		return ""
	}
	// The admin console's exemption, pinned to name, command, permissiveness and exact expression.
	if p.cmd != selectOnly {
		return fmt.Sprintf("is the %s exemption but is not FOR SELECT, so it would "+
			"admit another tenant's rows to a write", AdminPolicy)
	}
	if !p.permissive {
		return fmt.Sprintf("is the %s exemption but is RESTRICTIVE, so it hides every "+
			"row from every tenant", AdminPolicy)
	}
	if len(p.exprs) != 1 || collapse(p.exprs[0]) != adminQual {
		return fmt.Sprintf("is the %s exemption but does not read exactly %s", AdminPolicy, adminQual)
	}
	return ""
}

type policyRow struct {
	name       string
	cmd        string
	permissive bool
	exprs      []string
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
			var permissive bool
			var qual, check *string
			if err := polRows.Scan(&table, &name, &cmd, &permissive, &qual, &check); err != nil {
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
			policies[table] = append(policies[table], policyRow{name, cmd, permissive, exprs})
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
			// A permissive tenant policy: admin_read alone, or a restrictive one, leaves the table unreadable.
			if !slices.ContainsFunc(policies[name], func(p policyRow) bool {
				return p.permissive && slices.ContainsFunc(p.exprs, readsTenant)
			}) {
				return misconfigured("%s.%s has no row-level security policy scoping it"+
					" to one tenant", schema, name)
			}
			// Permissive policies are ORed, so every expression of every one MUST read a GUC.
			for _, p := range policies[name] {
				if fault := policyFault(p); fault != "" {
					return misconfigured("%s.%s policy %s %s", schema, name, p.name, fault)
				}
			}
		}
		return tblRows.Err()
	})
}
