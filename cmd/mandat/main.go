// Command mandat is the Mandat control plane: it turns tracker work items
// into reviewed pull requests through AI role agents acting under scoped,
// revocable Entra agent-identity mandates.
//
// Only the harness-proving skeleton exists today; every runtime capability
// is design-gated by the governed docs under docs/.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/baodq97/mandat/internal/buildinfo"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, "mandat "+buildinfo.Version())
		return 0
	}
	fmt.Fprintln(stderr, "mandat: only `mandat version` exists yet; the runtime is design-gated (see docs/)")
	return 2
}
