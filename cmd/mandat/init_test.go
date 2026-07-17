package main

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baodq97/mandat/internal/config"
	"github.com/baodq97/mandat/internal/discovery"
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

// synthPreflight returns a preflightChecks stand-in yielding exactly results,
// so a finishInit test drives the doctor table and its tri-state exit with no
// live claude/git/ADO probe (mirrors runChecks' no-live-environment split).
func synthPreflight(results ...checkResult) func(*config.Config) []func(context.Context) checkResult {
	return func(*config.Config) []func(context.Context) checkResult {
		checks := make([]func(context.Context) checkResult, len(results))
		for i, r := range results {
			checks[i] = func(context.Context) checkResult { return r }
		}
		return checks
	}
}

// swapPreflightChecks installs build as init's preflight builder for the test's
// duration, restoring the production builder on cleanup. A test that swaps it
// runs non-parallel: preflightChecks is package state and -race rejects a
// concurrent write.
func swapPreflightChecks(t *testing.T, build func(*config.Config) []func(context.Context) checkResult) {
	t.Helper()
	saved := preflightChecks
	preflightChecks = build
	t.Cleanup(func() { preflightChecks = saved })
}

// stubPassPreflight swaps in an all-PASS synthetic preflight for tests that
// drive initCmd end to end without asserting on the preflight itself, keeping
// them hermetic now that a successful init closes with finishInit's doctor run.
func stubPassPreflight(t *testing.T) {
	t.Helper()
	swapPreflightChecks(t, synthPreflight(checkResult{name: "preflight", required: true, ok: true, detail: "stubbed pass"}))
}

// swapSystemdTarget points init's systemd-unit resolver at home with the CURRENT
// uid/gid, so writeSystemdUnit's chown-to-self succeeds without root and the unit
// lands under a t.TempDir() instead of the real operator home. Restores the
// production resolver on cleanup. A test that swaps it runs non-parallel:
// systemdTarget is package state and -race rejects a concurrent write.
func swapSystemdTarget(t *testing.T, home string) {
	t.Helper()
	saved := systemdTarget
	systemdTarget = func() (string, int, int, error) {
		return filepath.Join(home, ".config/systemd/user"), os.Getuid(), os.Getgid(), nil
	}
	t.Cleanup(func() { systemdTarget = saved })
}

func TestInitCmd_NonInteractive_HappyPathEmitAndReload(t *testing.T) {
	stubPassPreflight(t)

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
	stubPassPreflight(t)

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
	stubPassPreflight(t)

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

// TestConfigLoad_MissingRoleUPN_YieldsDottedFieldError proves AC-13.4 at a
// nested path, not just the top-level repos/roles case above: a rendered
// config with roles.dev.agent_user_name blanked (otherwise valid, under the
// always-on entra.identity_mode: agent-user-pair) still yields the
// config.ValidationErrors/FieldError shape, with Path naming the exact
// dotted field and a non-empty Reason stating the fix — not a generic parse
// failure.
func TestConfigLoad_MissingRoleUPN_YieldsDottedFieldError(t *testing.T) {
	stubPassPreflight(t)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	code := initCmd(validInitArgs(configPath), strings.NewReader(""), new(strings.Builder), new(strings.Builder))
	if code != 0 {
		t.Fatalf("initCmd() code = %d, want 0", code)
	}
	full, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}

	// renderRole (init.go) writes this exact line for the dev role's UPN
	// (validInitArgs' --dev-user-upn); blank its value to simulate init
	// writing the role block with the UPN omitted.
	devUPNLine := "    agent_user_name: dev-agent@baodo0220.onmicrosoft.com\n"
	blanked := strings.Replace(string(full), devUPNLine, "    agent_user_name:\n", 1)
	if blanked == string(full) {
		t.Fatal("rendered config has no dev agent_user_name line to blank")
	}
	blankedPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(blankedPath, []byte(blanked), 0o600); err != nil {
		t.Fatalf("write blanked-UPN fixture: %v", err)
	}

	_, loadErr := config.Load(blankedPath)
	if loadErr == nil {
		t.Fatal("config.Load() on a config missing roles.dev.agent_user_name: error = nil, want a validation error")
	}

	var verrs config.ValidationErrors
	if !errors.As(loadErr, &verrs) {
		t.Fatalf("config.Load() error type = %T, want config.ValidationErrors", loadErr)
	}

	const wantPath = "roles.dev.agent_user_name"
	found := false
	for _, fe := range verrs {
		if fe.Path != wantPath {
			continue
		}
		found = true
		if len(fe.Reason) == 0 {
			t.Errorf("FieldError{Path: %q}.Reason is empty, want it to state the fix", wantPath)
		}
		if !strings.Contains(fe.Reason, "agent-user-pair") {
			t.Errorf("FieldError{Path: %q}.Reason = %q, want it to mention agent-user-pair (config.go's identity_mode gate)", wantPath, fe.Reason)
		}
	}
	if !found {
		t.Errorf("ValidationErrors = %v, want a FieldError with Path %q", verrs, wantPath)
	}
}

