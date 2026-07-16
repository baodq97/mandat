package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// remitGuardCmd is the PreToolUse hook entry point serve.go wires into every
// runner invocation as `mandat remit-guard --worktree "$CLAUDE_PROJECT_DIR"`
// (DenyToolHookCommand, ADR-0006 §Isolation). It only resolves the --worktree
// flag; remitGuard owns the allow/deny decision and stays stdin-injectable for
// tests, mirroring the git-credential split between flag plumbing and protocol
// core.
func remitGuardCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remit-guard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	worktree := fs.String("worktree", "", "the task worktree a Write/Edit target must resolve inside")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return remitGuard(stdin, *worktree, stdout)
}

// hookInput is the subset of the Claude Code PreToolUse payload (JSON on stdin)
// this guard reads. The full payload also carries tool_name, cwd, and a session
// id; the guard does not need tool_name because the settings.json matcher
// (Write|Edit, ADR-0006) already scopes which tool calls invoke this hook at
// all — the guard only needs the path the tool is about to write.
type hookInput struct {
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// hookDenyOutput is the PreToolUse decision shape the Claude Code hooks contract
// reads from stdout on exit 0 to block a tool call. The shape is Claude Code's,
// not mandat's, so it is pinned here rather than derived (ADR-0003) — it mirrors
// this repo's own .claude/settings.json audit-write hook, which denies the same
// way.
type hookDenyOutput struct {
	HookSpecificOutput hookDenyDecision `json:"hookSpecificOutput"`
}

type hookDenyDecision struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason"`
}

// remitGuard is the PreToolUse deny-hook core (ADR-0006 §Isolation; RFC-0001
// §4.5-4.6, "mechanical layers enforce remit, never prompts"). It is
// deliberately the coarse layer: allow a Write/Edit whose target resolves inside
// the task worktree, deny one that resolves outside it — an absolute path
// elsewhere, or a relative path that traverses out via "..". Per-remit
// path-pattern enforcement (matching the Remit's own allow/deny globs, not just
// "inside the worktree") is a follow-up story; the sparse-checkout worktree (the
// agent cannot see paths outside its remit) and the supervisor's post-hoc
// diff-inside-remit check remain the authoritative enforcement (RFC-0001
// §4.6) — this hook only stops an escape attempt before the write lands, as
// defense-in-depth.
//
// Fails open on everything short of that one escape case: empty/malformed
// stdin, a tool call with no file_path (every non-Write/Edit tool, and any
// Write/Edit payload shape this guard fails to parse), and a missing or
// unresolvable --worktree. A hook that blocked or crashed on a shape it did not
// expect would deny a write for a reason unrelated to the remit, which is worse
// than deferring to the mechanical layers below it — the deny path exists only
// to catch the one case those layers should not have to rely on this hook for.
func remitGuard(stdin io.Reader, worktree string, stdout io.Writer) int {
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return 0
	}
	if in.ToolInput.FilePath == "" || worktree == "" {
		return 0
	}

	absWorktree, err := filepath.Abs(worktree)
	if err != nil {
		return 0
	}
	target := in.ToolInput.FilePath
	if !filepath.IsAbs(target) {
		target = filepath.Join(absWorktree, target)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return 0
	}

	rel, err := filepath.Rel(absWorktree, absTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return denyEscapedWorktree(stdout, in.ToolInput.FilePath, absWorktree)
	}
	return 0
}

// denyEscapedWorktree writes the PreToolUse deny decision and returns the hook's
// own exit code. The exit code stays 0: the Claude Code hooks contract reads the
// deny from the JSON body on stdout, not from a non-zero exit (ADR-0006;
// exit 2 + stderr is the contract's other deny form, but remitGuard has no
// stderr seam of its own and the JSON form is sufficient to block the call).
func denyEscapedWorktree(stdout io.Writer, path, worktree string) int {
	out := hookDenyOutput{
		HookSpecificOutput: hookDenyDecision{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: fmt.Sprintf("remit-guard: %q resolves outside the task worktree %q", path, worktree),
		},
	}
	// Encoding this static, always-valid shape cannot fail in a way this hook
	// could act on differently; nothing further to report from this seam.
	_ = json.NewEncoder(stdout).Encode(out)
	return 0
}
