package workspace

import (
	"context"
	"io"
	"os/exec"
)

// SpawnSpec is one child invocation to run as a per-role OS user inside a
// worktree. RoleUser is the per-role OS account the child drops to; mandat never
// runs the agent as its own service user, so the OS-level file permissions of
// RoleUser bound what the child can touch on disk (spec §4.5: per-role OS user
// bounded by file permissions).
type SpawnSpec struct {
	RoleUser string
	Dir      string
	Argv     []string
	Env      []string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
}

// Spawner runs a child process as a per-role OS user. The real implementation
// drops privileges (systemd-run/setpriv), which needs root — absent on the dev
// and CI boxes — so it is exercised at integration; unit tests inject a fake to
// prove the call shape and the no-fallback error path without privilege (spec §9:
// every I/O seam gets a contract test).
type Spawner interface {
	Spawn(ctx context.Context, spec SpawnSpec) error
}

// DefaultSpawner runs children under the per-role OS user via systemd-run. It
// requires root and systemd; it is the integration-time spawner, not the unit
// path.
var DefaultSpawner Spawner = systemdRunSpawner{}

// systemdRunArgv wraps spec.Argv so it runs as spec.RoleUser via systemd-run:
// --pipe wires the child's stdio through, --wait blocks until it finishes and
// propagates the exit status, --collect reaps the transient unit even on
// failure, and --working-directory pins the child into the worktree. setpriv
// --reuid is the fallback where systemd is absent. Keeping the wrapping a pure
// function makes it unit-testable without root (the privilege drop is not).
func systemdRunArgv(spec SpawnSpec) []string {
	argv := []string{"systemd-run", "--uid=" + spec.RoleUser, "--pipe", "--collect", "--wait"}
	if spec.Dir != "" {
		argv = append(argv, "--working-directory="+spec.Dir)
	}
	argv = append(argv, "--")
	return append(argv, spec.Argv...)
}

type systemdRunSpawner struct{}

func (systemdRunSpawner) Spawn(ctx context.Context, spec SpawnSpec) error {
	argv := systemdRunArgv(spec)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr

	// A failure to start is an isolation-setup failure (could not drop to the
	// role user); it fires setup_failed with no fallback. A child that starts
	// and exits non-zero is the run outcome, not a setup fault, so its exit
	// error is returned raw for the runner (US-0006) to classify.
	if err := cmd.Start(); err != nil {
		return &SetupError{Op: "spawn", Err: err}
	}
	return cmd.Wait()
}
