// Command keepsake operates an OKF knowledge store.
package main

import (
	"os"

	"github.com/roee-fs/keepsake/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
