package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/baodq97/mandat/internal/config"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    []int
		wantErr bool
	}{
		{"claude line", "2.1.210 (Claude Code)\n", []int{2, 1, 210}, false},
		{"git line", "git version 2.43.0\n", []int{2, 43, 0}, false},
		{"two component", "v1.9 something", []int{1, 9}, false},
		{"no version", "unknown build", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseVersion(%q) error = nil, want an error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVersion(%q) error = %v", tc.in, err)
			}
			if !slices.Equal(got.parts, tc.want) {
				t.Errorf("parseVersion(%q) parts = %v, want %v", tc.in, got.parts, tc.want)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got   string
		floor string
		want  bool
	}{
		{"2.1.210", "2.1.208", true},  // above the floor (AC-9.4 pass side)
		{"2.1.208", "2.1.208", true},  // exactly the floor
		{"2.1.207", "2.1.208", false}, // below the floor (AC-9.4 fail side)
		{"2.0.999", "2.1.208", false}, // lower minor dominates a higher patch
		{"2.2.0", "2.1.208", true},    // higher minor
		{"3.0.0", "2.1.208", true},    // higher major
	}
	for _, tc := range cases {
		t.Run(tc.got+"_vs_"+tc.floor, func(t *testing.T) {
			t.Parallel()
			got, err := parseVersion(tc.got)
			if err != nil {
				t.Fatalf("parseVersion(%q) error = %v", tc.got, err)
			}
			if got := versionAtLeast(got, tc.floor); got != tc.want {
				t.Errorf("versionAtLeast(%s, %s) = %v, want %v", tc.got, tc.floor, got, tc.want)
			}
		})
	}
}

// TestRunChecks_ExitCodeAndTable proves the exit contract: a required failing check
// blocks (exit 1) while a non-required failing check only warns (exit 0), and every
// check renders a row with its status.
func TestRunChecks_ExitCodeAndTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		checks   []checkResult
		wantExit int
		wantRows []string
	}{
		{
			name: "all required pass",
			checks: []checkResult{
				{name: "claude CLI", required: true, ok: true, detail: "claude 2.1.210"},
				{name: "git", required: true, ok: true, detail: "git 2.43.0"},
			},
			wantExit: 0,
			wantRows: []string{"claude CLI", "PASS", "git", "PASS"},
		},
		{
			name: "required failure blocks",
			checks: []checkResult{
				{name: "claude CLI", required: true, ok: false, detail: "not found"},
				{name: "git", required: true, ok: true, detail: "git 2.43.0"},
			},
			wantExit: 1,
			wantRows: []string{"claude CLI", "FAIL", "git", "PASS"},
		},
		{
			name: "non-required failure only warns",
			checks: []checkResult{
				{name: "disk headroom", required: false, ok: false, detail: "tight"},
			},
			wantExit: 0,
			wantRows: []string{"disk headroom", "WARN"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fns := make([]func(context.Context) checkResult, len(tc.checks))
			for i := range tc.checks {
				r := tc.checks[i]
				fns[i] = func(context.Context) checkResult { return r }
			}
			var out strings.Builder
			exit := runChecks(context.Background(), fns, &out)
			if exit != tc.wantExit {
				t.Errorf("runChecks() exit = %d, want %d", exit, tc.wantExit)
			}
			for _, row := range tc.wantRows {
				if !strings.Contains(out.String(), row) {
					t.Errorf("table = %q, want it to contain %q", out.String(), row)
				}
			}
		})
	}
}

// TestReviewerIdentityCheck proves the three outcomes: PASS when a reviewer role
// has a UPN distinct from every other role, WARN (not FAIL) when no reviewer role
// is configured, and FAIL when the reviewer's UPN collides with another role's
// (writer != scorer, RFC-0001 AC-27).
func TestReviewerIdentityCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		roles      map[string]config.RoleConfig
		wantOK     bool
		wantReq    bool
		wantDetail string
	}{
		{
			name: "pass: reviewer distinct from dev",
			roles: map[string]config.RoleConfig{
				"dev":      {AgentUserName: "dev-agent@baodo0220.onmicrosoft.com"},
				"reviewer": {AgentUserName: "reviewer-agent@baodo0220.onmicrosoft.com"},
			},
			wantOK:  true,
			wantReq: true,
		},
		{
			name: "warn: no reviewer role configured",
			roles: map[string]config.RoleConfig{
				"dev": {AgentUserName: "dev-agent@baodo0220.onmicrosoft.com"},
			},
			wantOK:     false,
			wantReq:    false,
			wantDetail: "verification will hold every task at the probe",
		},
		{
			name: "fail: reviewer UPN equals dev UPN",
			roles: map[string]config.RoleConfig{
				"dev":      {AgentUserName: "shared-agent@baodo0220.onmicrosoft.com"},
				"reviewer": {AgentUserName: "shared-agent@baodo0220.onmicrosoft.com"},
			},
			wantOK:     false,
			wantReq:    true,
			wantDetail: "reviewer and dev share agent_user_name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Roles: tc.roles}
			r := reviewerIdentityCheck(cfg)
			if r.ok != tc.wantOK {
				t.Errorf("reviewerIdentityCheck() ok = %v, want %v (detail: %s)", r.ok, tc.wantOK, r.detail)
			}
			if r.required != tc.wantReq {
				t.Errorf("reviewerIdentityCheck() required = %v, want %v", r.required, tc.wantReq)
			}
			if tc.wantDetail != "" && !strings.Contains(r.detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", r.detail, tc.wantDetail)
			}
		})
	}
}

// TestSQLiteCheck_OpensTempPath proves the sqlite preflight passes against a fresh
// path (Open runs the idempotent migration) and closes it cleanly.
func TestSQLiteCheck_OpensTempPath(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "mandat.db")
	r := sqliteCheck(context.Background(), dbPath)
	if !r.ok {
		t.Errorf("sqliteCheck(%q) = %+v, want ok", dbPath, r)
	}
	if !strings.Contains(r.detail, dbPath) {
		t.Errorf("detail = %q, want it to name the db path", r.detail)
	}
}
