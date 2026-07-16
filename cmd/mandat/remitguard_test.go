package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemitGuard proves the worktree-boundary decision (ADR-0006 §Isolation): a
// Write/Edit target inside the worktree allows, one that escapes it (an absolute
// path elsewhere, or a "../" traversal) denies, and anything that is not that one
// escape case fails open. The hook contract always exits 0 (ADR-0006: the deny is
// carried in the JSON body, not the exit code), so every case asserts on the exit
// code plus whether stdout carries a deny decision.
func TestRemitGuard(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()

	tests := []struct {
		name     string
		stdin    string
		wantDeny bool
	}{
		{
			name:  "write inside worktree allows",
			stdin: hookFixture(t, filepath.Join(worktree, "src", "main.go")),
		},
		{
			name:     "write to absolute path outside worktree denies",
			stdin:    hookFixture(t, "/etc/passwd"),
			wantDeny: true,
		},
		{
			name:     "relative path traversal out of worktree denies",
			stdin:    hookFixture(t, "../../etc/x"),
			wantDeny: true,
		},
		{
			name:  "non-file tool allows",
			stdin: `{"tool_name":"Bash","tool_input":{"command":"ls"}}`,
		},
		{
			name:  "empty stdin allows",
			stdin: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			code := remitGuard(strings.NewReader(tt.stdin), worktree, &out)
			if code != 0 {
				t.Fatalf("remitGuard(%q) = %d, want 0 (the hook always exits 0; the decision is the stdout body)", tt.stdin, code)
			}
			denied := strings.Contains(out.String(), `"permissionDecision":"deny"`)
			if denied != tt.wantDeny {
				t.Errorf("remitGuard(%q) stdout = %q, want deny=%v", tt.stdin, out.String(), tt.wantDeny)
			}
		})
	}
}

// TestRemitGuard_EmptyWorktreeAllows proves a hook invoked with no --worktree
// resolved (CLAUDE_PROJECT_DIR unset) fails open rather than denying against a
// boundary it cannot compute, matching remitGuard's documented fail-open cases.
func TestRemitGuard_EmptyWorktreeAllows(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	code := remitGuard(strings.NewReader(hookFixture(t, "/anywhere/file.go")), "", &out)
	if code != 0 || out.Len() != 0 {
		t.Errorf("remitGuard() with empty worktree = %d, out = %q, want 0 and no deny output", code, out.String())
	}
}

// TestRemitGuardCmd_UnknownFlagIsDeterministicError proves remit-guard is wired
// into run()'s dispatch via a flag.Parse failure — a deterministic path that,
// unlike the happy path, needs no stdin read, so the dispatch test in
// main_test.go can assert on it without depending on the test process's stdin.
func TestRemitGuardCmd_UnknownFlagIsDeterministicError(t *testing.T) {
	t.Parallel()
	var out, errBuf strings.Builder
	code := remitGuardCmd([]string{"--bogus-flag"}, strings.NewReader(""), &out, &errBuf)
	if code != 2 {
		t.Errorf("remitGuardCmd(--bogus-flag) = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "flag provided but not defined") {
		t.Errorf("stderr = %q, want the flag package's parse-error message", errBuf.String())
	}
}

func hookFixture(t *testing.T, filePath string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"tool_name":  "Write",
		"tool_input": map[string]string{"file_path": filePath},
	})
	if err != nil {
		t.Fatalf("marshal hook fixture: %v", err)
	}
	return string(b)
}
