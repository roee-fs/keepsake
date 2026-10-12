package keepsake_test

import (
	"os/exec"
	"strings"
	"testing"
)

const mod = "github.com/roee-fs/keepsake/"

func deps(t *testing.T, pkg string) []string {
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

// layers maps each package to the module packages it MUST NOT reach, even transitively.
var layers = map[string][]string{
	// okf MUST be publishable without keepsake.
	"./okf":             {"internal/"},
	"./internal/models": {"internal/"},
	"./internal/store":  {"internal/server", "internal/cli"},
}

func TestLayering(t *testing.T) {
	for pkg, banned := range layers {
		self := mod + strings.TrimPrefix(pkg, "./")
		for _, d := range deps(t, pkg) {
			for _, b := range banned {
				if d != self && strings.HasPrefix(d, mod+b) {
					t.Errorf("%s imports %s", pkg, d)
				}
			}
		}
	}
}
