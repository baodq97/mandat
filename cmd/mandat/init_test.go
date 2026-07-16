package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baodq97/mandat/internal/config"
)

// validInitArgs returns a full, valid --non-interactive flag set: every
// irreducible field US-0013 AC-13.3(c)/AC-13.9 name, plus --config pointed
// at path. Tests that exercise one missing/invalid flag start from this and
// mutate it, so each case isolates one violation (mirrors config_test.go's
// baseYAML pattern).
func validInitArgs(configPath string) []string {
	return []string{
		"--non-interactive",
		"--config", configPath,
		"--tracker-org", "baodo0220",
		"--tracker-project", "mandat-dogfood",
		"--auth-mode", "arc-managed-identity",
		"--entra-tenant", "d1a7b725-aaaa-bbbb-cccc-dddddddddddd",
		"--entra-blueprint", "blueprint-01",
		"--repo", "mandat=https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat",
		"--base-branch", "main",
		"--remit-path", "internal/",
		"--remit-path", "cmd/",
		"--gate", "make check",
		"--gate", "npx govkit check",
		"--dev-identity-id", "agent-identity-dev-01",
		"--dev-user-id", "agent-user-dev-01",
		"--dev-user-upn", "dev-agent@baodo0220.onmicrosoft.com",
		"--reviewer-identity-id", "agent-identity-reviewer-01",
		"--reviewer-user-id", "agent-user-reviewer-01",
		"--reviewer-user-upn", "reviewer-agent@baodo0220.onmicrosoft.com",
		"--autonomy-ceiling", "draft-pr",
		"--max-usd-per-run", "5",
	}
}

// removeFlag drops a flag and its value from args, so a test can start from
// validInitArgs and omit exactly one required flag.
func removeFlag(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			i++ // also skip its value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestInitCmd_NonInteractive_HappyPathEmitAndReload(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr strings.Builder
	code := initCmd(validInitArgs(configPath), strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), configPath) {
		t.Errorf("stdout = %q, want it to name %q", stdout.String(), configPath)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}

	if cfg.Tracker.Kind != config.TrackerAzureDevOps {
		t.Errorf("Tracker.Kind = %q, want %q (constant, no flag)", cfg.Tracker.Kind, config.TrackerAzureDevOps)
	}
	if cfg.Tracker.Org != "baodo0220" || cfg.Tracker.Project != "mandat-dogfood" {
		t.Errorf("Tracker = %+v, want org/project from flags", cfg.Tracker)
	}
	if cfg.Tracker.States.InProgress != config.DefaultInProgressState {
		t.Errorf("Tracker.States.InProgress = %q, want the default %q", cfg.Tracker.States.InProgress, config.DefaultInProgressState)
	}
	if cfg.Auth.Mode != config.AuthArcManagedIdentity {
		t.Errorf("Auth.Mode = %q, want arc-managed-identity", cfg.Auth.Mode)
	}
	if cfg.Entra.IdentityMode != config.IdentityAgentUserPair {
		t.Errorf("Entra.IdentityMode = %q, want %q (constant, no flag)", cfg.Entra.IdentityMode, config.IdentityAgentUserPair)
	}
	if cfg.Entra.Tenant != "d1a7b725-aaaa-bbbb-cccc-dddddddddddd" || cfg.Entra.Blueprint != "blueprint-01" {
		t.Errorf("Entra = %+v, want tenant/blueprint from flags", cfg.Entra)
	}

	repo, ok := cfg.Repos["mandat"]
	if !ok {
		t.Fatalf("Repos = %+v, want a %q entry", cfg.Repos, "mandat")
	}
	if repo.URL != "https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat" || repo.BaseBranch != "main" {
		t.Errorf("Repos[mandat] = %+v, want url/base_branch from flags", repo)
	}
	if got, want := repo.Paths, []string{"internal/", "cmd/"}; !slices.Equal(got, want) {
		t.Errorf("Repos[mandat].Paths = %v, want %v", got, want)
	}
	if got, want := repo.Gates, []string{"make check", "npx govkit check"}; !slices.Equal(got, want) {
		t.Errorf("Repos[mandat].Gates = %v, want %v", got, want)
	}

	dev, ok := cfg.Roles["dev"]
	if !ok {
		t.Fatalf("Roles = %+v, want a dev entry", cfg.Roles)
	}
	if dev.AgentIdentityID != "agent-identity-dev-01" || dev.AgentUserID != "agent-user-dev-01" || dev.AgentUserName != "dev-agent@baodo0220.onmicrosoft.com" {
		t.Errorf("Roles[dev] identity = %+v, want the dev flags", dev)
	}
	if dev.AutonomyCeiling != config.CeilingDraftPR {
		t.Errorf("Roles[dev].AutonomyCeiling = %q, want %q (from --autonomy-ceiling)", dev.AutonomyCeiling, config.CeilingDraftPR)
	}
	if dev.ModelTier != "" {
		t.Errorf("Roles[dev].ModelTier = %q, want empty (no --model flag in this slice)", dev.ModelTier)
	}
	if dev.Playbook == "" {
		t.Error("Roles[dev].Playbook is empty, want the template-derived path")
	}

	reviewer, ok := cfg.Roles["reviewer"]
	if !ok {
		t.Fatalf("Roles = %+v, want a reviewer entry", cfg.Roles)
	}
	if reviewer.AgentIdentityID != "agent-identity-reviewer-01" || reviewer.AgentUserName != "reviewer-agent@baodo0220.onmicrosoft.com" {
		t.Errorf("Roles[reviewer] identity = %+v, want the reviewer flags", reviewer)
	}
	if reviewer.AutonomyCeiling != config.CeilingReport {
		t.Errorf("Roles[reviewer].AutonomyCeiling = %q, want %q (constant, not --autonomy-ceiling)", reviewer.AutonomyCeiling, config.CeilingReport)
	}

	if cfg.Runner.PoolSize != config.DefaultPoolSize {
		t.Errorf("Runner.PoolSize = %d, want the default %d", cfg.Runner.PoolSize, config.DefaultPoolSize)
	}
	if cfg.Budget.MaxUSDPerRun != 5 {
		t.Errorf("Budget.MaxUSDPerRun = %v, want 5", cfg.Budget.MaxUSDPerRun)
	}
	if cfg.Budget.MaxUSDInFlight != 0 {
		t.Errorf("Budget.MaxUSDInFlight = %v, want 0 (derive sentinel, no flag)", cfg.Budget.MaxUSDInFlight)
	}
}