// TestInitCmd_RenderedComments_CoverEveryOmitemptyField proves AC-13.2 /
// bullet 4: every omitempty-tagged field in config.go that this slice takes
// no flag for gets an adjacent comment in the rendered YAML naming its
// default, its derive rule, or its no-default omission behavior.
func TestInitCmd_RenderedComments_CoverEveryOmitemptyField(t *testing.T) {
	stubPassPreflight(t)

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
			for _, sub := range tt.wantSub {
				if !strings.Contains(yamlText, sub) {
					t.Errorf("rendered config.yaml has no comment for %s: missing %q\n%s", tt.field, sub, yamlText)
				}
			}
		})
	}
}

// validInteractiveScriptLines returns one scripted answer per
// runInteractiveInterview prompt, in prompt order, equivalent to
// validInitArgs' flag values: a full, valid interview from tracker.org
// through budget.max_usd_per_run, closing with a blank (declines the systemd
// unit, AC-13.6). Index 2 is tracker.states.in_progress (blank keeps the
// default) and index 22 is runner.pool_size (blank keeps the default); tests
// that exercise those two fields index into this slice directly, so keep it in
// sync with runInteractiveInterview's prompt order.
func validInteractiveScriptLines() []string {
	return []string{
		"baodo0220",      // tracker.org
		"mandat-dogfood", // tracker.project
		"",               // tracker.states.in_progress [Doing]
		"arc-managed-identity",
		"d1a7b725-aaaa-bbbb-cccc-dddddddddddd",
		"blueprint-01",
		"mandat", // repo key
		"https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat",
		"main", // base_branch
		"internal/",
		"cmd/",
		"", // end remit paths
		"make check",
		"npx govkit check",
		"", // end gates
		"agent-identity-dev-01",
		"agent-user-dev-01",
		"dev-agent@baodo0220.onmicrosoft.com",
		"agent-identity-reviewer-01",
		"agent-user-reviewer-01",
		"reviewer-agent@baodo0220.onmicrosoft.com",
		"draft-pr",
		"",  // runner.pool_size [1]
		"5", // budget.max_usd_per_run
		"",  // install systemd unit [y/N] — decline
	}
}

func newInteractiveScript(lines []string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

// failingTokenSource simulates az missing or the operator not being logged
// in, so every test exercising the manual-entry interview flow does so
// deterministically, with no real az invocation and no dependence on the
// test host's environment (US-0013 AC-13.1).
func failingTokenSource(context.Context) (string, error) {
	return "", errors.New("az: command not found")
}

// unreachableDiscoverer fails a test if the interview ever calls discover
// after its token source already failed: discovery is only attempted with a
// token in hand.
func unreachableDiscoverer(t *testing.T) discoverer {
	t.Helper()
	return func(context.Context, string) (discovery.Result, error) {
		t.Fatal("discoverer called despite a failed token source")
		return discovery.Result{}, nil
	}
}

func TestRunInteractiveInterview_HappyPath_EmitAndReload(t *testing.T) {
	t.Parallel()

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, failingTokenSource, unreachableDiscoverer(t), nil)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if err := in.validate(); err != nil {
		t.Fatalf("in.validate() error = %v, want nil (transcript: %s)", err, transcript.String())
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr strings.Builder
	if code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("")), true, &stdout, &stderr); code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}
	if cfg.Tracker.Org != "baodo0220" || cfg.Tracker.Project != "mandat-dogfood" {
		t.Errorf("Tracker = %+v, want org/project from the interview", cfg.Tracker)
	}
	if cfg.Tracker.States.InProgress != config.DefaultInProgressState {
		t.Errorf("Tracker.States.InProgress = %q, want the default %q (blank Enter)", cfg.Tracker.States.InProgress, config.DefaultInProgressState)
	}
	if cfg.Auth.Mode != config.AuthArcManagedIdentity {
		t.Errorf("Auth.Mode = %q, want arc-managed-identity", cfg.Auth.Mode)
	}
	if cfg.Entra.Tenant != "d1a7b725-aaaa-bbbb-cccc-dddddddddddd" || cfg.Entra.Blueprint != "blueprint-01" {
		t.Errorf("Entra = %+v, want tenant/blueprint from the interview", cfg.Entra)
	}

	repo, ok := cfg.Repos["mandat"]
	if !ok {
		t.Fatalf("Repos = %+v, want a %q entry", cfg.Repos, "mandat")
	}
	if repo.URL != "https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat" || repo.BaseBranch != "main" {
		t.Errorf("Repos[mandat] = %+v, want url/base_branch from the interview", repo)
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
	if dev.AgentIdentityID != "agent-identity-dev-01" || dev.AgentUserName != "dev-agent@baodo0220.onmicrosoft.com" {
		t.Errorf("Roles[dev] identity = %+v, want the interview's dev answers", dev)
	}
	if dev.AutonomyCeiling != config.CeilingDraftPR {
		t.Errorf("Roles[dev].AutonomyCeiling = %q, want %q", dev.AutonomyCeiling, config.CeilingDraftPR)
	}

	reviewer, ok := cfg.Roles["reviewer"]
	if !ok {
		t.Fatalf("Roles = %+v, want a reviewer entry", cfg.Roles)
	}
	if reviewer.AutonomyCeiling != config.CeilingReport {
		t.Errorf("Roles[reviewer].AutonomyCeiling = %q, want %q (constant, never prompted)", reviewer.AutonomyCeiling, config.CeilingReport)
	}

	if cfg.Runner.PoolSize != config.DefaultPoolSize {
		t.Errorf("Runner.PoolSize = %d, want the default %d (blank Enter)", cfg.Runner.PoolSize, config.DefaultPoolSize)
	}
	if cfg.Budget.MaxUSDPerRun != 5 {
		t.Errorf("Budget.MaxUSDPerRun = %v, want 5", cfg.Budget.MaxUSDPerRun)
	}
}

