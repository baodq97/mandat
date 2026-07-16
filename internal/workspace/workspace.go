// Package workspace establishes and checks the mechanical remit boundary for a
// task's run (spec §4.5, RFC-0001 §Isolation). It provisions a per-task git
// worktree from a shared bare mirror with sparse checkout set to exactly the
// remit paths, so no file outside the remit is ever materialized, and it runs
// the post-hoc diff-inside-remit check that fails a run whose changes touch a
// path outside the remit. Isolation is all-or-nothing: any setup failure returns
// a *SetupError and there is no fallback to a shared or full checkout (spec §4.5,
// RFC-0001 AC-18). Every git operation shells out through os/exec (ADR-0002 rung
// 1: stdlib), the operations being a handful of plumbing commands below the bar
// where a git library earns a dependency.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/baodq97/mandat/internal/result"
	"github.com/baodq97/mandat/internal/task"
)

// DefaultTasksRoot is the production parent directory for per-task worktrees
// (spec §4.5, RFC-0001 §Isolation). Tests pass a temp dir instead.
const DefaultTasksRoot = "/var/lib/mandat/tasks"

// branchPrefix namespaces the per-task branch the worktree checks out, so the
// PR branch a run pushes is traceable to its task and never collides with a base
// branch that another worktree holds (a branch can be checked out in one
// worktree at a time).
const branchPrefix = "mandat/"

// controlDir is the worktree-relative directory the runner writes the
// ResultContract into; it is excluded from the diff-inside-remit comparison
// (RFC-0001 §Load-bearing contracts: "the .mandat/ control directory is excluded
// from the diff-inside-remit check"). It is derived from result.Path so the
// exclusion tracks the contract's own location rather than a duplicated literal.
var controlDir = path.Dir(result.Path) + "/"

// Config is the input to Provision: where the repo lives, the shared mirror and
// per-task locations, the task identity that names the worktree and its branch,
// and the remit whose paths bound the sparse checkout and the post-hoc diff.
type Config struct {
	// RepoURL is the origin: an Azure DevOps remote in production, a local bare
	// repo in tests (the §9 double). Provision mirrors it, never clones a full
	// working checkout of it.
	RepoURL string
	// MirrorDir is the shared bare mirror cache for RepoURL. Per-task worktrees
	// are linked worktrees of this mirror and borrow its object store.
	MirrorDir string
	// TasksRoot is the parent of per-task worktrees; DefaultTasksRoot in
	// production, a temp dir in tests.
	TasksRoot string
	// TaskID names the worktree directory and the per-task branch.
	TaskID string
	// Remit carries the base branch the worktree forks from and the allow-list
	// of path patterns the sparse checkout materializes (spec §4.5).
	Remit task.Remit
	// CredentialHelper, when set, is passed as `-c credential.helper=<value>` to
	// the mirror clone and fetch so those network ops authenticate to the origin
	// (the agent-user token via `mandat git-credential`); local-bare-origin tests
	// leave it empty and clone unauthenticated.
	CredentialHelper string
}

// Workspace is a provisioned per-task sandbox: the worktree directory, its
// branch, and the base commit the diff-inside-remit check compares against.
type Workspace struct {
	Dir    string
	Branch string

	baseRef string
	remit   task.Remit
}

// SetupError marks an isolation-setup failure — mirror, base branch, worktree,
// sparse checkout, or the OS-user spawn — that the orchestrator maps to the
// setup_failed event (RFC-0001 §Orchestrator: queued -> needs-human). Its
// existence encodes the invariant that there is NO fallback to a shared or full
// checkout: a setup failure fails the task, it never degrades to a wider
// checkout (spec §4.5, RFC-0001 AC-18).
type SetupError struct {
	// Op is the isolation step that failed: "mirror", "base-branch",
	// "worktree", "sparse-checkout", or "spawn".
	Op  string
	Err error
}

func (e *SetupError) Error() string {
	return fmt.Sprintf("workspace: isolation setup failed at %s: %v; no shared-checkout fallback", e.Op, e.Err)
}

