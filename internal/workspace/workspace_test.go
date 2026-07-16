package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func TestSharesMergeBase_FreshWorktreePasses(t *testing.T) {
	t.Parallel()

	ws := provisionForTest(t)
	// A freshly provisioned worktree's HEAD is the base ref itself, the ordinary
	// case a concurrent-provision defect must not falsely flag.
	if err := ws.SharesMergeBase(context.Background()); err != nil {
		t.Fatalf("SharesMergeBase() for a freshly provisioned worktree = %v, want nil", err)
	}
}

func TestSharesMergeBase_OrphanHeadFailsTyped(t *testing.T) {
	t.Parallel()

	ws := provisionForTest(t)
	// A concurrent-provision defect can leave the worktree on a parentless root
	// commit carrying the whole tree; --orphan + commit builds that here directly.
	git(t, ws.Dir, "checkout", "--orphan", "orphan-root")
	git(t, ws.Dir, "add", "-A")
	git(t, ws.Dir, "commit", "-m", "orphan root commit")

	err := ws.SharesMergeBase(context.Background())
	var ae *AncestryViolationError
	if !errors.As(err, &ae) {
		t.Fatalf("SharesMergeBase() = %v, want *AncestryViolationError", err)
	}
	if ae.Base != "main" {
		t.Errorf("AncestryViolationError.Base = %q, want %q", ae.Base, "main")
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

func TestProvision_ConcurrentSameRepoWarmMirrorNoRace(t *testing.T) {
	t.Parallel()

	// Warm the mirror with a first provision, then fire two more concurrent
	// Provision calls against that same warm mirror — the case where the
	// idempotent config heal in ensureMirror runs on every call and, unlocked,
	// could race a concurrent clone/fetch/worktree-add and collide on git's
	// exclusive config.lock (US-0012 AC-12.2).
	origin := newBareOrigin(t)
	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	tasksRoot := t.TempDir()
	remit := task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}}

	warm := Config{RepoURL: origin, MirrorDir: mirrorDir, TasksRoot: tasksRoot, TaskID: "ado-baodo0220-warm", Remit: remit}
	if _, err := Provision(context.Background(), warm); err != nil {
		t.Fatalf("warm-up Provision() error = %v, want nil", err)
	}

	taskIDs := []string{"ado-baodo0220-race-a", "ado-baodo0220-race-b"}
	var wg sync.WaitGroup
	results := make([]*Workspace, len(taskIDs))
	errs := make([]error, len(taskIDs))
	for i, id := range taskIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			cfg := Config{RepoURL: origin, MirrorDir: mirrorDir, TasksRoot: tasksRoot, TaskID: id, Remit: remit}
			results[i], errs[i] = Provision(context.Background(), cfg)
		}(i, id)
	}
	wg.Wait()

	for i, id := range taskIDs {
		if errs[i] != nil {
			var se *SetupError
			if errors.As(errs[i], &se) {
				t.Fatalf("concurrent Provision(%s) = %v, want nil (no spurious SetupError against a warm mirror)", id, se)
			}
			t.Fatalf("concurrent Provision(%s) error = %v, want nil", id, errs[i])
		}
		assertExists(t, filepath.Join(results[i].Dir, "cmd/mandat/main.go"))
		assertExists(t, filepath.Join(results[i].Dir, "internal/buildinfo/build.go"))
	}
}