// TestRunInteractiveInterview_EmptyRequiredField_RePrompts proves US-0013
// AC-13.3(c) bullet 2: a required field left empty re-prompts rather than
// accepting the blank answer, so the interview never hands validate/render
// an invalid config.
func TestRunInteractiveInterview_EmptyRequiredField_RePrompts(t *testing.T) {
	t.Parallel()

	lines := append([]string{""}, validInteractiveScriptLines()...) // blank answer to the first prompt, tracker.org

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), &transcript, failingTokenSource, unreachableDiscoverer(t), nil)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if in.trackerOrg != "baodo0220" {
		t.Errorf("trackerOrg = %q, want %q (the value given after the re-prompt)", in.trackerOrg, "baodo0220")
	}
	if !strings.Contains(transcript.String(), "try again") {
		t.Errorf("transcript = %q, want a re-prompt message after the blank tracker.org answer", transcript.String())
	}
}

// TestRunInteractiveInterview_EnterKeepsDefault proves US-0013 AC-13.3(c)
// bullet 2 for the two applyDefaults fields: the prompt shows the default
// in brackets, and a blank Enter keeps it, so config.Load resolves the same
// default value applyDefaults would apply to an omitted field.
func TestRunInteractiveInterview_EnterKeepsDefault(t *testing.T) {
	t.Parallel()

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, failingTokenSource, unreachableDiscoverer(t), nil)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if in.inProgressState != "" {
		t.Errorf("inProgressState = %q, want empty (blank Enter keeps the built-in default)", in.inProgressState)
	}
	if in.poolSize != 0 {
		t.Errorf("poolSize = %d, want 0 (blank Enter keeps the built-in default)", in.poolSize)
	}
	if !strings.Contains(transcript.String(), "["+config.DefaultInProgressState+"]") {
		t.Errorf("transcript = %q, want the in_progress prompt to show the default %q in brackets", transcript.String(), config.DefaultInProgressState)
	}
	if !strings.Contains(transcript.String(), "[1]") {
		t.Errorf("transcript = %q, want the pool_size prompt to show the default 1 in brackets", transcript.String())
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("")), true, new(strings.Builder), new(strings.Builder)); code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0", code)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}
	if cfg.Tracker.States.InProgress != config.DefaultInProgressState {
		t.Errorf("Tracker.States.InProgress = %q, want the default %q", cfg.Tracker.States.InProgress, config.DefaultInProgressState)
	}
	if cfg.Runner.PoolSize != config.DefaultPoolSize {
		t.Errorf("Runner.PoolSize = %d, want the default %d", cfg.Runner.PoolSize, config.DefaultPoolSize)
	}
}

// TestRunInteractiveInterview_OverridesDefaultedField proves a non-blank
// answer to a defaulted-field prompt actually overrides it end to end,
// exercising the else branch TestRunInteractiveInterview_EnterKeepsDefault
// never touches.
func TestRunInteractiveInterview_OverridesDefaultedField(t *testing.T) {
	t.Parallel()

	lines := validInteractiveScriptLines()
	lines[2] = "InProgress"
	lines[22] = "3"

	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), new(strings.Builder), failingTokenSource, unreachableDiscoverer(t), nil)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v", err)
	}
	if in.inProgressState != "InProgress" {
		t.Errorf("inProgressState = %q, want the entered override %q", in.inProgressState, "InProgress")
	}
	if in.poolSize != 3 {
		t.Errorf("poolSize = %d, want the entered override 3", in.poolSize)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("")), true, new(strings.Builder), new(strings.Builder)); code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0", code)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}
	if cfg.Tracker.States.InProgress != "InProgress" {
		t.Errorf("Tracker.States.InProgress = %q, want the override %q", cfg.Tracker.States.InProgress, "InProgress")
	}
	if cfg.Runner.PoolSize != 3 {
		t.Errorf("Runner.PoolSize = %d, want the override 3", cfg.Runner.PoolSize)
	}
}

// TestReconstructPriorInput_RoundTrip proves the AC-13.11 byte-identical
// invariant at the reconstruct↔render seam: an init-written config read back
// through reconstructPriorInput and re-rendered is byte-for-byte the original,
// for both a config that SETS the two optional fields
// (tracker.states.in_progress, runner.pool_size → present as YAML) and one that
// leaves them at their defaults (commented). The commented case is the
// load-bearing one — reconstruct must read raw, never config.Load, whose
// applyDefaults would resolve an omitted optional into a written value and make
// render emit a field the operator never touched.
func TestReconstructPriorInput_RoundTrip(t *testing.T) {
	t.Parallel()

	present := validNonInteractiveInput()
	present.inProgressState = "InReview"
	present.poolSize = 3

	for _, tc := range []struct {
		name string
		in   nonInteractiveInput
	}{
		{"optionals commented", validNonInteractiveInput()},
		{"optionals present", present},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := tc.in.render()
			got, ok := reconstructPriorInput([]byte(want))
			if !ok {
				t.Fatal("reconstructPriorInput() ok = false, want true for an init-written config")
			}
			if roundTrip := got.render(); roundTrip != want {
				t.Errorf("reconstruct→render is not byte-identical\n--- want ---\n%s\n--- got ---\n%s", want, roundTrip)
			}
		})
	}
}