func (e *SetupError) Unwrap() error { return e.Err }

// RemitViolationError names the first changed path that falls outside the remit,
// the post-hoc diff-inside-remit failure the orchestrator maps to the
// remit_violation event (RFC-0001 AC-17). It reports one path: the check is a
// gate, not a linter, so the first breach is enough to hold the task for a human.
type RemitViolationError struct {
	Path string
}

func (e *RemitViolationError) Error() string {
	return fmt.Sprintf("workspace: change to %q is outside the remit", e.Path)
}

// mirrorLocksMu guards mirrorLocks, the registry of per-mirror in-process
// locks. It is only ever held for the map lookup/insert in mirrorLock, never
// across a git call.
var mirrorLocksMu sync.Mutex

// mirrorLocks maps a mirror directory to the mutex that serializes every
// touch of that mirror: ensureMirror's clone-or-fetch and config heal, and the
// `git worktree add` that links a new worktree into it. `git config` takes an
// exclusive, non-retrying config.lock, so two concurrent Provision calls
// against the same warm mirror can otherwise collide on that lock and the
// loser surfaces as a spurious needs-human SetupError (US-0012 AC-12.2).
//
// This lock is in-process only, and that is the correct scope: RFC-0001 (the
// single-VM concurrent-dispatch amendment) runs exactly one serve daemon per
// VM, so there is never a second process touching this mirror to race
// against. Cross-process locking is explicitly out of scope.
//
// Different mirror directories get different mutexes, so concurrent
// Provision calls for different repos never block each other.
var mirrorLocks = make(map[string]*sync.Mutex)

// mirrorLock returns the mutex serializing touches of mirrorDir, creating one
// on first use.
func mirrorLock(mirrorDir string) *sync.Mutex {
	mirrorLocksMu.Lock()
	defer mirrorLocksMu.Unlock()
	mu, ok := mirrorLocks[mirrorDir]
	if !ok {
		mu = &sync.Mutex{}
		mirrorLocks[mirrorDir] = mu
	}
	return mu
}