// TestProvision_RefreshKeepsPriorTaskBranches proves a second Provision against a
// warm mirror does not delete the branch an earlier task's worktree is still
// checked out on. The mirror is a `git clone --mirror` (+refs/*:refs/*), so a
// pruning refresh fetch deletes every ref the origin lacks — including
// refs/heads/mandat/<id>, the per-task worktree branches no origin ever has.
// Losing a live worktree's branch mid-run either fails its `read-tree HEAD`
// (setup_failed) or leaves it on an unborn HEAD so the agent's commit becomes a
// parentless root commit carrying the whole tree. The check is sequential because
// the deletion is deterministic on the second Provision's fetch, not a race:
// concurrency only decides which of the two symptoms surfaces (US-0012 AC-12.2).
func TestProvision_RefreshKeepsPriorTaskBranches(t *testing.T) {
	t.Parallel()

	origin := newBareOrigin(t)
	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	tasksRoot := t.TempDir()
	remit := task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}}

	first, err := Provision(context.Background(), Config{RepoURL: origin, MirrorDir: mirrorDir, TasksRoot: tasksRoot, TaskID: "ado-baodo0220-first", Remit: remit})
	if err != nil {
		t.Fatalf("first Provision() error = %v, want nil", err)
	}

	// A second task provisions against the now-warm mirror; its refresh fetch runs
	// and must not prune the first task's branch out from under its open worktree.
	if _, err := Provision(context.Background(), Config{RepoURL: origin, MirrorDir: mirrorDir, TasksRoot: tasksRoot, TaskID: "ado-baodo0220-second", Remit: remit}); err != nil {
		t.Fatalf("second Provision() error = %v, want nil", err)
	}

	// The first worktree must still resolve HEAD: a pruned branch leaves it unborn,
	// which is exactly what turns the agent's next commit into an orphan root commit.
	out, err := runGit(context.Background(), first.Dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		t.Fatalf("first worktree HEAD no longer resolves after a sibling Provision: %v; its branch %q was pruned by the refresh fetch", err, first.Branch)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("first worktree HEAD resolved to empty after a sibling Provision")
	}
}

func TestProvision_ConcurrentDifferentReposDoNotShareALock(t *testing.T) {
	t.Parallel()

	// Two distinct repos (distinct mirror directories) provisioned at the same
	// time must not serialize each other: each gets its own per-mirror lock, so
	// both complete on their own, independent of the other.
	remit := task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}}
	tasksRoot := t.TempDir()
	configs := []Config{
		{RepoURL: newBareOrigin(t), MirrorDir: filepath.Join(t.TempDir(), "mirror-a.git"), TasksRoot: tasksRoot, TaskID: "ado-baodo0220-repo-a", Remit: remit},
		{RepoURL: newBareOrigin(t), MirrorDir: filepath.Join(t.TempDir(), "mirror-b.git"), TasksRoot: tasksRoot, TaskID: "ado-baodo0220-repo-b", Remit: remit},
	}

	var wg sync.WaitGroup
	results := make([]*Workspace, len(configs))
	errs := make([]error, len(configs))
	for i, cfg := range configs {
		wg.Add(1)
		go func(i int, cfg Config) {
			defer wg.Done()
			results[i], errs[i] = Provision(context.Background(), cfg)
		}(i, cfg)
	}
	wg.Wait()

	for i := range configs {
		if errs[i] != nil {
			t.Fatalf("concurrent Provision() for repo %d error = %v, want nil", i, errs[i])
		}
		assertExists(t, filepath.Join(results[i].Dir, "cmd/mandat/main.go"))
	}
}

// TestMirrorLock_PerDirRegistry is the cheap regression net the concurrency
// tests above cannot provide: a global-mutex regression would still let two
// concurrent Provision calls against different mirrors pass, since neither
// case actually races two mirrors against each other. This asserts the
// registry property directly: the same dir always gets the same *sync.Mutex,
// and different dirs never share one.
func TestMirrorLock_PerDirRegistry(t *testing.T) {
	t.Parallel()

	a1 := mirrorLock("dirA")
	a2 := mirrorLock("dirA")
	if a1 != a2 {
		t.Errorf("mirrorLock(%q) = %p then %p, want the same *sync.Mutex on repeat calls", "dirA", a1, a2)
	}

	b := mirrorLock("dirB")
	if a1 == b {
		t.Errorf("mirrorLock(%q) and mirrorLock(%q) returned the same *sync.Mutex, want distinct per-dir locks", "dirA", "dirB")
	}
}

