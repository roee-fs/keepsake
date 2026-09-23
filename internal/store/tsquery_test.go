// package store, not store_test: unqualified tsquery is unexported, and this test
// needs no database, so it does not share isolation_test.go's TestMain.
package store

import (
	"encoding/json"
	"os"
	"testing"
)

type tsqueryGolden struct {
	Query string `json:"query"`
	Terms string `json:"terms"`
}

func loadTsqueryGoldens(t *testing.T) []tsqueryGolden {
	t.Helper()
	data, err := os.ReadFile("../../okf/testdata/goldens/tsquery.json")
	if err != nil {
		t.Fatal(err)
	}
	var goldens []tsqueryGolden
	if err := json.Unmarshal(data, &goldens); err != nil {
		t.Fatal(err)
	}
	return goldens
}

func TestTsqueryKeepsUnicodeWords(t *testing.T) {
	for _, g := range loadTsqueryGoldens(t) { // okf/testdata/goldens/tsquery.json
		if got := tsquery(g.Query); got != g.Terms {
			t.Errorf("tsquery(%q) = %q, python %q", g.Query, got, g.Terms)
		}
	}
}
