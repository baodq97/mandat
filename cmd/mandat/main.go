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
	case "init":
		return initCmd(args[1:], os.Stdin, stdout, stderr)
	case "git-credential":
		// The credential helper is a stdio filter: git writes its request on stdin,
		// so this one subcommand reads os.Stdin directly rather than through run's
		// writer-only seam. The protocol core (gitCredential) stays stdin-injectable
		// for tests.
		return gitCredentialCmd(args[1:], os.Stdin, stdout, stderr)
	case "remit-guard":
		// remit-guard is a Claude Code PreToolUse hook (ADR-0006 §Isolation): Claude
		// Code writes the tool-call JSON on stdin and reads the allow/deny decision
		// from stdout, so like git-credential this reads os.Stdin directly rather
		// than through run's writer-only seam. The core (remitGuard) stays
		// stdin-injectable for tests.
		return remitGuardCmd(args[1:], os.Stdin, stdout, stderr)
	default:
		return designGated(stderr)
	}
}

func designGated(stderr io.Writer) int {
	fmt.Fprintln(stderr, "mandat: only `version`, `serve`, `doctor`, `init`, `git-credential`, and `remit-guard` exist yet; other runtime is design-gated (see docs/)")
	return 2
}
