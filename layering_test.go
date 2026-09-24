package keepsake_test

import (
	"os/exec"
	"strings"
	"testing"
)

func deps(t *testing.T, pkg string) []string {
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

func TestOkfIsALeaf(t *testing.T) {
	for _, d := range deps(t, "./okf") {
		if strings.HasPrefix(d, "github.com/roee-fs/keepsake/internal") {
			t.Errorf("okf imports %s: okf MUST be publishable without keepsake", d)
		}
	}
}

func TestStoreDoesNotImportTheServer(t *testing.T) {
	for _, d := range deps(t, "./internal/store") {
		if strings.Contains(d, "/internal/server") || strings.Contains(d, "/internal/cli") {
			t.Errorf("internal/store imports %s", d)
		}
	}
}