func TestGitCredArgs(t *testing.T) {
	t.Parallel()

	args := []string{"fetch"}

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
	want := []string{"-c", "credential.helper=" + helper, "fetch"}
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

func TestProvision_MirrorStaysBareWorktreeOverrides(t *testing.T) {
	t.Parallel()

	// The invariant pair (ensureMirror): the mirror MUST stay bare so its
	// +refs/*:refs/* fetch never hits the non-bare refusal, while the linked
	// worktree MUST see core.bare=false so work-tree ops run on git < 2.35 (the
	// customer VM's 2.34.1), which leaks the shared core.bare into worktrees.
	// extensions.worktreeConfig + a per-worktree override reconciles the two.
	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	cfg := Config{
		RepoURL:   newBareOrigin(t),
		MirrorDir: mirrorDir,
		TasksRoot: t.TempDir(),
		TaskID:    "ado-baodo0220-44",
		Remit:     task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}},
	}
	ws, err := Provision(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Provision() error = %v, want nil", err)
	}

	if got := gitConfig(t, mirrorDir, "core.bare"); got != "true" {
		t.Errorf("mirror core.bare = %q, want %q", got, "true")
	}
	// The per-worktree override wins inside the worktree.
	if got := gitConfig(t, ws.Dir, "core.bare"); got != "false" {
		t.Errorf("worktree core.bare = %q, want %q", got, "false")
	}
	// A work-tree op runs in the worktree despite the bare mirror.
	if out, err := exec.Command("git", "-C", ws.Dir, "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("git -C %s status: %v\n%s", ws.Dir, err, out)
	}
}

func TestProvision_HealsV011PoisonedMirror(t *testing.T) {
	t.Parallel()

	// Regression for finding #3: v0.1.1 set core.bare=false on the mirror to dodge
	// the git < 2.35 worktree leak, but a non-bare mirror refuses the next
	// +refs/*:refs/* fetch that updates the branch HEAD points at, breaking every
	// provision after the first. ensureMirror must heal the mirror back to bare on
	// the fetch path — before the fetch — so a second provision succeeds.
	origin := newBareOrigin(t)
	mirrorDir := filepath.Join(t.TempDir(), "mirror.git")
	base := Config{
		RepoURL:   origin,
		MirrorDir: mirrorDir,
		TasksRoot: t.TempDir(),
		Remit:     task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}},
	}

	first := base
	first.TaskID = "ado-baodo0220-45"
	if _, err := Provision(context.Background(), first); err != nil {
		t.Fatalf("first Provision() error = %v, want nil", err)
	}

	// Reproduce the v0.1.1 poisoning: core.bare=false on the mirror, plus a new
	// commit on the default branch so the next fetch must update the branch the
	// mirror's HEAD points at — the exact update a non-bare mirror refuses.
	git(t, mirrorDir, "config", "core.bare", "false")
	pushNewCommitToDefaultBranch(t, origin)

	second := base
	second.TaskID = "ado-baodo0220-46"
	ws, err := Provision(context.Background(), second)
	if err != nil {
		t.Fatalf("second Provision() against a v0.1.1-poisoned mirror = %v, want nil (heal expected)", err)
	}

	if got := gitConfig(t, mirrorDir, "core.bare"); got != "true" {
		t.Errorf("healed mirror core.bare = %q, want %q", got, "true")
	}
	// The full provision completed: the remit tree materialized and a work-tree op
	// runs in the new worktree.
	assertExists(t, filepath.Join(ws.Dir, "cmd/mandat/main.go"))
	if out, err := exec.Command("git", "-C", ws.Dir, "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("git -C %s status: %v\n%s", ws.Dir, err, out)
	}
}

// pushNewCommitToDefaultBranch clones origin into a scratch checkout, commits a
// change on main, and pushes it back, advancing the branch the mirror's HEAD
// tracks so the next mirror fetch must update that ref.
func pushNewCommitToDefaultBranch(t *testing.T, origin string) {
	t.Helper()
	work := t.TempDir()
	git(t, "", "clone", origin, work)
	appendFile(t, filepath.Join(work, "cmd/mandat/main.go"), "\n// second commit\n")
	git(t, work, "add", "-A")
	git(t, work, "commit", "-m", "second commit")
	git(t, work, "push", "origin", "HEAD:main")
}

// gitConfig reads a single git config value from dir, failing the test on error.
func gitConfig(t *testing.T, dir, key string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "config", "--get", key).CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s config --get %s: %v\n%s", dir, key, err, out)
	}
	return strings.TrimSpace(string(out))
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