// TestReconstructPriorInput_EmptyReturnsFalse proves the ok contract: an empty
// or content-free document reconstructs nothing, so initCmd falls back to a
// fresh interview instead of seeding prompts from an unusable file.
func TestReconstructPriorInput_EmptyReturnsFalse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"comments only", "# just a header, no fields\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := reconstructPriorInput([]byte(tc.data)); ok {
				t.Errorf("reconstructPriorInput(%q) ok = true, want false", tc.data)
			}
		})
	}
}

// TestRunInteractiveInterview_Rerun_AllEnter_ByteIdentical proves AC-13.11 end
// to end: a second interview seeded with a prior config shows each stored value
// as its bracketed prompt default (property a) and, when the operator changes
// nothing (a bare Enter through every prompt), reconstructs an input that
// renders byte-for-byte the file that seeded it (property b) — for both the
// commented and the present optional-field states. The failing token source and
// the fatal-on-call discoverer prove a re-run never probes Azure DevOps: the
// existing config, not discovery, is the source of truth.
func TestRunInteractiveInterview_Rerun_AllEnter_ByteIdentical(t *testing.T) {
	t.Parallel()

	present := validNonInteractiveInput()
	present.inProgressState = "InReview"
	present.poolSize = 3

	for _, tc := range []struct {
		name string
		in   nonInteractiveInput
	}{
		{"optionals commented", validNonInteractiveInput()},
		{"optionals present", present},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := tc.in.render()
			prior, ok := reconstructPriorInput([]byte(want))
			if !ok {
				t.Fatal("reconstructPriorInput() ok = false, want true")
			}

			// One blank line per prompt: a bare Enter through the whole re-run
			// keeps every default. Keep this count in step with the prompt order
			// in runInteractiveInterview (the trailing systemd confirm is one).
			const rerunPromptCount = 21
			reader := bufio.NewReader(strings.NewReader(strings.Repeat("\n", rerunPromptCount)))

			var transcript strings.Builder
			got, err := runInteractiveInterview(context.Background(), reader, &transcript, failingTokenSource, unreachableDiscoverer(t), &prior)
			if err != nil {
				t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
			}

			if roundTrip := got.render(); roundTrip != want {
				t.Errorf("all-Enter re-run is not byte-identical to the prior config\n--- want ---\n%s\n--- got ---\n%s", want, roundTrip)
			}
			if bracket := "[" + prior.trackerOrg + "]"; !strings.Contains(transcript.String(), bracket) {
				t.Errorf("transcript = %q, want the tracker.org prompt to show the prior value %q as a bracketed default", transcript.String(), bracket)
			}
		})
	}
}

// fakeADOServer serves the four discovery endpoints from canned JSON bodies,
// standing in for Azure DevOps so these tests never dial a real host
// (US-0013 AC-13.1).
func fakeADOServer(t *testing.T, profileBody, accountsBody, projectsBody, reposBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_apis/profile/profiles/me"):
			_, _ = w.Write([]byte(profileBody))
		case strings.HasSuffix(r.URL.Path, "/_apis/accounts"):
			_, _ = w.Write([]byte(accountsBody))
		case strings.HasSuffix(r.URL.Path, "/_apis/projects"):
			_, _ = w.Write([]byte(projectsBody))
		case strings.HasSuffix(r.URL.Path, "/_apis/git/repositories"):
			_, _ = w.Write([]byte(reposBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// discovererFor builds a discoverer bound to srv, standing in for
// productionDiscoverer in tests.
func discovererFor(t *testing.T, srv *httptest.Server) discoverer {
	t.Helper()
	c, err := discovery.New(discovery.Config{VSSPSBaseURL: srv.URL, AzureDevOpsBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("discovery.New() error = %v", err)
	}
	return c.Discover
}

// fakeADOTokenSource always returns token with no az invocation.
func fakeADOTokenSource(token string) tokenSource {
	return func(context.Context) (string, error) {
		return token, nil
	}
}

// TestRunInteractiveInterview_DiscoverySuccess_ConfirmProducesLoadableConfig
// proves US-0013 AC-13.1: a successful discovery prefills
// tracker.org, tracker.project, and repo url, Enter accepts each discovered
// value without re-typing it, and the confirmed values flow through the
// same writeConfig/render path into a config.Load-acceptable file.
func TestRunInteractiveInterview_DiscoverySuccess_ConfirmProducesLoadableConfig(t *testing.T) {
	t.Parallel()

	srv := fakeADOServer(t,
		`{"id":"11111111-0000-4000-8000-000000000001"}`,
		`{"count":1,"value":[{"accountId":"22222222-0000-4000-8000-000000000002","accountName":"baodo0220"}]}`,
		`{"count":1,"value":[{"id":"33333333-0000-4000-8000-000000000003","name":"mandat-pilot"}]}`,
		`{"count":1,"value":[{"id":"44444444-0000-4000-8000-000000000004","name":"mandat","remoteUrl":"https://dev.azure.com/baodo0220/mandat-pilot/_git/mandat"}]}`,
	)

	lines := validInteractiveScriptLines()
	lines[0] = "" // tracker.org: accept the discovered value
	lines[1] = "" // tracker.project: accept the discovered value
	lines[7] = "" // repo url: accept the discovered value

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), &transcript, fakeADOTokenSource(testFakeToken), discovererFor(t, srv), nil)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if err := in.validate(); err != nil {
		t.Fatalf("in.validate() error = %v, want nil (transcript: %s)", err, transcript.String())
	}
	if in.trackerOrg != "baodo0220" {
		t.Errorf("trackerOrg = %q, want the discovered org %q", in.trackerOrg, "baodo0220")
	}
	if in.trackerProject != "mandat-pilot" {
		t.Errorf("trackerProject = %q, want the discovered project %q", in.trackerProject, "mandat-pilot")
	}
	if in.repoURL != "https://dev.azure.com/baodo0220/mandat-pilot/_git/mandat" {
		t.Errorf("repoURL = %q, want the discovered remote url", in.repoURL)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr strings.Builder
	if code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("")), true, &stdout, &stderr); code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}
	if cfg.Tracker.Org != "baodo0220" || cfg.Tracker.Project != "mandat-pilot" {
		t.Errorf("Tracker = %+v, want the confirmed discovered org/project", cfg.Tracker)
	}
	repo, ok := cfg.Repos["mandat"]
	if !ok || repo.URL != "https://dev.azure.com/baodo0220/mandat-pilot/_git/mandat" {
		t.Errorf("Repos[mandat] = %+v, want the confirmed discovered remote url", cfg.Repos)
	}
}

