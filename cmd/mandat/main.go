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
	if len(args) == 0 {
		return designGated(stderr)
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "mandat "+buildinfo.Version())
		return 0
	case "serve":
		return serve(args[1:], stdout, stderr)
	case "doctor":
		return doctor(args[1:], stdout, stderr)
	case "git-credential":
		// The credential helper is a stdio filter: git writes its request on stdin,
		// so this one subcommand reads os.Stdin directly rather than through run's
		// writer-only seam. The protocol core (gitCredential) stays stdin-injectable
		// for tests.
		return gitCredentialCmd(args[1:], os.Stdin, stdout, stderr)
	default:
		return designGated(stderr)
	}
}

func designGated(stderr io.Writer) int {
	fmt.Fprintln(stderr, "mandat: only `version`, `serve`, `doctor`, and `git-credential` exist yet; other runtime is design-gated (see docs/)")
	return 2
}