// TestInitCmd_MissingRequiredFlag proves AC-13.9's contract for every
// irreducible flag: dropping it from an otherwise-valid invocation errors
// naming that exact flag, exits non-zero, and writes nothing.
func TestInitCmd_MissingRequiredFlag(t *testing.T) {
	t.Parallel()

	flags := []string{
		"--tracker-org", "--tracker-project", "--auth-mode",
		"--entra-tenant", "--entra-blueprint", "--repo", "--base-branch",
		"--dev-identity-id", "--dev-user-id", "--dev-user-upn",
		"--reviewer-identity-id", "--reviewer-user-id", "--reviewer-user-upn",
		"--autonomy-ceiling", "--max-usd-per-run",
	}

	for _, flagName := range flags {
		t.Run(flagName, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			args := removeFlag(validInitArgs(configPath), flagName)

			var stdout, stderr strings.Builder
			code := initCmd(args, strings.NewReader(""), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("initCmd() without %s: code = 0, want non-zero", flagName)
			}
			if !strings.Contains(stderr.String(), flagName) {
				t.Errorf("stderr = %q, want it to name %s", stderr.String(), flagName)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Errorf("initCmd() without %s wrote %s, want nothing written", flagName, configPath)
			}
		})
	}

	// --remit-path and --gate are repeatable: "missing" means zero
	// occurrences, not a single flag=value pair removeFlag can strip.
	for _, repeatable := range []string{"--remit-path", "--gate"} {
		t.Run(repeatable, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			args := removeFlag(removeFlag(validInitArgs(configPath), repeatable), repeatable)

			var stdout, stderr strings.Builder
			code := initCmd(args, strings.NewReader(""), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("initCmd() without any %s: code = 0, want non-zero", repeatable)
			}
			if !strings.Contains(stderr.String(), repeatable) {
				t.Errorf("stderr = %q, want it to name %s", stderr.String(), repeatable)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Errorf("initCmd() without any %s wrote %s, want nothing written", repeatable, configPath)
			}
		})
	}
}

func TestInitCmd_InvalidEnumValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		flag    string
		value   string
		wantSub string
	}{
		{"bad auth-mode", "--auth-mode", "password", "--auth-mode"},
		{"bad autonomy-ceiling", "--autonomy-ceiling", "full-auto", "--autonomy-ceiling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			args := replaceFlagValue(validInitArgs(configPath), tt.flag, tt.value)

			var stdout, stderr strings.Builder
			code := initCmd(args, strings.NewReader(""), &stdout, &stderr)
			if code == 0 {
				t.Fatalf("initCmd() with %s=%s: code = 0, want non-zero", tt.flag, tt.value)
			}
			if !strings.Contains(stderr.String(), tt.wantSub) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantSub)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Errorf("initCmd() with an invalid %s wrote %s, want nothing written", tt.flag, configPath)
			}
		})
	}
}

