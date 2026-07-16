package workspace

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"slices"
	"testing"
)

func TestSystemdRunArgv_WrapsChildAsRoleUser(t *testing.T) {
	t.Parallel()

	got := systemdRunArgv(SpawnSpec{
		RoleUser: "mandat-dev",
		Dir:      "/var/lib/mandat/tasks/ado-baodo0220-42",
		Argv:     []string{"claude", "-p", "--bare"},
	})
	want := []string{
		"systemd-run", "--uid=mandat-dev", "--pipe", "--collect", "--wait",
		"--working-directory=/var/lib/mandat/tasks/ado-baodo0220-42",
		"--", "claude", "-p", "--bare",
	}
	if !slices.Equal(got, want) {
		t.Errorf("systemdRunArgv() = %v, want %v", got, want)
	}
}

// fakeSpawner is the no-root double for the OS-user spawn seam: it records the
// spec it was handed and returns a configured error, so a test can assert the
// call shape and the no-fallback error path without dropping privileges.
type fakeSpawner struct {
	got SpawnSpec
	err error
}

func (f *fakeSpawner) Spawn(_ context.Context, spec SpawnSpec) error {
	f.got = spec
	return f.err
}

func TestSpawner_FakeSurfacesSpawnFailure(t *testing.T) {
	t.Parallel()

	f := &fakeSpawner{err: errors.New("systemd-run: unknown user")}
	var spawner Spawner = f

	spec := SpawnSpec{RoleUser: "mandat-dev", Dir: "/wt", Argv: []string{"claude"}}
	err := spawner.Spawn(context.Background(), spec)
	if err == nil {
		t.Fatal("Spawn() on a failing spawn = nil, want an error (isolation setup failure, no fallback)")
	}
	if f.got.RoleUser != "mandat-dev" || f.got.Dir != "/wt" {
		t.Errorf("Spawn() received spec = %+v, want RoleUser=mandat-dev Dir=/wt", f.got)
	}
}

func TestDirectSpawner_RunsChildAsCurrentUserAndCapturesStdout(t *testing.T) {
	t.Parallel()

	sh := lookPathOrSkip(t, "sh")
	var out bytes.Buffer
	// RoleUser is set but must be ignored: DirectSpawner runs as the current user
	// with no privilege drop, so a child that exits 0 succeeds with its stdout captured.
	err := DirectSpawner.Spawn(context.Background(), SpawnSpec{
		RoleUser: "mandat-dev",
		Argv:     []string{sh, "-c", "printf hello"},
		Stdout:   &out,
	})
	if err != nil {
		t.Fatalf("Spawn() on a child that exits 0 = %v, want nil", err)
	}
	if out.String() != "hello" {
		t.Errorf("captured stdout = %q, want %q", out.String(), "hello")
	}
}

func TestDirectSpawner_MissingBinaryIsSetupError(t *testing.T) {
	t.Parallel()

	err := DirectSpawner.Spawn(context.Background(), SpawnSpec{
		Argv: []string{"mandat-no-such-binary-xyz"},
	})
	var se *SetupError
	if !errors.As(err, &se) {
		t.Fatalf("Spawn() on a missing binary = %v, want *SetupError (isolation setup failure, no fallback)", err)
	}
	if se.Op != "spawn" {
		t.Errorf("SetupError.Op = %q, want %q", se.Op, "spawn")
	}
}

func TestDirectSpawner_ChildSeesSpecEnv(t *testing.T) {
	t.Parallel()

	sh := lookPathOrSkip(t, "sh")
	var out bytes.Buffer
	// cmd.Env = spec.Env, so the child sees exactly the injected environment (the
	// runner passes the per-task HOME, config dir, and token-helper wiring this way).
	err := DirectSpawner.Spawn(context.Background(), SpawnSpec{
		Argv:   []string{sh, "-c", `printf %s "$MANDAT_TEST_VAR"`},
		Env:    []string{"MANDAT_TEST_VAR=isolation-env"},
		Stdout: &out,
	})
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	if out.String() != "isolation-env" {
		t.Errorf("child stdout = %q, want the injected env value %q", out.String(), "isolation-env")
	}
}

func lookPathOrSkip(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is required to exercise the direct-exec spawner", name)
	}
	return p
}
