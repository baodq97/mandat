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

// Provision creates the per-task worktree at <TasksRoot>/<TaskID>: a linked
// worktree of the mirror, on a fresh task branch off the remit's base branch,
// with sparse checkout set to exactly the remit paths so no file outside the
// remit is ever materialized — not even transiently (RFC-0001 AC-10). Any
// failure returns a *SetupError and no Workspace; there is no shared-checkout
// fallback (spec §4.5, RFC-0001 AC-18).
func Provision(ctx context.Context, cfg Config) (*Workspace, error) {
	if err := ensureMirror(ctx, cfg.RepoURL, cfg.MirrorDir); err != nil {
		return nil, err
	}

	baseRef, err := runGit(ctx, cfg.MirrorDir, "rev-parse", "--verify", cfg.Remit.BaseBranch)
	if err != nil {
		return nil, &SetupError{Op: "base-branch", Err: err}
	}
	baseRef = strings.TrimSpace(baseRef)

	if err := os.MkdirAll(cfg.TasksRoot, 0o700); err != nil {
		return nil, &SetupError{Op: "worktree", Err: err}
	}
	wtDir := filepath.Join(cfg.TasksRoot, cfg.TaskID)
	branch := branchPrefix + cfg.TaskID

	// --no-checkout defers materialization until the sparse patterns are set, so
	// the full tree is never written to disk (the remit must never be exceeded,
	// even for the moment between checkout and sparse configuration).
	if _, err := runGit(ctx, cfg.MirrorDir, "worktree", "add", "--no-checkout", "-b", branch, wtDir, baseRef); err != nil {
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
// "git worktree add --reference against the mirror" phrasing intends. The
// skeleton runs one in-flight task (RFC-0001 scope), so the refresh needs no
// cross-task lock yet.
func ensureMirror(ctx context.Context, repoURL, mirrorDir string) error {
	if _, err := os.Stat(mirrorDir); err == nil {
		if _, err := runGit(ctx, mirrorDir, "fetch", "--prune"); err != nil {
			return &SetupError{Op: "mirror", Err: err}
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o700); err != nil {
		return &SetupError{Op: "mirror", Err: err}
	}
	if _, err := runGit(ctx, "", "clone", "--mirror", repoURL, mirrorDir); err != nil {
		return &SetupError{Op: "mirror", Err: err}
	}
	return nil
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