func TestInitCmd_InvalidRepoFormat(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	args := replaceFlagValue(validInitArgs(configPath), "--repo", "mandat-without-an-equals-sign")

	var stdout, stderr strings.Builder
	code := initCmd(args, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatal("initCmd() with a malformed --repo: code = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "--repo") {
		t.Errorf("stderr = %q, want it to name --repo", stderr.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("initCmd() with a malformed --repo wrote %s, want nothing written", configPath)
	}
}

func replaceFlagValue(args []string, name, value string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := range out {
		if out[i] == name && i+1 < len(out) {
			out[i+1] = value
		}
	}
	return out
}

// TestInitCmd_NonTTYStdin_NeverBlocks proves AC-13.9's non-TTY autodetect:
// running init with stdin from /dev/null (never a terminal) completes
// without --non-interactive and without hanging on a stdin read, for both a
// valid flag set and one missing a required flag.
func TestInitCmd_NonTTYStdin_NeverBlocks(t *testing.T) {
	t.Parallel()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { devNull.Close() })

	if isTTY(devNull) {
		t.Fatalf("isTTY(%s) = true, want false", os.DevNull)
	}

	cases := []struct {
		name string
		args func(configPath string) []string
	}{
		{"valid flags, no --non-interactive", func(p string) []string {
			return removeFlag(validInitArgs(p), "--non-interactive")
		}},
		{"missing flag, no --non-interactive", func(p string) []string {
			return removeFlag(removeFlag(validInitArgs(p), "--non-interactive"), "--tracker-org")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			done := make(chan int, 1)
			go func() {
				var stdout, stderr strings.Builder
				done <- initCmd(tc.args(configPath), devNull, &stdout, &stderr)
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("initCmd() with stdin from /dev/null did not return within 5s, want no hang")
			}
		})
	}
}

func TestIsTTY_NonFileReader(t *testing.T) {
	t.Parallel()
	if isTTY(strings.NewReader("")) {
		t.Error("isTTY(strings.Reader) = true, want false (never a live terminal)")
	}
}

// TestConfigLoad_TruncatedInitOutput_YieldsFieldError proves AC-13.4/AC-5:
// feeding config.Load a config.yaml that init's writer stopped partway
// through (an aborted run, simulated by truncating a valid rendered
// document mid-file) yields the existing config.ValidationErrors/FieldError
// shape, with Path naming the exact dotted field, not a generic parse
// failure or a new error type.
func TestConfigLoad_TruncatedInitOutput_YieldsFieldError(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	code := initCmd(validInitArgs(configPath), strings.NewReader(""), new(strings.Builder), new(strings.Builder))
	if code != 0 {
		t.Fatalf("initCmd() code = %d, want 0", code)
	}
	full, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}

	// Truncate right after the tracker/auth/entra sections, before repos and
	// roles ever appear: simulates a run that aborted mid-write.
	cut := strings.Index(string(full), "repos:")
	if cut < 0 {
		t.Fatal("rendered config has no repos: section to truncate before")
	}
	truncatedPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(truncatedPath, full[:cut], 0o600); err != nil {
		t.Fatalf("write truncated fixture: %v", err)
	}

	_, loadErr := config.Load(truncatedPath)
	if loadErr == nil {
		t.Fatal("config.Load() on a truncated file: error = nil, want a validation error")
	}

	var verrs config.ValidationErrors
	if !errors.As(loadErr, &verrs) {
		t.Fatalf("config.Load() error type = %T, want config.ValidationErrors", loadErr)
	}

	wantPaths := []string{"repos", "roles"}
	for _, want := range wantPaths {
		found := false
		for _, fe := range verrs {
			if fe.Path == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ValidationErrors = %v, want a FieldError with Path %q", verrs, want)
		}
	}
}

// TestInitCmd_RenderedComments_CoverEveryOmitemptyField proves AC-13.2 /
// bullet 4: every omitempty-tagged field in config.go that this slice takes
// no flag for gets an adjacent comment in the rendered YAML naming its
// default, its derive rule, or its no-default omission behavior.
func TestInitCmd_RenderedComments_CoverEveryOmitemptyField(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	code := initCmd(validInitArgs(configPath), strings.NewReader(""), new(strings.Builder), new(strings.Builder))
	if code != 0 {
		t.Fatalf("initCmd() code = %d, want 0", code)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}
	yamlText := string(got)

	tests := []struct {
		field   string // the omitempty yaml key this comment documents
		wantSub []string
	}{
		{"tracker.states.in_progress", []string{"in_progress", "Doing"}},
		{"runner.pool_size", []string{"pool_size", "1"}},
		{"budget.max_usd_in_flight", []string{"max_usd_in_flight", "pool_size", "max_usd_per_run"}},
		{"roles.*.model_tier", []string{"model_tier", "no --model flag is passed"}},
		{"roles.*.skills", []string{"skills", "no default"}},
		{"roles.*.remit_paths", []string{"remit_paths", "no default"}},
		{"repos.*.gates", []string{"gates", "no default"}},
		{"notifications.teams", []string{"teams", "no default"}},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()
			for _, sub := range tt.wantSub {
				if !strings.Contains(yamlText, sub) {
					t.Errorf("rendered config.yaml has no comment for %s: missing %q\n%s", tt.field, sub, yamlText)
				}
			}
		})
	}
}
