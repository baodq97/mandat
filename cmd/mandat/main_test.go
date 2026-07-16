package main

import (
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version", args: []string{"version"}, wantCode: 0, wantStdout: "mandat "},
		{name: "no args", args: nil, wantCode: 2, wantStderr: "design-gated"},
		{name: "unknown command", args: []string{"dispatch"}, wantCode: 2, wantStderr: "design-gated"},
		// The new subcommands route to their handlers, not the design-gated path: a
		// missing config file fails fast with a config error before any daemon loop or
		// stdin read, which proves dispatch reached each handler.
		{name: "serve routes", args: []string{"serve", "--config", "/nonexistent/mandat.yaml"}, wantCode: 1, wantStderr: "config"},
		{name: "doctor routes", args: []string{"doctor", "--config", "/nonexistent/mandat.yaml"}, wantCode: 1, wantStderr: "config"},
		{name: "git-credential routes", args: []string{"git-credential", "--config", "/nonexistent/mandat.yaml", "get"}, wantCode: 1, wantStderr: "config"},
		// remit-guard reads no config, so unlike the other subcommands its
		// deterministic dispatch proof is a flag error rather than a config-load
		// error; this also avoids the test depending on the test process's stdin.
		{name: "remit-guard routes", args: []string{"remit-guard", "--bogus-flag"}, wantCode: 2, wantStderr: "flag provided but not defined"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("run(%v) = %d, want %d", tt.args, code, tt.wantCode)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