// Provision creates the per-task worktree at <TasksRoot>/<TaskID>: a linked
// worktree of the mirror, on a fresh task branch off the remit's base branch,
// with sparse checkout set to exactly the remit paths so no file outside the
// remit is ever materialized — not even transiently (RFC-0001 AC-10). Any
// failure returns a *SetupError and no Workspace; there is no shared-checkout
// fallback (spec §4.5, RFC-0001 AC-18).
func Provision(ctx context.Context, cfg Config) (*Workspace, error) {
	baseRef, wtDir, branch, err := touchMirror(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// The mirror is bare (see ensureMirror); git < 2.35 leaks that core.bare=true
	// into this linked worktree and fails the work-tree ops below. The per-worktree
	// override — honored because ensureMirror enabled extensions.worktreeConfig —
	// flips core.bare=false for this worktree alone, leaving the mirror bare so its
	// refresh fetch never hits the non-bare refusal.
	if _, err := runGit(ctx, wtDir, "config", "--worktree", "core.bare", "false"); err != nil {
		return nil, &SetupError{Op: "worktree", Err: err}
	}

	// `sparse-checkout set` writes the pattern file and skip-worktree bits;
	// `read-tree -mu HEAD` then materializes exactly the in-pattern subset from
	// the empty --no-checkout working tree, honoring those skip-worktree bits so
	// out-of-pattern paths stay absent. --no-cone treats each remit path as a
	// gitignore-style pattern, not a cone prefix, because remit paths are
	// arbitrary patterns.
	setArgs := append([]string{"sparse-checkout", "set", "--no-cone"}, cfg.Remit.Paths...)
	if _, err := runGit(ctx, wtDir, setArgs...); err != nil {
		return nil, &SetupError{Op: "sparse-checkout", Err: err}
	}
	if _, err := runGit(ctx, wtDir, "read-tree", "-mu", "HEAD"); err != nil {
		return nil, &SetupError{Op: "sparse-checkout", Err: err}
	}

	return &Workspace{Dir: wtDir, Branch: branch, baseRef: baseRef, remit: cfg.Remit}, nil
}

// touchMirror runs the entire per-mirror touch — ensureMirror's clone-or-fetch
// and config heal, resolving the remit's base branch, and linking a new
// worktree into the mirror — under that mirror's in-process lock (see
// mirrorLocks), so a concurrent Provision call against the same mirror cannot
// interleave a `git config` with this one and collide on git's exclusive,
// non-retrying config.lock (US-0012 AC-12.2). The lock is released once the
// worktree is linked; the remaining setup in Provision only touches wtDir, not
// the shared mirror.
func touchMirror(ctx context.Context, cfg Config) (baseRef, wtDir, branch string, err error) {
	mu := mirrorLock(cfg.MirrorDir)
	mu.Lock()
	defer mu.Unlock()

	if err := ensureMirror(ctx, cfg.RepoURL, cfg.MirrorDir, cfg.CredentialHelper); err != nil {
		return "", "", "", err
	}

	ref, err := runGit(ctx, cfg.MirrorDir, "rev-parse", "--verify", cfg.Remit.BaseBranch)
	if err != nil {
		return "", "", "", &SetupError{Op: "base-branch", Err: err}
	}
	baseRef = strings.TrimSpace(ref)

	if err := os.MkdirAll(cfg.TasksRoot, 0o700); err != nil {
		return "", "", "", &SetupError{Op: "worktree", Err: err}
	}
	wtDir = filepath.Join(cfg.TasksRoot, cfg.TaskID)
	branch = branchPrefix + cfg.TaskID

	// --no-checkout defers materialization until the sparse patterns are set, so
	// the full tree is never written to disk (the remit must never be exceeded,
	// even for the moment between checkout and sparse configuration).
	if _, err := runGit(ctx, cfg.MirrorDir, "worktree", "add", "--no-checkout", "-b", branch, wtDir, baseRef); err != nil {
		return "", "", "", &SetupError{Op: "worktree", Err: err}
	}

	return baseRef, wtDir, branch, nil
}

// DiffInsideRemit computes the set of paths the run changed against the base
// commit and reports a *RemitViolationError naming the first that falls outside
// the remit, or nil when every change stays inside it (RFC-0001 AC-16, AC-17).
// The .mandat/ control directory is excluded from the comparison (AC-5.5).
func (w *Workspace) DiffInsideRemit(ctx context.Context) error {
	changed, err := w.changedPaths(ctx)
	if err != nil {
		return err
	}
	for _, p := range changed {
		if strings.HasPrefix(p, controlDir) {
			continue
		}
		if !pathInRemit(p, w.remit.Paths) {
			return &RemitViolationError{Path: p}
		}
	}
	return nil
}

// changedPaths is the union of tracked changes against the base commit and new
// untracked files, sorted for a deterministic first-violation report. Untracked
// files matter because sparse checkout hides tracked paths outside the remit, so
// the realistic escape is a NEW path the agent writes outside its remit, which
// `git diff` alone would not surface (RFC-0001 AC-17).
func (w *Workspace) changedPaths(ctx context.Context) ([]string, error) {
	tracked, err := runGit(ctx, w.Dir, "diff", "--name-only", w.baseRef)
	if err != nil {
		return nil, err
	}
	untracked, err := runGit(ctx, w.Dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	for _, p := range splitLines(tracked) {
		seen[p] = struct{}{}
	}
	for _, p := range splitLines(untracked) {
		seen[p] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// pathInRemit reports whether changed is covered by any remit pattern. A pattern
// is matched as a directory prefix (with or without a trailing slash) or as a
// gitignore-style glob, mirroring how the sparse checkout materialized the tree.
func pathInRemit(changed string, patterns []string) bool {
	for _, raw := range patterns {
		p := strings.TrimSuffix(raw, "/")
		if changed == p || strings.HasPrefix(changed, p+"/") {
			return true
		}
		if ok, _ := path.Match(raw, changed); ok {
			return true
		}
	}
	return false
}

// ensureMirror makes sure a bare mirror of repoURL exists at mirrorDir, cloning
// it on first use and fetching to refresh it otherwise. The mirror is the shared
// object store per-task worktrees borrow from: a linked worktree of a bare
// mirror shares its object store natively, which is the object sharing RFC-0001's
// "git worktree add --reference against the mirror" phrasing intends. Callers
// must hold mirrorDir's lock (see mirrorLock) for the duration of this call;
// ensureMirror does no locking of its own. credHelper is passed through
// gitCredArgs to the clone and the fetch, the two network ops that need to
// authenticate to the origin.
func ensureMirror(ctx context.Context, repoURL, mirrorDir, credHelper string) error {
	existed := true
	if _, err := os.Stat(mirrorDir); err != nil {
		existed = false
		if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o700); err != nil {
			return &SetupError{Op: "mirror", Err: err}
		}
		if _, err := runGit(ctx, "", gitCredArgs(credHelper, "clone", "--mirror", repoURL, mirrorDir)...); err != nil {
			return &SetupError{Op: "mirror", Err: err}
		}
	}

	// Invariant pair. The mirror MUST stay bare: with its +refs/*:refs/* mirror
	// refspec, `git fetch` into a non-bare repo refuses to update the branch HEAD
	// points at ("Refusing to fetch into current branch ... of non-bare
	// repository"), so every refresh after the first fails. Yet git < 2.35 leaks
	// the shared core.bare into linked worktrees, where core.bare=true fails every
	// work-tree op ("must be run in a work tree"). extensions.worktreeConfig
	// reconciles them: the mirror stays bare here while each worktree overrides
	// core.bare=false in its own per-worktree config at provision time. The heal
	// runs on every ensureMirror — not just after clone — because v0.1.1 shipped
	// core.bare=false on the mirror and the refresh below fetches into those
	// already-poisoned mirrors, so it must precede the fetch to unbreak it.
	if _, err := runGit(ctx, mirrorDir, "config", "core.bare", "true"); err != nil {
		return &SetupError{Op: "mirror", Err: err}
	}
	if _, err := runGit(ctx, mirrorDir, "config", "extensions.worktreeConfig", "true"); err != nil {
		return &SetupError{Op: "mirror", Err: err}
	}

	// Refresh WITHOUT --prune. The mirror's refspec is +refs/*:refs/* (git clone
	// --mirror), so a pruning fetch deletes every local ref the origin lacks — which
	// includes the per-task worktree branches refs/heads/mandat/<taskID> that
	// Provision creates and no origin ever has. When a second task provisions against
	// this warm mirror, its prune deletes a live sibling's branch: if it lands before
	// that sibling's `read-tree HEAD`, the sibling fails setup (needs-human); if it
	// lands after, the sibling runs on an unborn HEAD and the agent's commit becomes a
	// parentless root commit carrying the whole tree (both observed live at pool>1).
	// A non-pruning fetch force-updates the shared origin branches and never touches
	// the task branches; stale deleted-origin branches in the cache are not worth that.
	if existed {
		if _, err := runGit(ctx, mirrorDir, gitCredArgs(credHelper, "fetch")...); err != nil {
			return &SetupError{Op: "mirror", Err: err}
		}
	}
	return nil
}

// gitCredArgs prepends a per-invocation credential.helper override to a git
// argument list, or returns args unchanged when no helper is configured. The
// override is one argv element, so a helper value with spaces (e.g. a `!cmd
// --flag` shell helper) reaches git intact and is never shell-split here.
func gitCredArgs(helper string, args ...string) []string {
	if helper == "" {
		return args
	}
	return append([]string{"-c", "credential.helper=" + helper}, args...)
}

// runGit runs a git subprocess in dir (or the current directory when dir is "")
// and returns its stdout, wrapping a non-zero exit with the command and stderr.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// gitEnv strips the ambient global and system git config from every mandat git
// invocation and forbids interactive prompts, so a per-task worktree depends only
// on the repo and remit and never inherits the operator's settings or blocks on a
// credential prompt.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