// testFakeToken is the bearer token fakeADOTokenSource hands the discoverer in
// tests; it is never sent to a real host, only to fakeADOServer.
const testFakeToken = "fake-bearer-token"

// TestRunInteractiveInterview_AmbiguousOrg_FallsBackToPrompting proves
// US-0013 AC-13.1: a typed AmbiguousOrgError falls back to prompting for
// org (and, since discovery never reached project/repo, those too) through
// the same manual prompts the no-discovery path uses.
func TestRunInteractiveInterview_AmbiguousOrg_FallsBackToPrompting(t *testing.T) {
	t.Parallel()

	srv := fakeADOServer(t,
		`{"id":"11111111-0000-4000-8000-000000000001"}`,
		`{"count":2,"value":[
			{"accountId":"aaaaaaaa-0000-4000-8000-000000000001","accountName":"baodo0220"},
			{"accountId":"bbbbbbbb-0000-4000-8000-000000000002","accountName":"other-org"}
		]}`,
		"", "",
	)

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, fakeADOTokenSource(testFakeToken), discovererFor(t, srv), nil)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if !strings.Contains(transcript.String(), "note:") || !strings.Contains(transcript.String(), "more than one organization") {
		t.Errorf("transcript = %q, want a one-line note naming the ambiguous-org failure", transcript.String())
	}
	if in.trackerOrg != "baodo0220" {
		t.Errorf("trackerOrg = %q, want the manually typed %q", in.trackerOrg, "baodo0220")
	}

	if err := in.validate(); err != nil {
		t.Fatalf("in.validate() error = %v, want nil (transcript: %s)", err, transcript.String())
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("")), true, new(strings.Builder), new(strings.Builder)); code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0", code)
	}
	if _, err := config.Load(configPath); err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}
}

// TestRunInteractiveInterview_TokenSourceFailure_FallsBackToPrompting proves
// US-0013 AC-13.1: a token source failure prints a one-line note and
// falls back to the full manual prompt path, with the discoverer never
// invoked (no az call, no network call, of any kind).
func TestRunInteractiveInterview_TokenSourceFailure_FallsBackToPrompting(t *testing.T) {
	t.Parallel()

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, failingTokenSource, unreachableDiscoverer(t), nil)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if !strings.Contains(transcript.String(), "note:") {
		t.Errorf("transcript = %q, want a one-line note about the token source failure", transcript.String())
	}
	if in.trackerOrg != "baodo0220" || in.trackerProject != "mandat-dogfood" {
		t.Errorf("in = %+v, want the manually typed org/project from the fallback prompts", in)
	}

	if err := in.validate(); err != nil {
		t.Fatalf("in.validate() error = %v, want nil (transcript: %s)", err, transcript.String())
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("")), true, new(strings.Builder), new(strings.Builder)); code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0", code)
	}
	if _, err := config.Load(configPath); err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}
}

// TestPlaybookTemplate_Dev proves the dev half of AC-13.5: the embedded dev
// template names the implement / self-review / commit-push / ResultContract-write
// sequence.
func TestPlaybookTemplate_Dev(t *testing.T) {
	t.Parallel()

	content, ok := config.PlaybookTemplate("dev")
	if !ok {
		t.Fatal(`PlaybookTemplate("dev") ok = false, want true`)
	}
	text := string(content)
	for _, want := range []string{"commit", "push", "ResultContract"} {
		if !strings.Contains(text, want) {
			t.Errorf("dev playbook missing %q; it names the commit-push/ResultContract sequence (AC-13.5)", want)
		}
	}
}

