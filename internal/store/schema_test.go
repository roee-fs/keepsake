package store

import (
	"os"
	"testing"
)

// Ported from 2de90d2:tests/test_migration.py::test_a_schema_name_that_is_not_an_identifier_is_rejected
// and 2de90d2:tests/test_schema_name.py::test_an_unusable_schema_name_is_refused_before_it_reaches_ddl.
func TestValidatedSchemaRejectsNonIdentifiers(t *testing.T) {
	for _, name := range []string{
		"okf; DROP TABLE concept", "public.okf", "OKF", "", "1okf",
		"Okf", "okf-other", "okf okf",
	} {
		if _, err := ValidatedSchema(name); err == nil {
			t.Errorf("ValidatedSchema(%q) = nil error, want one", name)
		}
	}
}

func TestValidatedSchemaAcceptsBareIdentifiers(t *testing.T) {
	for _, name := range []string{"okf", "okf_elsewhere", "_okf", "okf2"} {
		got, err := ValidatedSchema(name)
		if err != nil || got != name {
			t.Errorf("ValidatedSchema(%q) = %q, %v", name, got, err)
		}
	}
}

func TestSchemaDefaultsToOkf(t *testing.T) {
	t.Setenv("KEEPSAKE_SCHEMA", "")
	os.Unsetenv("KEEPSAKE_SCHEMA")
	if got, err := Schema(); err != nil || got != "okf" {
		t.Errorf("Schema() = %q, %v, want okf", got, err)
	}
}

func TestSchemaRefusesAnUnusableName(t *testing.T) {
	for _, name := range []string{"not-usable", ""} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KEEPSAKE_SCHEMA", name)
			_, err := Schema()
			if want := "not a usable schema name: '" + name + "'"; err == nil || err.Error() != want {
				t.Errorf("err = %v, want %s", err, want)
			}
		})
	}
}

func TestPoolSizeDefaultsToTen(t *testing.T) {
	t.Setenv("KEEPSAKE_POOL_SIZE", "")
	n, err := PoolSize()
	if err != nil || n != 10 {
		t.Errorf("PoolSize() = %d, %v, want 10, nil", n, err)
	}
}

func TestPoolSizeRejectsNonPositiveIntegers(t *testing.T) {
	for _, value := range []string{"0", "-1", "abc", "1.5", "+1", "2147483648"} {
		t.Setenv("KEEPSAKE_POOL_SIZE", value)
		if _, err := PoolSize(); err == nil {
			t.Errorf("PoolSize() with KEEPSAKE_POOL_SIZE=%q = nil error, want one", value)
		}
	}
}
