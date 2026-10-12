package keepsake_test

import (
	"os/exec"
	"strings"
	"testing"
)

const mod = "github.com/roee-fs/keepsake/"

// imports lists pkg's direct imports, or with transitive set its whole dependency closure.
func imports(t *testing.T, pkg string, transitive bool) []string {
	args := []string{"list", "-f", `{{join .Imports " "}}`, pkg}
	if transitive {
		args = []string{"list", "-deps", pkg}
	}
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

// layers lists the module packages each package MUST NOT reach.
var layers = []struct {
	pkg        string
	transitive bool
	banned     []string
}{
	// okf MUST be publishable without keepsake.
	{"./okf", true, []string{"internal/"}},
	{"./internal/models", true, []string{"internal/"}},
	{"./internal/repositories", true, []string{"internal/services", "internal/controllers", "internal/app", "internal/cli"}},
	{"./internal/services", true, []string{"internal/controllers", "internal/app", "internal/cli"}},
	// Direct only: controllers reach the repositories through services.
	{"./internal/controllers", false, []string{"internal/repositories", "internal/app", "internal/cli"}},
	{"./internal/app", true, []string{"internal/cli"}},
}

func TestLayering(t *testing.T) {
	for _, l := range layers {
		self := mod + strings.TrimPrefix(l.pkg, "./")
		for _, d := range imports(t, l.pkg, l.transitive) {
			for _, b := range l.banned {
				if d != self && strings.HasPrefix(d, mod+b) {
					t.Errorf("%s imports %s", l.pkg, d)
				}
			}
		}
	}
}

func TestServicesAreHTTPAgnostic(t *testing.T) {
	for _, imp := range imports(t, "./internal/services", false) {
		if imp == "net/http" || strings.HasPrefix(imp, "github.com/modelcontextprotocol/") {
			t.Errorf("internal/services imports %s", imp)
		}
	}
}