// TestPlaybookTemplate_Reviewer proves the reviewer half of AC-13.5: the
// embedded reviewer template names the read / probe / report sequence and
// carries no commit or push step. The template states "no commits" as a limit,
// so the bare word "commit" is present by design; AC-13.5's "no commit step"
// means no git commit command, which is what the dev template carries.
func TestPlaybookTemplate_Reviewer(t *testing.T) {
	t.Parallel()

	content, ok := config.PlaybookTemplate("reviewer")
	if !ok {
		t.Fatal(`PlaybookTemplate("reviewer") ok = false, want true`)
	}
	text := string(content)
	for _, want := range []string{"probe", "report"} {
		if !strings.Contains(text, want) {
			t.Errorf("reviewer playbook missing %q; it names the read/probe/report sequence (AC-13.5)", want)
		}
	}
	if strings.Contains(text, "push") {
		t.Errorf("reviewer playbook contains %q, want no push step (AC-13.5)", "push")
	}
	if strings.Contains(text, "git commit") {
		t.Error("reviewer playbook contains a git commit step, want none (AC-13.5)")
	}
}

// TestPlaybookTemplate_UnknownRole proves the accessor's ok contract: a role
// with no shipped template returns ok=false and nil content.
func TestPlaybookTemplate_UnknownRole(t *testing.T) {
	t.Parallel()

	content, ok := config.PlaybookTemplate("nope")
	if ok {
		t.Error(`PlaybookTemplate("nope") ok = true, want false (no shipped template)`)
	}
	if content != nil {
		t.Errorf(`PlaybookTemplate("nope") content = %q, want nil`, content)
	}
}

