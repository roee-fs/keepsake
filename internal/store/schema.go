// Package store owns the SQL and connection handling: every statement keepsake
// sends to Postgres. Ported from 2de90d2:src/keepsake/store/__init__.py.
package store

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/roee-fs/keepsake/okf"
)

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ValidatedSchema rejects anything not a bare identifier: the name is formatted into DDL.
func ValidatedSchema(name string) (string, error) {
	if !schemaName.MatchString(name) {
		return "", fmt.Errorf("not a usable schema name: %s", okf.PyReprString(name))
	}
	return name, nil
}

// The only GUCs a policy may key on. The policy, the connection that sets one and the
// startup check that asserts the policy reads it must all name the same string.
const (
	TenantGUC = "okf.current_tenant"
	AdminGUC  = "okf.admin"
)

// AdminPolicy is the one policy the startup check exempts from reading TenantGUC. The
// migration creating it and the check recognising it must agree on the name.
const AdminPolicy = "admin_read"

// Schema reads KEEPSAKE_SCHEMA, defaulting to "okf" only when it is unset.
func Schema() (string, error) {
	name, ok := os.LookupEnv("KEEPSAKE_SCHEMA")
	if !ok {
		name = "okf"
	}
	return ValidatedSchema(name)
}

// PoolSize reads KEEPSAKE_POOL_SIZE, defaulting to 10. It refuses a bad value with a
// sentence rather than a strconv error.
func PoolSize() (int, error) {
	value := cmp.Or(os.Getenv("KEEPSAKE_POOL_SIZE"), "10")
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || !okf.IsDecimal(value) {
		return 0, fmt.Errorf("KEEPSAKE_POOL_SIZE must be a positive integer: %s", okf.PyReprString(value))
	}
	return n, nil
}
