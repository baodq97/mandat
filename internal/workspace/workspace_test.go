package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baodq97/mandat/internal/task"
)

// newBareOrigin builds the §9 double: a local bare git repo with a known tree
// and no network. It seeds two in-remit trees (cmd/mandat, internal/buildinfo)
// and two out-of-remit paths (secrets/leak.txt, README.md) so a sparse checkout
// scoped to the remit provably omits the latter. It returns the bare repo path.
func newBareOrigin(t *testing.T) string {
	t.Helper()

	work := t.TempDir()
	git(t, work, "init", "-b", "main")
	writeFile(t, filepath.Join(work, "cmd/mandat/main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(work, "internal/buildinfo/build.go"), "package buildinfo\n")
	writeFile(t, filepath.Join(work, "secrets/leak.txt"), "out of remit\n")
	writeFile(t, filepath.Join(work, "README.md"), "# out of remit\n")
	git(t, work, "add", "-A")
	git(t, work, "commit", "-m", "seed")

	origin := filepath.Join(t.TempDir(), "origin.git")
	git(t, "", "clone", "--bare", work, origin)
	return origin
}

// provisionForTest provisions a worktree against a fresh bare origin with a
// remit of cmd/mandat/ and internal/buildinfo/, the fixture every diff case
// starts from.
func provisionForTest(t *testing.T) *Workspace {
	t.Helper()

	cfg := Config{
		RepoURL:   newBareOrigin(t),
		MirrorDir: filepath.Join(t.TempDir(), "mirror.git"),
		TasksRoot: t.TempDir(),
		TaskID:    "ado-baodo0220-42",
		Remit:     task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}},
	}
	ws, err := Provision(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Provision() error = %v, want nil", err)
	}
	return ws
}

func TestProvision_SparseCheckoutScopesToRemit(t *testing.T) {
	t.Parallel()

	ws := provisionForTest(t)

	// In-remit trees are materialized (RFC-0001 AC-10 / AC-5.1).
	assertExists(t, filepath.Join(ws.Dir, "cmd/mandat/main.go"))
	assertExists(t, filepath.Join(ws.Dir, "internal/buildinfo/build.go"))
	// Out-of-remit paths are never materialized: the agent cannot see them.
	assertAbsent(t, filepath.Join(ws.Dir, "secrets/leak.txt"))
	assertAbsent(t, filepath.Join(ws.Dir, "README.md"))
}

func TestDiffInsideRemit_InRemitChangePasses(t *testing.T) {
	t.Parallel()

	ws := provisionForTest(t)
	// Scripted edit inside the remit (RFC-0001 AC-16 / AC-5.2).
	appendFile(t, filepath.Join(ws.Dir, "cmd/mandat/main.go"), "\n// edited inside the remit\n")

	if err := ws.DiffInsideRemit(context.Background()); err != nil {
		t.Fatalf("DiffInsideRemit() for an in-remit change = %v, want nil", err)
	}
}

func TestDiffInsideRemit_OutOfRemitChangeFailsNamingPath(t *testing.T) {
	t.Parallel()

	ws := provisionForTest(t)
	// Scripted edit outside the remit: a new path the sparse checkout never
	// materialized, so it lands as an untracked file (RFC-0001 AC-17 / AC-5.3).
	writeFile(t, filepath.Join(ws.Dir, "internal/secret/leak.go"), "package secret\n")

	err := ws.DiffInsideRemit(context.Background())
	var rv *RemitViolationError
	if !errors.As(err, &rv) {
		t.Fatalf("DiffInsideRemit() = %v, want *RemitViolationError", err)
	}
	if rv.Path != "internal/secret/leak.go" {
		t.Errorf("RemitViolationError.Path = %q, want %q", rv.Path, "internal/secret/leak.go")
	}
}

func TestDiffInsideRemit_ExcludesControlDir(t *testing.T) {
	t.Parallel()

	ws := provisionForTest(t)
	// The runner writes the ResultContract under .mandat/; it must not read as a
	// remit violation (RFC-0001 §Load-bearing contracts / AC-5.5).
	writeFile(t, filepath.Join(ws.Dir, ".mandat/result.json"), `{"schema_version":1,"task_id":"t","status":"failed","reason":"x"}`)

	if err := ws.DiffInsideRemit(context.Background()); err != nil {
		t.Fatalf("DiffInsideRemit() with only a .mandat/ write = %v, want nil (control dir excluded)", err)
	}
}