// TestInitCmd_WritesPerRolePlaybooks proves AC-13.5 end to end: init writes the
// embedded playbook template to each role's configured playbook path beside the
// config, and the written content differs per role (the reviewer's has no push
// step).
func TestInitCmd_WritesPerRolePlaybooks(t *testing.T) {
	stubPassPreflight(t)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr strings.Builder
	if code := initCmd(validInitArgs(configPath), strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("initCmd() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	configDir := filepath.Dir(configPath)
	dev, err := os.ReadFile(filepath.Join(configDir, "playbooks", "dev.md"))
	if err != nil {
		t.Fatalf("read dev playbook: %v", err)
	}
	reviewer, err := os.ReadFile(filepath.Join(configDir, "playbooks", "reviewer.md"))
	if err != nil {
		t.Fatalf("read reviewer playbook: %v", err)
	}

	if string(dev) == string(reviewer) {
		t.Error("dev and reviewer playbooks are identical, want per-role content (AC-13.5)")
	}
	if strings.Contains(string(reviewer), "push") {
		t.Errorf("reviewer playbook contains %q, want no push step (AC-13.5)", "push")
	}
}

// TestInitCmd_InstallSystemdUnit_WritesUnitToOperatorHome proves AC-13.6's yes
// path: with --install-systemd-unit, init writes the GETTING-STARTED §7 unit
// (ExecStart sourcing the env file, Restart=on-failure) to the operator's
// ~/.config/systemd/user and prints — never runs — the enable commands.
func TestInitCmd_InstallSystemdUnit_WritesUnitToOperatorHome(t *testing.T) {
	stubPassPreflight(t)
	home := t.TempDir()
	swapSystemdTarget(t, home)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	args := append(validInitArgs(configPath), "--install-systemd-unit")

	var stdout, stderr strings.Builder
	if code := initCmd(args, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("initCmd() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	unitPath := filepath.Join(home, ".config", "systemd", "user", "mandat.service")
	got, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read %s: %v", unitPath, err)
	}
	unit := string(got)
	for _, want := range []string{"ExecStart=/bin/sh -c", "set -a; . ", "mandat.env", "serve --config", "Restart=on-failure"} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit missing %q\n%s", want, unit)
		}
	}
	if !strings.Contains(stdout.String(), "systemctl --user enable") {
		t.Errorf("stdout = %q, want the printed (not executed) systemctl enable command", stdout.String())
	}
}

// TestInitCmd_NoSystemdUnit_WritesNothing proves AC-13.6's default/no path:
// without --install-systemd-unit, init writes no unit file and makes no
// systemctl call.
func TestInitCmd_NoSystemdUnit_WritesNothing(t *testing.T) {
	stubPassPreflight(t)
	home := t.TempDir()
	swapSystemdTarget(t, home)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr strings.Builder
	if code := initCmd(validInitArgs(configPath), strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("initCmd() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	unitPath := filepath.Join(home, ".config", "systemd", "user", "mandat.service")
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("init wrote %s without --install-systemd-unit, want no unit file (err = %v)", unitPath, err)
	}
	if strings.Contains(stdout.String(), "systemctl") {
		t.Errorf("stdout = %q, want no systemctl call attempt on the no-systemd path", stdout.String())
	}
}

// TestFinishInit_AllPass_PrintsTableAndHandoff proves AC-13.7 + AC-13.13: a
// completed init closes by running the doctor preflight against the config it
// just wrote — the identical CHECK/STATUS/DETAIL table, here an injected
// synthetic PASS check — then prints the handoff naming the next command and,
// from that config, each role's Entra agent user plus the remit paths this VM
// now operates under, exiting zero when every check passes.
func TestFinishInit_AllPass_PrintsTableAndHandoff(t *testing.T) {
	swapPreflightChecks(t, synthPreflight(checkResult{name: "synthetic", required: true, ok: true, detail: "stubbed pass"}))

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr strings.Builder
	code := initCmd(validInitArgs(configPath), strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	// The doctor table shape and the injected check rendered PASS (AC-13.7);
	// the next command and both roles' Entra UPNs plus a remit path (AC-13.13).
	want := []string{
		"CHECK", "STATUS", "DETAIL",
		"synthetic", "PASS",
		"mandat serve",
		"dev-agent@baodo0220.onmicrosoft.com",
		"reviewer-agent@baodo0220.onmicrosoft.com",
		"internal/",
	}
	for _, sub := range want {
		if !strings.Contains(out, sub) {
			t.Errorf("finishInit stdout missing %q\n%s", sub, out)
		}
	}
}

// TestFinishInit_RequiredCheckFails_NonZeroButStillPrintsHandoff proves the
// sharp tri-state (AC-13.7 — flutter doctor's model, not brew doctor's shrug):
// a required check reporting FAIL makes init exit non-zero, yet the config is
// still on disk and the handoff (AC-13.13) still prints — the operator needs
// the security note even to fix the failing check.
func TestFinishInit_RequiredCheckFails_NonZeroButStillPrintsHandoff(t *testing.T) {
	swapPreflightChecks(t, synthPreflight(checkResult{name: "synthetic", required: true, ok: false, detail: "stubbed fail"}))

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr strings.Builder
	code := initCmd(validInitArgs(configPath), strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("initCmd() code = 0, want non-zero when a required preflight check FAILs (stderr: %s)", stderr.String())
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("config not on disk despite a failing preflight: %v (writeConfig runs before finishInit)", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "FAIL") {
		t.Errorf("finishInit stdout has no FAIL row for the failing required check\n%s", out)
	}
	if !strings.Contains(out, "mandat serve") || !strings.Contains(out, "dev-agent@baodo0220.onmicrosoft.com") {
		t.Errorf("finishInit did not print the handoff after a failing check\n%s", out)
	}
}

// TestFinishInit_ConfigLoadError_ReturnsOne proves finishInit's load-error
// branch: pointed at a path with no config, it reports the error to stderr and
// returns 1 without reaching the preflight (AC-13.7 runs against the config
// init wrote — no config, no run).
func TestFinishInit_ConfigLoadError_ReturnsOne(t *testing.T) {
	var stdout, stderr strings.Builder
	code := finishInit(context.Background(), filepath.Join(t.TempDir(), "absent.yaml"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("finishInit() code = %d, want 1 on a config load error", code)
	}
	if !strings.Contains(stderr.String(), "mandat init:") {
		t.Errorf("stderr = %q, want it to name mandat init and the load error", stderr.String())
	}
}

// validNonInteractiveInput builds a fully-populated input equivalent to
// validInitArgs, for writeConfig/diff tests that need an in without driving the
// whole interview or flag parse.
func validNonInteractiveInput() nonInteractiveInput {
	return nonInteractiveInput{
		trackerOrg:         "baodo0220",
		trackerProject:     "mandat-dogfood",
		authMode:           string(config.AuthArcManagedIdentity),
		entraTenant:        "d1a7b725-aaaa-bbbb-cccc-dddddddddddd",
		entraBlueprint:     "blueprint-01",
		repoRaw:            "mandat=https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat",
		repoKey:            "mandat",
		repoURL:            "https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat",
		baseBranch:         "main",
		remitPaths:         []string{"internal/", "cmd/"},
		gates:              []string{"make check", "npx govkit check"},
		devIdentityID:      "agent-identity-dev-01",
		devUserID:          "agent-user-dev-01",
		devUserUPN:         "dev-agent@baodo0220.onmicrosoft.com",
		reviewerIdentityID: "agent-identity-reviewer-01",
		reviewerUserID:     "agent-user-reviewer-01",
		reviewerUserUPN:    "reviewer-agent@baodo0220.onmicrosoft.com",
		autonomyCeiling:    string(config.CeilingDraftPR),
		maxUSDPerRun:       5,
	}
}

// TestRenderConfigDiff_FreshInstall_AllAdditions proves AC-13.12's fresh-install
// clause: with no existing file (oldContent == "") every new line renders as a
// "+ " addition, so the diff shown is the whole file.
func TestRenderConfigDiff_FreshInstall_AllAdditions(t *testing.T) {
	t.Parallel()

	diff := renderConfigDiff("", "a\nb\nc\n")
	want := []string{"+ a", "+ b", "+ c"}
	got := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Errorf("renderConfigDiff(fresh) = %q, want every new line as a + addition %q", got, want)
	}
	if strings.Contains(diff, "- ") {
		t.Errorf("renderConfigDiff(fresh) = %q, want no removals with an empty old", diff)
	}
}

// TestRenderConfigDiff_Identical_AllContext proves an unchanged rewrite renders
// every line as two-space context, with no +/- change rows.
func TestRenderConfigDiff_Identical_AllContext(t *testing.T) {
	t.Parallel()

	content := "a\nb\nc\n"
	diff := renderConfigDiff(content, content)
	for _, line := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") {
		if !strings.HasPrefix(line, "  ") {
			t.Errorf("renderConfigDiff(identical) line = %q, want a two-space unchanged prefix", line)
		}
	}
	if strings.Contains(diff, "+ ") || strings.Contains(diff, "- ") {
		t.Errorf("renderConfigDiff(identical) = %q, want no +/- change rows", diff)
	}
}

// TestRenderConfigDiff_SingleChangedLine proves one changed value line yields
// exactly one "- " old row and one "+ " new row for that field, with every
// other line unchanged context.
func TestRenderConfigDiff_SingleChangedLine(t *testing.T) {
	t.Parallel()

	old := "tracker:\n  org: baodo0220\n  project: mandat\n"
	updated := "tracker:\n  org: rebranded\n  project: mandat\n"
	diff := renderConfigDiff(old, updated)

	var removed, added, context []string
	for _, line := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			removed = append(removed, strings.TrimPrefix(line, "- "))
		case strings.HasPrefix(line, "+ "):
			added = append(added, strings.TrimPrefix(line, "+ "))
		default:
			context = append(context, strings.TrimPrefix(line, "  "))
		}
	}
	if want := []string{"  org: baodo0220"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %q, want exactly the changed old line %q", removed, want)
	}
	if want := []string{"  org: rebranded"}; !slices.Equal(added, want) {
		t.Errorf("added = %q, want exactly the changed new line %q", added, want)
	}
	if want := []string{"tracker:", "  project: mandat"}; !slices.Equal(context, want) {
		t.Errorf("context = %q, want the two unchanged lines", context)
	}
}

// TestWriteConfig_FreshInstall_ShowsWholeFileAndWrites proves AC-13.12 on the
// fresh path: writeConfig against a non-existent configPath prints the whole
// rendered file as "+ " additions and writes it (skipConfirm true, so the empty
// reader is never consulted).
func TestWriteConfig_FreshInstall_ShowsWholeFileAndWrites(t *testing.T) {
	t.Parallel()

	in := validNonInteractiveInput()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	var stdout, stderr strings.Builder
	code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("")), true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}
	out := stdout.String()
	for _, line := range strings.Split(strings.TrimSuffix(string(written), "\n"), "\n") {
		if !strings.Contains(out, "+ "+line) {
			t.Errorf("diff missing %q as a + addition\n%s", line, out)
		}
	}
}

