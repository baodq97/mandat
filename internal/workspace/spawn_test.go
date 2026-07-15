package workspace

import (
	"context"
	"errors"
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