func TestProvision_SetupFailureHasNoFallback(t *testing.T) {
	t.Parallel()

	// The bare origin is unreachable, so the mirror clone fails (RFC-0001 AC-18
	// / AC-5.4, forced-failure variant).
	cfg := Config{
		RepoURL:   filepath.Join(t.TempDir(), "does-not-exist.git"),
		MirrorDir: filepath.Join(t.TempDir(), "mirror.git"),
		TasksRoot: t.TempDir(),
		TaskID:    "ado-baodo0220-99",
		Remit:     task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/"}},
	}

	ws, err := Provision(context.Background(), cfg)
	if ws != nil {
		t.Fatalf("Provision() returned a workspace on setup failure = %+v, want nil (no shared-checkout fallback)", ws)
	}
	var se *SetupError
	if !errors.As(err, &se) {
		t.Fatalf("Provision() error = %v, want *SetupError", err)
	}
}

func TestGitCredArgs(t *testing.T) {
	t.Parallel()

	args := []string{"fetch", "--prune"}

	got := gitCredArgs("", args...)
	if len(got) != len(args) {
		t.Fatalf("gitCredArgs(\"\", ...) = %v, want args unchanged %v", got, args)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("gitCredArgs(\"\", ...)[%d] = %q, want %q", i, got[i], args[i])
		}
	}

	helper := "!mandat git-credential --role dev"
	got = gitCredArgs(helper, args...)
	want := []string{"-c", "credential.helper=" + helper, "fetch", "--prune"}
	if len(got) != len(want) {
		t.Fatalf("gitCredArgs(%q, ...) = %v, want %v", helper, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("gitCredArgs(%q, ...)[%d] = %q, want %q", helper, i, got[i], want[i])
		}
	}
}

func TestProvision_CredentialHelperOverrideHarmlessAgainstLocalOrigin(t *testing.T) {
	t.Parallel()

	// The mirror clone/fetch gain a `-c credential.helper=...` override
	// (RFC-0001 §Identity injection); a local bare origin needs no auth, so the
	// override must be a no-op, not a breakage.
	cfg := Config{
		RepoURL:          newBareOrigin(t),
		MirrorDir:        filepath.Join(t.TempDir(), "mirror.git"),
		TasksRoot:        t.TempDir(),
		TaskID:           "ado-baodo0220-43",
		Remit:            task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}},
		CredentialHelper: "!mandat git-credential --role dev",
	}
	ws, err := Provision(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Provision() with CredentialHelper set against a local origin error = %v, want nil", err)
	}
	assertExists(t, filepath.Join(ws.Dir, "cmd/mandat/main.go"))
}

func TestProvision_MirrorForcedNonBare(t *testing.T) {
	t.Parallel()

	// A --mirror clone sets core.bare=true; on git < 2.35 (the customer VM's
	// 2.34.1) that leaks into linked worktrees and every work-tree op there
	// fails. ensureMirror must force it back to false so the fix holds
	// regardless of the git version running the test.
	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	cfg := Config{
		RepoURL:   newBareOrigin(t),
		MirrorDir: mirrorDir,
		TasksRoot: t.TempDir(),
		TaskID:    "ado-baodo0220-44",
		Remit:     task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}},
	}
	if _, err := Provision(context.Background(), cfg); err != nil {
		t.Fatalf("Provision() error = %v, want nil", err)
	}

	out, err := exec.Command("git", "-C", mirrorDir, "config", "--get", "core.bare").CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get core.bare: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "false" {
		t.Errorf("core.bare = %q, want %q", got, "false")
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=mandat-test",
		"GIT_AUTHOR_EMAIL=test@mandat.invalid",
		"GIT_COMMITTER_NAME=mandat-test",
		"GIT_COMMITTER_EMAIL=test@mandat.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func appendFile(t *testing.T, p, content string) {
	t.Helper()
	existing, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if err := os.WriteFile(p, append(existing, content...), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func assertExists(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("expected %s to be materialized, got stat error %v", p, err)
	}
}

func assertAbsent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected %s to be absent (outside remit), stat error = %v", p, err)
	}
}