// TestWriteConfig_ConfirmYes_WritesAndShowsDiff proves AC-13.12's confirm path:
// against an existing config, skipConfirm false, a scripted "y" writes the new
// content and the diff shows the change against what was there.
func TestWriteConfig_ConfirmYes_WritesAndShowsDiff(t *testing.T) {
	t.Parallel()

	in := validNonInteractiveInput()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("tracker:\n  org: stale\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var stdout, stderr strings.Builder
	code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("y\n")), false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}
	if string(written) != in.render() {
		t.Errorf("config after confirm-yes = %q, want the freshly rendered content", string(written))
	}
	out := stdout.String()
	if !strings.Contains(out, "[y/N]") {
		t.Errorf("stdout = %q, want the [y/N] confirmation prompt", out)
	}
	if !strings.Contains(out, "- ") {
		t.Errorf("stdout = %q, want the diff to show removals against the stale config", out)
	}
}

// TestWriteConfig_ConfirmNo_LeavesFileUntouched proves the decline path: a
// scripted "n" writes nothing (config bytes unchanged, no playbooks), returns
// non-zero, and says aborted.
func TestWriteConfig_ConfirmNo_LeavesFileUntouched(t *testing.T) {
	t.Parallel()

	in := validNonInteractiveInput()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("tracker:\n  org: stale\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var stdout, stderr strings.Builder
	code := writeConfig(in, configPath, bufio.NewReader(strings.NewReader("n\n")), false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("writeConfig() code = 0, want non-zero on a declined confirmation")
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}
	if string(after) != string(original) {
		t.Errorf("config after confirm-no = %q, want it untouched (%q)", string(after), string(original))
	}
	if !strings.Contains(stdout.String(), "aborted") {
		t.Errorf("stdout = %q, want an aborted message", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), "playbooks", "dev.md")); !os.IsNotExist(err) {
		t.Errorf("a playbook exists after a declined confirmation, want nothing written (err = %v)", err)
	}
}

// TestWriteConfig_SkipConfirm_DoesNotReadReader proves the --yes / non-interactive
// implication (AC-13.9): with skipConfirm the reader is never consulted, so a
// reader scripted to decline still writes and its "n" stays buffered.
func TestWriteConfig_SkipConfirm_DoesNotReadReader(t *testing.T) {
	t.Parallel()

	in := validNonInteractiveInput()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("tracker:\n  org: stale\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	reader := bufio.NewReader(strings.NewReader("n\n"))
	var stdout, stderr strings.Builder
	code := writeConfig(in, configPath, reader, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("writeConfig() code = %d, want 0 (skipConfirm ignores the reader)", code)
	}

	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}
	if string(written) != in.render() {
		t.Errorf("config = %q, want the rendered content (skipConfirm wrote despite the declining reader)", string(written))
	}
	if b, _ := reader.ReadByte(); b != 'n' {
		t.Errorf("first buffered byte = %q, want 'n' still unread (skipConfirm must not read the reader)", b)
	}
	if strings.Contains(stdout.String(), "[y/N]") {
		t.Errorf("stdout = %q, want no confirmation prompt on skipConfirm", stdout.String())
	}
}
