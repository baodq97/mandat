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
	"github.com/baodq97/mandat/internal/entra"
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
		"--tracker-org", "contoso",
		"--tracker-project", "mandat-dogfood",
		"--auth-mode", "arc-managed-identity",
		"--entra-tenant", "11111111-1111-1111-1111-111111111111",
		"--entra-blueprint", "blueprint-01",
		"--repo", "mandat=https://dev.azure.com/contoso/mandat-dogfood/_git/mandat",
		"--base-branch", "main",
		"--remit-path", "internal/",
		"--remit-path", "cmd/",
		"--gate", "make check",
		"--gate", "npx govkit check",
		"--dev-identity-id", "agent-identity-dev-01",
		"--dev-user-id", "agent-user-dev-01",
		"--dev-user-upn", "dev-agent@contoso.onmicrosoft.com",
		"--reviewer-identity-id", "agent-identity-reviewer-01",
		"--reviewer-user-id", "agent-user-reviewer-01",
		"--reviewer-user-upn", "reviewer-agent@contoso.onmicrosoft.com",
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
	if cfg.Tracker.Org != "contoso" || cfg.Tracker.Project != "mandat-dogfood" {
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
	if cfg.Entra.Tenant != "11111111-1111-1111-1111-111111111111" || cfg.Entra.Blueprint != "blueprint-01" {
		t.Errorf("Entra = %+v, want tenant/blueprint from flags", cfg.Entra)
	}

	repo, ok := cfg.Repos["mandat"]
	if !ok {
		t.Fatalf("Repos = %+v, want a %q entry", cfg.Repos, "mandat")
	}
	if repo.URL != "https://dev.azure.com/contoso/mandat-dogfood/_git/mandat" || repo.BaseBranch != "main" {
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
	if dev.AgentIdentityID != "agent-identity-dev-01" || dev.AgentUserID != "agent-user-dev-01" || dev.AgentUserName != "dev-agent@contoso.onmicrosoft.com" {
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
	if reviewer.AgentIdentityID != "agent-identity-reviewer-01" || reviewer.AgentUserName != "reviewer-agent@contoso.onmicrosoft.com" {
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
// clearInitEnv blanks every MANDAT_* var init reads (AC-13.10), so a test that
// omits a required flag observes it as truly absent rather than env-filled. Uses
// t.Setenv, so callers cannot be t.Parallel.
func clearInitEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MANDAT_TRACKER_ORG", "MANDAT_TRACKER_PROJECT", "MANDAT_TRACKER_IN_PROGRESS_STATE",
		"MANDAT_AUTH_MODE", "MANDAT_ENTRA_TENANT", "MANDAT_ENTRA_BLUEPRINT",
		"MANDAT_REPO", "MANDAT_BASE_BRANCH", "MANDAT_REMIT_PATHS", "MANDAT_GATES",
		"MANDAT_DEV_IDENTITY_ID", "MANDAT_DEV_USER_ID", "MANDAT_DEV_USER_UPN",
		"MANDAT_REVIEWER_IDENTITY_ID", "MANDAT_REVIEWER_USER_ID", "MANDAT_REVIEWER_USER_UPN",
		"MANDAT_AUTONOMY_CEILING", "MANDAT_MAX_USD_PER_RUN", "MANDAT_POOL_SIZE",
	} {
		t.Setenv(k, "")
	}
}

func TestInitCmd_MissingRequiredFlag(t *testing.T) {
	clearInitEnv(t)

	flags := []string{
		"--tracker-org", "--tracker-project", "--auth-mode",
		"--entra-tenant", "--entra-blueprint", "--repo", "--base-branch",
		"--dev-identity-id", "--dev-user-id", "--dev-user-upn",
		"--reviewer-identity-id", "--reviewer-user-id", "--reviewer-user-upn",
		"--autonomy-ceiling", "--max-usd-per-run",
	}

	for _, flagName := range flags {
		t.Run(flagName, func(t *testing.T) {
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
	devUPNLine := "    agent_user_name: dev-agent@contoso.onmicrosoft.com\n"
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
		"contoso",        // tracker.org
		"mandat-dogfood", // tracker.project
		"",               // tracker.states.in_progress [Doing]
		"arc-managed-identity",
		"11111111-1111-1111-1111-111111111111",
		"blueprint-01",
		"mandat", // repo key
		"https://dev.azure.com/contoso/mandat-dogfood/_git/mandat",
		"main", // base_branch
		"internal/",
		"cmd/",
		"", // end remit paths
		"make check",
		"npx govkit check",
		"", // end gates
		"agent-identity-dev-01",
		"agent-user-dev-01",
		"dev-agent@contoso.onmicrosoft.com",
		"agent-identity-reviewer-01",
		"agent-user-reviewer-01",
		"reviewer-agent@contoso.onmicrosoft.com",
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
// test host's environment (US-0013 AC-13.1). The account-id argument (US-0015) is
// ignored: a failing source never reaches the mint.
func failingTokenSource(context.Context, string) (string, error) {
	return "", errors.New("az: command not found")
}

// discoveryTenantPrefill is the fresh-install prefill the interview tests that
// exercise discovery pass: a resolved tenant (so discovery is attempted) and the
// az account that pins its token (US-0015), with the role identities left empty
// so those fields still prompt from the script exactly as before.
func discoveryTenantPrefill() discoveredPrefill {
	return discoveredPrefill{tenant: "11111111-1111-1111-1111-111111111111", accountID: "account-11111111"}
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
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, failingTokenSource, unreachableDiscoverer(t), nil, discoveryTenantPrefill())
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
	if cfg.Tracker.Org != "contoso" || cfg.Tracker.Project != "mandat-dogfood" {
		t.Errorf("Tracker = %+v, want org/project from the interview", cfg.Tracker)
	}
	if cfg.Tracker.States.InProgress != config.DefaultInProgressState {
		t.Errorf("Tracker.States.InProgress = %q, want the default %q (blank Enter)", cfg.Tracker.States.InProgress, config.DefaultInProgressState)
	}
	if cfg.Auth.Mode != config.AuthArcManagedIdentity {
		t.Errorf("Auth.Mode = %q, want arc-managed-identity", cfg.Auth.Mode)
	}
	if cfg.Entra.Tenant != "11111111-1111-1111-1111-111111111111" || cfg.Entra.Blueprint != "blueprint-01" {
		t.Errorf("Entra = %+v, want tenant/blueprint from the interview", cfg.Entra)
	}

	repo, ok := cfg.Repos["mandat"]
	if !ok {
		t.Fatalf("Repos = %+v, want a %q entry", cfg.Repos, "mandat")
	}
	if repo.URL != "https://dev.azure.com/contoso/mandat-dogfood/_git/mandat" || repo.BaseBranch != "main" {
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
	if dev.AgentIdentityID != "agent-identity-dev-01" || dev.AgentUserName != "dev-agent@contoso.onmicrosoft.com" {
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
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), &transcript, failingTokenSource, unreachableDiscoverer(t), nil, discoveryTenantPrefill())
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if in.trackerOrg != "contoso" {
		t.Errorf("trackerOrg = %q, want %q (the value given after the re-prompt)", in.trackerOrg, "contoso")
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
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, failingTokenSource, unreachableDiscoverer(t), nil, discoveryTenantPrefill())
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

	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), new(strings.Builder), failingTokenSource, unreachableDiscoverer(t), nil, discoveryTenantPrefill())
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
			got, err := runInteractiveInterview(context.Background(), reader, &transcript, failingTokenSource, unreachableDiscoverer(t), &prior, discoveredPrefill{})
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

// fakeADOTokenSource always returns token with no az invocation. It ignores the
// account-id argument (US-0015): tests that assert the mint pin use a capturing
// source instead.
func fakeADOTokenSource(token string) tokenSource {
	return func(context.Context, string) (string, error) {
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
		`{"count":1,"value":[{"accountId":"22222222-0000-4000-8000-000000000002","accountName":"contoso"}]}`,
		`{"count":1,"value":[{"id":"33333333-0000-4000-8000-000000000003","name":"mandat-pilot"}]}`,
		`{"count":1,"value":[{"id":"44444444-0000-4000-8000-000000000004","name":"mandat","remoteUrl":"https://dev.azure.com/contoso/mandat-pilot/_git/mandat"}]}`,
	)

	lines := validInteractiveScriptLines()
	lines[0] = "" // tracker.org: accept the discovered value
	lines[1] = "" // tracker.project: accept the discovered value
	lines[7] = "" // repo url: accept the discovered value

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), &transcript, fakeADOTokenSource(testFakeToken), discovererFor(t, srv), nil, discoveryTenantPrefill())
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if err := in.validate(); err != nil {
		t.Fatalf("in.validate() error = %v, want nil (transcript: %s)", err, transcript.String())
	}
	if in.trackerOrg != "contoso" {
		t.Errorf("trackerOrg = %q, want the discovered org %q", in.trackerOrg, "contoso")
	}
	if in.trackerProject != "mandat-pilot" {
		t.Errorf("trackerProject = %q, want the discovered project %q", in.trackerProject, "mandat-pilot")
	}
	if in.repoURL != "https://dev.azure.com/contoso/mandat-pilot/_git/mandat" {
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
	if cfg.Tracker.Org != "contoso" || cfg.Tracker.Project != "mandat-pilot" {
		t.Errorf("Tracker = %+v, want the confirmed discovered org/project", cfg.Tracker)
	}
	repo, ok := cfg.Repos["mandat"]
	if !ok || repo.URL != "https://dev.azure.com/contoso/mandat-pilot/_git/mandat" {
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
			{"accountId":"aaaaaaaa-0000-4000-8000-000000000001","accountName":"contoso"},
			{"accountId":"bbbbbbbb-0000-4000-8000-000000000002","accountName":"other-org"}
		]}`,
		"", "",
	)

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, fakeADOTokenSource(testFakeToken), discovererFor(t, srv), nil, discoveryTenantPrefill())
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if !strings.Contains(transcript.String(), "note:") || !strings.Contains(transcript.String(), "more than one organization") {
		t.Errorf("transcript = %q, want a one-line note naming the ambiguous-org failure", transcript.String())
	}
	if in.trackerOrg != "contoso" {
		t.Errorf("trackerOrg = %q, want the manually typed %q", in.trackerOrg, "contoso")
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
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, failingTokenSource, unreachableDiscoverer(t), nil, discoveryTenantPrefill())
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if !strings.Contains(transcript.String(), "note:") {
		t.Errorf("transcript = %q, want a one-line note about the token source failure", transcript.String())
	}
	if in.trackerOrg != "contoso" || in.trackerProject != "mandat-dogfood" {
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
		"dev-agent@contoso.onmicrosoft.com",
		"reviewer-agent@contoso.onmicrosoft.com",
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
	if !strings.Contains(out, "mandat serve") || !strings.Contains(out, "dev-agent@contoso.onmicrosoft.com") {
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
		trackerOrg:         "contoso",
		trackerProject:     "mandat-dogfood",
		authMode:           string(config.AuthArcManagedIdentity),
		entraTenant:        "11111111-1111-1111-1111-111111111111",
		entraBlueprint:     "blueprint-01",
		repoRaw:            "mandat=https://dev.azure.com/contoso/mandat-dogfood/_git/mandat",
		repoKey:            "mandat",
		repoURL:            "https://dev.azure.com/contoso/mandat-dogfood/_git/mandat",
		baseBranch:         "main",
		remitPaths:         []string{"internal/", "cmd/"},
		gates:              []string{"make check", "npx govkit check"},
		devIdentityID:      "agent-identity-dev-01",
		devUserID:          "agent-user-dev-01",
		devUserUPN:         "dev-agent@contoso.onmicrosoft.com",
		reviewerIdentityID: "agent-identity-reviewer-01",
		reviewerUserID:     "agent-user-reviewer-01",
		reviewerUserUPN:    "reviewer-agent@contoso.onmicrosoft.com",
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

	old := "tracker:\n  org: contoso\n  project: mandat\n"
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
	if want := []string{"  org: contoso"}; !slices.Equal(removed, want) {
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

// TestValidateADOBeforeWrite_TokenFailsValidation_Refuses proves US-0013
// AC-13.1's refuse clause: an obtainable token that cannot reach the org fails
// the pre-write probe, so init must not proceed to write config.yaml — the gate
// returns false and names both the org whose validation failed and that the
// config was not written.
func TestValidateADOBeforeWrite_TokenFailsValidation_Refuses(t *testing.T) {
	t.Parallel()

	failValidate := func(context.Context, string, string) error {
		return &discovery.APIError{Status: http.StatusForbidden, Body: "no access"}
	}

	var out strings.Builder
	if validateADOBeforeWrite(context.Background(), fakeADOTokenSource(testFakeToken), failValidate, "11111111-1111-1111-1111-111111111111", "contoso", &out) {
		t.Fatalf("validateADOBeforeWrite() = true, want false (a token that fails validation must refuse the write) (out: %s)", out.String())
	}
	if !strings.Contains(out.String(), "contoso") {
		t.Errorf("out = %q, want it to name the org whose validation failed", out.String())
	}
	if !strings.Contains(out.String(), "NOT written") {
		t.Errorf("out = %q, want it to state config.yaml was not written", out.String())
	}
}

// TestValidateADOBeforeWrite_TokenReachesOrg_Proceeds proves the write path: an
// obtainable token that reaches the org passes the probe, so init proceeds.
func TestValidateADOBeforeWrite_TokenReachesOrg_Proceeds(t *testing.T) {
	t.Parallel()

	okValidate := func(context.Context, string, string) error { return nil }

	var out strings.Builder
	if !validateADOBeforeWrite(context.Background(), fakeADOTokenSource(testFakeToken), okValidate, "11111111-1111-1111-1111-111111111111", "contoso", &out) {
		t.Fatalf("validateADOBeforeWrite() = false, want true (a reachable token proceeds) (out: %s)", out.String())
	}
}

// TestValidateADOBeforeWrite_NoToken_ProceedsUnvalidated proves the manual path:
// with no az-derived token obtainable, init cannot validate but still lets an
// operator configure — the gate proceeds and notes the write is unvalidated
// (the refuse precondition is a token that EXISTS but fails, not a missing one),
// and the validator is never consulted without a token in hand.
func TestValidateADOBeforeWrite_NoToken_ProceedsUnvalidated(t *testing.T) {
	t.Parallel()

	unreachableValidate := func(context.Context, string, string) error {
		t.Fatal("validate called despite a failed token source")
		return nil
	}

	var out strings.Builder
	if !validateADOBeforeWrite(context.Background(), failingTokenSource, unreachableValidate, "11111111-1111-1111-1111-111111111111", "contoso", &out) {
		t.Fatalf("validateADOBeforeWrite() = false, want true (no token still lets the operator configure) (out: %s)", out.String())
	}
	if !strings.Contains(out.String(), "unvalidated") {
		t.Errorf("out = %q, want the unvalidated note", out.String())
	}
}

// TestInitCmd_NonInteractive_FlagBeatsEnv proves AC-13.10's flags > env
// precedence: with MANDAT_TRACKER_ORG set AND --tracker-org supplied, the flag
// value is what lands in config.yaml. Not parallel: t.Setenv forbids it.
func TestInitCmd_NonInteractive_FlagBeatsEnv(t *testing.T) {
	stubPassPreflight(t)
	t.Setenv("MANDAT_TRACKER_ORG", "envorg")

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var stdout, stderr strings.Builder
	code := initCmd(validInitArgs(configPath), strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}
	if cfg.Tracker.Org != "contoso" {
		t.Errorf("Tracker.Org = %q, want the flag value %q (flags > env, not the env %q)", cfg.Tracker.Org, "contoso", "envorg")
	}
}

// TestInitCmd_NonInteractive_EnvFillsOmittedFlag proves AC-13.10's env
// fallback: with --tracker-org omitted but MANDAT_TRACKER_ORG set, the env
// value fills the unset flag and lands in config.yaml. Not parallel: t.Setenv.
func TestInitCmd_NonInteractive_EnvFillsOmittedFlag(t *testing.T) {
	stubPassPreflight(t)
	t.Setenv("MANDAT_TRACKER_ORG", "envorg")

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	args := removeFlag(validInitArgs(configPath), "--tracker-org")
	var stdout, stderr strings.Builder
	code := initCmd(args, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load(%q) error = %v, want nil", configPath, err)
	}
	if cfg.Tracker.Org != "envorg" {
		t.Errorf("Tracker.Org = %q, want the env value %q filling the omitted --tracker-org", cfg.Tracker.Org, "envorg")
	}
}

// TestMergeEnvOverPrior_EnvBeatsExistingConfig proves AC-13.10's env > existing
// config on an interactive re-run: a set MANDAT_* var overrides the on-disk
// value in the reconstructed prior, while a field with no env var keeps what
// the config held (so an untouched field stays byte-identical, AC-13.11). Not
// parallel: t.Setenv.
func TestMergeEnvOverPrior_EnvBeatsExistingConfig(t *testing.T) {
	disk := validNonInteractiveInput()
	disk.trackerOrg = "diskorg"
	a := disk.render()

	prior, ok := reconstructPriorInput([]byte(a))
	if !ok {
		t.Fatal("reconstructPriorInput() ok = false, want true for an init-written config")
	}

	t.Setenv("MANDAT_TRACKER_ORG", "envorg")
	merged := mergeEnvOverPrior(&prior, envInput())
	if merged == nil {
		t.Fatal("mergeEnvOverPrior() = nil, want a merged prior")
	}
	if merged.trackerOrg != "envorg" {
		t.Errorf("merged.trackerOrg = %q, want the env value %q beating the on-disk %q", merged.trackerOrg, "envorg", "diskorg")
	}
	if merged.trackerProject != disk.trackerProject {
		t.Errorf("merged.trackerProject = %q, want the on-disk %q (no env var set, prior kept)", merged.trackerProject, disk.trackerProject)
	}
}

// TestEnvInput_ParsesListsNumbersAndRepo proves envInput's non-trivial parses
// (AC-13.10): comma lists trim and drop empties, the repo key=url form splits,
// and the numerics parse to their typed values. Not parallel: t.Setenv.
func TestEnvInput_ParsesListsNumbersAndRepo(t *testing.T) {
	t.Setenv("MANDAT_REMIT_PATHS", "a, b ,c")
	t.Setenv("MANDAT_GATES", "make check, npx govkit check")
	t.Setenv("MANDAT_REPO", "mandat=https://example.test/_git/mandat")
	t.Setenv("MANDAT_MAX_USD_PER_RUN", "7.5")
	t.Setenv("MANDAT_POOL_SIZE", "4")

	env := envInput()
	if got, want := env.remitPaths, []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("remitPaths = %v, want %v (comma-split, trimmed, empties dropped)", got, want)
	}
	if got, want := env.gates, []string{"make check", "npx govkit check"}; !slices.Equal(got, want) {
		t.Errorf("gates = %v, want %v", got, want)
	}
	if env.repoKey != "mandat" || env.repoURL != "https://example.test/_git/mandat" {
		t.Errorf("repo = %q / %q, want the key=url split of MANDAT_REPO", env.repoKey, env.repoURL)
	}
	if env.maxUSDPerRun != 7.5 {
		t.Errorf("maxUSDPerRun = %v, want 7.5", env.maxUSDPerRun)
	}
	if env.poolSize != 4 {
		t.Errorf("poolSize = %d, want 4", env.poolSize)
	}
}

// TestEnvInput_BadNumericSkipped proves AC-13.10's bad-numeric handling: an
// unparseable MANDAT_MAX_USD_PER_RUN or MANDAT_POOL_SIZE is skipped (the field
// stays at its zero value), never fatal. Not parallel: t.Setenv.
func TestEnvInput_BadNumericSkipped(t *testing.T) {
	t.Setenv("MANDAT_MAX_USD_PER_RUN", "not-a-float")
	t.Setenv("MANDAT_POOL_SIZE", "not-an-int")

	env := envInput()
	if env.maxUSDPerRun != 0 {
		t.Errorf("maxUSDPerRun = %v, want 0 (a bad float is skipped, not fatal)", env.maxUSDPerRun)
	}
	if env.poolSize != 0 {
		t.Errorf("poolSize = %d, want 0 (a bad int is skipped, not fatal)", env.poolSize)
	}
}

// swapListTenants installs fn as init's tenant-enumeration seam for the test's
// duration, restoring the production one on cleanup. A test that swaps it runs
// non-parallel: listTenants is package state and -race rejects a concurrent
// write.
func swapListTenants(t *testing.T, fn func(context.Context) ([]tenantOption, error)) {
	t.Helper()
	saved := listTenants
	listTenants = fn
	t.Cleanup(func() { listTenants = saved })
}

// swapDiscoverEntra installs fn as init's Entra-registry seam for the test's
// duration, restoring the production one on cleanup. Non-parallel for the same
// reason as swapListTenants.
func swapDiscoverEntra(t *testing.T, fn func(context.Context, string) (entra.Registry, error)) {
	t.Helper()
	saved := discoverEntra
	discoverEntra = fn
	t.Cleanup(func() { discoverEntra = saved })
}

// writeFakeAz writes a fake az onto a fresh dir prepended to PATH: it records
// its argument list to argsFile and prints a token, so a test drives the real
// production token source with no live az and inspects exactly what was invoked
// (US-0015 AC-15.4). The caller runs non-parallel: it mutates PATH via t.Setenv.
func writeFakeAz(t *testing.T) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	script := "#!/bin/sh\nprintf '%s' \"$*\" > '" + argsFile + "'\nprintf 'fake-token\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "az"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake az: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

// TestAzCLITokenSource_PinsSubscriptionFlag is US-0015 AC-15.4's contract test:
// with a fake az on PATH, the production token source's argument list carries
// --subscription <az account id> — the pin that works without a re-login, unlike
// --tenant. Dropping --subscription from azCLITokenSource reproduces a failing
// test, not a silent pass. Not parallel: mutates PATH.
func TestAzCLITokenSource_PinsSubscriptionFlag(t *testing.T) {
	argsFile := writeFakeAz(t)

	token, err := azCLITokenSource(context.Background(), "my-account-id")
	if err != nil {
		t.Fatalf("azCLITokenSource() error = %v, want nil", err)
	}
	if token != "fake-token" {
		t.Errorf("token = %q, want the fake az output", token)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if !strings.Contains(string(args), "--subscription my-account-id") {
		t.Errorf("az args = %q, want them to carry --subscription my-account-id (US-0015 AC-15.4)", string(args))
	}
	if strings.Contains(string(args), "--tenant") {
		t.Errorf("az args = %q, want no --tenant (it forces a re-login; --subscription pins instead)", string(args))
	}
	if !strings.Contains(string(args), "--resource "+adoResourceID) {
		t.Errorf("az args = %q, want the pinned ADO --resource still present", string(args))
	}
}

// TestAzCLITokenSource_EmptyAccount_OmitsFlag proves the guard: with no account,
// azCLITokenSource omits --subscription rather than passing --subscription "" (the
// interview skips discovery when no tenant resolves, AC-15.5, and an --entra-tenant
// override probes the active account). Not parallel: mutates PATH.
func TestAzCLITokenSource_EmptyAccount_OmitsFlag(t *testing.T) {
	argsFile := writeFakeAz(t)

	if _, err := azCLITokenSource(context.Background(), ""); err != nil {
		t.Fatalf("azCLITokenSource() error = %v, want nil", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	if strings.Contains(string(args), "--subscription") {
		t.Errorf("az args = %q, want no --subscription flag for an empty account", string(args))
	}
}

// TestRoleIdentitiesFromRegistry_MatchesAndPairs proves the identity prefill's
// core: each role picks the agent identity whose displayName carries the role
// name (case-insensitive) and pairs it to its agent user, so the interview
// offers the six fields as confirmed defaults.
func TestRoleIdentitiesFromRegistry_MatchesAndPairs(t *testing.T) {
	t.Parallel()

	reg := entra.Registry{
		Identities: []entra.AgentIdentity{
			{ID: "id-dev", DisplayName: "mandat-DEV-agent"},
			{ID: "id-rev", DisplayName: "mandat reviewer agent"},
			{ID: "id-other", DisplayName: "unrelated-principal"},
		},
		Users: []entra.AgentUser{
			{ID: "u-dev", UserPrincipalName: "dev@contoso.onmicrosoft.com", IdentityParentID: "id-dev"},
			{ID: "u-rev", UserPrincipalName: "rev@contoso.onmicrosoft.com", IdentityParentID: "id-rev"},
		},
	}

	got := roleIdentitiesFromRegistry(reg, "dev", "reviewer")
	dev := got["dev"]
	if dev.identityID != "id-dev" || dev.userID != "u-dev" || dev.userName != "dev@contoso.onmicrosoft.com" {
		t.Errorf("dev prefill = %+v, want id-dev paired to u-dev/dev@...", dev)
	}
	rev := got["reviewer"]
	if rev.identityID != "id-rev" || rev.userID != "u-rev" || rev.userName != "rev@contoso.onmicrosoft.com" {
		t.Errorf("reviewer prefill = %+v, want id-rev paired to u-rev/rev@...", rev)
	}
}

// TestRoleIdentitiesFromRegistry_NoMatchOmitsRole proves the fallback: a role
// with no matching identity is omitted from the map, so the interview prompts
// for it rather than prefilling a wrong identity.
func TestRoleIdentitiesFromRegistry_NoMatchOmitsRole(t *testing.T) {
	t.Parallel()

	reg := entra.Registry{
		Identities: []entra.AgentIdentity{{ID: "id-dev", DisplayName: "mandat dev agent"}},
		Users:      []entra.AgentUser{{ID: "u-dev", UserPrincipalName: "dev@contoso", IdentityParentID: "id-dev"}},
	}

	got := roleIdentitiesFromRegistry(reg, "dev", "reviewer")
	if _, ok := got["reviewer"]; ok {
		t.Error("reviewer prefilled despite no matching identity, want it omitted (prompt fallback)")
	}
	if _, ok := got["dev"]; !ok {
		t.Error("dev omitted despite a matching identity, want it prefilled")
	}
}

// TestRoleIdentitiesFromRegistry_MatchWithoutPairedUser proves the partial-fill
// case: a matched identity with no paired user prefills the identity id but
// leaves the user fields empty, so the interview prefills one and prompts for
// the others.
func TestRoleIdentitiesFromRegistry_MatchWithoutPairedUser(t *testing.T) {
	t.Parallel()

	reg := entra.Registry{Identities: []entra.AgentIdentity{{ID: "id-dev", DisplayName: "dev"}}}
	dev := roleIdentitiesFromRegistry(reg, "dev")["dev"]
	if dev.identityID != "id-dev" {
		t.Errorf("dev.identityID = %q, want id-dev (prefilled even with no paired user)", dev.identityID)
	}
	if dev.userID != "" || dev.userName != "" {
		t.Errorf("dev user fields = %q/%q, want empty (no paired user → prompt for them)", dev.userID, dev.userName)
	}
}

// TestResolvePrefill_FreshInstall_SingleTenantAutoPicksAndReadsRegistry proves
// the resolve step init runs before the interview: a lone enumerated tenant
// auto-picks (no prompt, like a single discovered org), config gets its TenantID,
// and the registry read is pinned to its AccountID. Not parallel: it swaps
// package-var seams.
func TestResolvePrefill_FreshInstall_SingleTenantAutoPicksAndReadsRegistry(t *testing.T) {
	swapListTenants(t, func(context.Context) ([]tenantOption, error) {
		return []tenantOption{{TenantID: "solo-tenant", AccountID: "solo-account", Name: "Contoso"}}, nil
	})
	var gotAccount string
	swapDiscoverEntra(t, func(_ context.Context, accountID string) (entra.Registry, error) {
		gotAccount = accountID
		return entra.Registry{
			Identities: []entra.AgentIdentity{{ID: "id-dev", DisplayName: "dev-agent"}},
			Users:      []entra.AgentUser{{ID: "u-dev", UserPrincipalName: "dev@contoso", IdentityParentID: "id-dev"}},
		}, nil
	})

	var out strings.Builder
	pf := resolvePrefill(context.Background(), bufio.NewReader(strings.NewReader("")), &out, nil, "")
	if pf.tenant != "solo-tenant" {
		t.Errorf("prefill.tenant = %q, want the sole tenant's id (goes to config.entra.tenant)", pf.tenant)
	}
	if pf.accountID != "solo-account" {
		t.Errorf("prefill.accountID = %q, want the sole tenant's az account id (the mint pin)", pf.accountID)
	}
	if gotAccount != "solo-account" {
		t.Errorf("registry read pinned to account %q, want the chosen %q", gotAccount, "solo-account")
	}
	if pf.roleIdentities["dev"].identityID != "id-dev" {
		t.Errorf("prefill dev identity = %+v, want id-dev from the registry", pf.roleIdentities["dev"])
	}
	// A single tenant auto-picks like a single discovered org: no picker prompt.
	if strings.Contains(out.String(), "Multiple Entra tenants") {
		t.Errorf("out = %q, want no picker prompt for a single tenant", out.String())
	}
}

// TestResolvePrefill_MultipleTenants_ChosenValuePinsDiscovery proves the picker:
// with several tenants the operator's selection yields the TenantID for config and
// the AccountID that pins the registry read (and, downstream, the
// discovery/validation tokens). Not parallel: swaps package-var seams.
func TestResolvePrefill_MultipleTenants_ChosenValuePinsDiscovery(t *testing.T) {
	swapListTenants(t, func(context.Context) ([]tenantOption, error) {
		return []tenantOption{
			{TenantID: "tenant-a", AccountID: "account-a", Name: "Contoso"},
			{TenantID: "tenant-b", AccountID: "account-b", Name: "Fabrikam"},
		}, nil
	})
	var gotAccount string
	swapDiscoverEntra(t, func(_ context.Context, accountID string) (entra.Registry, error) {
		gotAccount = accountID
		return entra.Registry{}, nil
	})

	// The operator selects row 2 (tenant-b / account-b).
	var out strings.Builder
	pf := resolvePrefill(context.Background(), bufio.NewReader(strings.NewReader("2\n")), &out, nil, "")
	if pf.tenant != "tenant-b" {
		t.Errorf("prefill.tenant = %q, want the chosen row-2 tenant-b", pf.tenant)
	}
	if pf.accountID != "account-b" {
		t.Errorf("prefill.accountID = %q, want the chosen row-2 account-b", pf.accountID)
	}
	if gotAccount != "account-b" {
		t.Errorf("registry read pinned to %q, want the chosen account %q", gotAccount, "account-b")
	}
	if !strings.Contains(out.String(), "Fabrikam") {
		t.Errorf("out = %q, want the picker to list the tenant names", out.String())
	}
}

// TestResolvePrefill_TenantOverride_SkipsPicker proves US-0015 AC-15.3: an
// explicit --entra-tenant/MANDAT_ENTRA_TENANT value becomes prefill.tenant and
// skips the picker entirely (listTenants is never called); it carries no az
// account, so prefill.accountID is empty and the mint falls back to the active
// account. Not parallel: swaps package-var seams.
func TestResolvePrefill_TenantOverride_SkipsPicker(t *testing.T) {
	swapListTenants(t, func(context.Context) ([]tenantOption, error) {
		t.Fatal("listTenants called with an explicit tenant override, want the picker skipped")
		return nil, nil
	})
	var gotAccount string
	swapDiscoverEntra(t, func(_ context.Context, accountID string) (entra.Registry, error) {
		gotAccount = accountID
		return entra.Registry{}, nil
	})

	pf := resolvePrefill(context.Background(), bufio.NewReader(strings.NewReader("")), new(strings.Builder), nil, "override-tenant")
	if pf.tenant != "override-tenant" {
		t.Errorf("prefill.tenant = %q, want the override", pf.tenant)
	}
	if pf.accountID != "" {
		t.Errorf("prefill.accountID = %q, want empty (an override carries no az account) (AC-15.3)", pf.accountID)
	}
	if gotAccount != "" {
		t.Errorf("registry read pinned to account %q, want empty (fall back to the active account)", gotAccount)
	}
}

// TestResolvePrefill_EnumerationFails_LeavesEmptyAndSkipsRegistry proves the
// never-fatal fallback: an enumeration failure leaves prefill.tenant empty
// (US-0015 AC-15.5), and with no tenant the registry read is skipped rather than
// minting an unpinned token, so the interview prompts every field. Not parallel:
// swaps package-var seams.
func TestResolvePrefill_EnumerationFails_LeavesEmptyAndSkipsRegistry(t *testing.T) {
	swapListTenants(t, func(context.Context) ([]tenantOption, error) {
		return nil, errors.New("az missing")
	})
	swapDiscoverEntra(t, func(context.Context, string) (entra.Registry, error) {
		t.Fatal("discoverEntra called with no resolved tenant, want the registry read skipped")
		return entra.Registry{}, nil
	})

	var out strings.Builder
	pf := resolvePrefill(context.Background(), bufio.NewReader(strings.NewReader("")), &out, nil, "")
	if pf.tenant != "" {
		t.Errorf("prefill.tenant = %q, want empty when enumeration fails", pf.tenant)
	}
	if len(pf.roleIdentities) != 0 {
		t.Errorf("prefill.roleIdentities = %+v, want empty (registry read skipped with no tenant)", pf.roleIdentities)
	}
	if !strings.Contains(out.String(), "note:") {
		t.Errorf("out = %q, want a one-line note about the enumeration failure", out.String())
	}
}

// TestResolvePrefill_Rerun_SkipsEnumeration proves a re-run (any non-nil prior)
// consults neither seam: the stored config, not a fresh probe, is the source of
// truth. Not parallel: swaps package-var seams.
func TestResolvePrefill_Rerun_SkipsEnumeration(t *testing.T) {
	swapListTenants(t, func(context.Context) ([]tenantOption, error) {
		t.Fatal("listTenants called on a re-run, want the stored config used")
		return nil, nil
	})
	swapDiscoverEntra(t, func(context.Context, string) (entra.Registry, error) {
		t.Fatal("discoverEntra called on a re-run, want the stored config used")
		return entra.Registry{}, nil
	})

	prior := validNonInteractiveInput()
	pf := resolvePrefill(context.Background(), bufio.NewReader(strings.NewReader("")), new(strings.Builder), &prior, "")
	if pf.tenant != "" || len(pf.roleIdentities) != 0 {
		t.Errorf("re-run prefill = %+v, want empty (interview reads the stored config)", pf)
	}
}

// TestPickTenant_SelectsByNumberIdOrReprompts proves the picker's confirm-or-
// override shape: Enter takes the first tenant, a row number selects that tenant
// (both carrying its az account id), a typed id that names a LISTED tenant selects
// that tenant WITH its account id (an operator naturally types the id shown in the
// row — it must not be mistaken for an accountless override), a typed id on NO
// listed tenant is an accountless override (empty account id → active account),
// and an out-of-range number re-prompts rather than pinning a wrong tenant.
func TestPickTenant_SelectsByNumberIdOrReprompts(t *testing.T) {
	t.Parallel()

	tenants := []tenantOption{
		{TenantID: "tenant-a", AccountID: "account-a", Name: "A"},
		{TenantID: "tenant-b", AccountID: "account-b", Name: "B"},
	}
	for _, tc := range []struct{ name, script, wantTenant, wantAccountID string }{
		{"enter takes first", "\n", "tenant-a", "account-a"},
		{"row number selects", "2\n", "tenant-b", "account-b"},
		// The load-bearing case: typing a listed tenant's id keeps its account id
		// (the mint pin), not "" — reverting the match reproduces the live 401.
		{"typed listed tenant id keeps its account", "tenant-b\n", "tenant-b", "account-b"},
		{"typed listed account id keeps its account", "account-b\n", "tenant-b", "account-b"},
		{"typed unlisted id is an accountless override", "tenant-z\n", "tenant-z", ""},
		{"out-of-range reprompts", "9\n1\n", "tenant-a", "account-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			iv := &interviewer{r: bufio.NewReader(strings.NewReader(tc.script)), w: new(strings.Builder)}
			got := iv.pickTenant(tenants)
			if got.TenantID != tc.wantTenant || got.AccountID != tc.wantAccountID {
				t.Errorf("pickTenant(%q) = %+v, want TenantID %q / AccountID %q", tc.script, got, tc.wantTenant, tc.wantAccountID)
			}
		})
	}
}

// TestRunInteractiveInterview_PrefillsTenantAndRoleIdentities proves the visible
// win: a fresh install seeded with a discovered prefill offers the tenant and
// the six role-identity fields as bracketed defaults, so a blank Enter through
// them confirms the derived values instead of retyping GUIDs.
func TestRunInteractiveInterview_PrefillsTenantAndRoleIdentities(t *testing.T) {
	t.Parallel()

	prefill := discoveredPrefill{
		tenant: "derived-tenant-guid",
		roleIdentities: map[string]roleIdentityPrefill{
			"dev":      {identityID: "reg-dev-id", userID: "reg-dev-user", userName: "reg-dev@contoso.onmicrosoft.com"},
			"reviewer": {identityID: "reg-rev-id", userID: "reg-rev-user", userName: "reg-rev@contoso.onmicrosoft.com"},
		},
	}

	lines := validInteractiveScriptLines()
	lines[4] = "" // entra.tenant: accept the derived default
	for i := 15; i <= 20; i++ {
		lines[i] = "" // accept each prefilled role-identity default
	}

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), &transcript, failingTokenSource, unreachableDiscoverer(t), nil, prefill)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if in.entraTenant != "derived-tenant-guid" {
		t.Errorf("entraTenant = %q, want the derived default kept by a blank Enter", in.entraTenant)
	}
	if in.devIdentityID != "reg-dev-id" || in.devUserID != "reg-dev-user" || in.devUserUPN != "reg-dev@contoso.onmicrosoft.com" {
		t.Errorf("dev identity = %q/%q/%q, want the prefilled registry values", in.devIdentityID, in.devUserID, in.devUserUPN)
	}
	if in.reviewerIdentityID != "reg-rev-id" || in.reviewerUserID != "reg-rev-user" || in.reviewerUserUPN != "reg-rev@contoso.onmicrosoft.com" {
		t.Errorf("reviewer identity = %q/%q/%q, want the prefilled registry values", in.reviewerIdentityID, in.reviewerUserID, in.reviewerUserUPN)
	}
	if !strings.Contains(transcript.String(), "[derived-tenant-guid]") {
		t.Errorf("transcript = %q, want the entra.tenant prompt to show the derived tenant in brackets", transcript.String())
	}
	if !strings.Contains(transcript.String(), "[reg-dev-id]") {
		t.Errorf("transcript = %q, want the dev identity prompt to show the prefilled id in brackets", transcript.String())
	}
}

// TestRunInteractiveInterview_MissingRoleInRegistry_PromptsForIt proves the
// per-role fallback: with only dev in the registry, dev is prefilled and
// confirmed by a blank Enter while reviewer has no default and is typed from the
// script, exactly as a no-discovery run would prompt it.
func TestRunInteractiveInterview_MissingRoleInRegistry_PromptsForIt(t *testing.T) {
	t.Parallel()

	prefill := discoveredPrefill{
		tenant: "derived-tenant-guid",
		roleIdentities: map[string]roleIdentityPrefill{
			"dev": {identityID: "reg-dev-id", userID: "reg-dev-user", userName: "reg-dev@contoso.onmicrosoft.com"},
		},
	}

	lines := validInteractiveScriptLines()
	lines[15] = "" // roles.dev.agent_identity_id: accept the prefilled default
	lines[16] = "" // roles.dev.agent_user_id
	lines[17] = "" // roles.dev.agent_user_name
	// 18–20 (reviewer) keep their script values: no prefill, so those prompt.

	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), new(strings.Builder), failingTokenSource, unreachableDiscoverer(t), nil, prefill)
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v", err)
	}
	if in.devIdentityID != "reg-dev-id" {
		t.Errorf("devIdentityID = %q, want the prefilled registry value", in.devIdentityID)
	}
	if in.reviewerIdentityID != "agent-identity-reviewer-01" {
		t.Errorf("reviewerIdentityID = %q, want the script-typed value (no registry match → prompt fallback)", in.reviewerIdentityID)
	}
}

// TestRunInteractiveInterview_DiscoveryPinsAccountToTokenSource proves US-0015
// AC-15.1 at the discovery call site: the token minted for discovery is pinned to
// the chosen az account (--subscription), not the tenant. A capturing token
// source records the account id it was asked to mint for; it then errors so the
// interview falls back to the script.
func TestRunInteractiveInterview_DiscoveryPinsAccountToTokenSource(t *testing.T) {
	t.Parallel()

	var gotAccount string
	capturing := func(_ context.Context, accountID string) (string, error) {
		gotAccount = accountID
		return "", errors.New("stop after capturing the account id")
	}

	_, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), new(strings.Builder), capturing, unreachableDiscoverer(t), nil, discoveredPrefill{tenant: "pinned-tenant", accountID: "pinned-account"})
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v", err)
	}
	if gotAccount != "pinned-account" {
		t.Errorf("discovery token minted for account %q, want the chosen %q (US-0015 AC-15.1)", gotAccount, "pinned-account")
	}
}

// TestRunInteractiveInterview_NoTenant_SkipsDiscovery proves US-0015 AC-15.5: a
// fresh install with no resolved tenant never mints a token — the token source
// is untouched — and prints the skip note, then prompts every field from the
// script.
func TestRunInteractiveInterview_NoTenant_SkipsDiscovery(t *testing.T) {
	t.Parallel()

	called := false
	tokenSrc := func(context.Context, string) (string, error) {
		called = true
		return "", errors.New("token source must not be called with no tenant")
	}

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(validInteractiveScriptLines()), &transcript, tokenSrc, unreachableDiscoverer(t), nil, discoveredPrefill{})
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if called {
		t.Error("token source called with no resolved tenant, want discovery skipped (US-0015 AC-15.5: no unpinned mint)")
	}
	if !strings.Contains(transcript.String(), "skipping Azure DevOps discovery") {
		t.Errorf("transcript = %q, want the skip note", transcript.String())
	}
	if in.trackerOrg != "contoso" {
		t.Errorf("trackerOrg = %q, want the manually typed value from the script", in.trackerOrg)
	}
}

// TestValidateADOBeforeWrite_PinsAccountToTokenSource proves US-0015 AC-15.1 at
// the pre-write validation call site: the probe token is minted pinned to the
// chosen az account (--subscription), not the tenant.
func TestValidateADOBeforeWrite_PinsAccountToTokenSource(t *testing.T) {
	t.Parallel()

	var gotAccount string
	capturing := func(_ context.Context, accountID string) (string, error) {
		gotAccount = accountID
		return "tok", nil
	}
	okValidate := func(context.Context, string, string) error { return nil }

	var out strings.Builder
	if !validateADOBeforeWrite(context.Background(), capturing, okValidate, "my-account", "contoso", &out) {
		t.Fatalf("validateADOBeforeWrite() = false, want true (out: %s)", out.String())
	}
	if gotAccount != "my-account" {
		t.Errorf("validation token minted for account %q, want the pinned %q (US-0015 AC-15.1)", gotAccount, "my-account")
	}
}

// TestRunInteractiveInterview_DiscoveryPrefillsBaseBranch proves derivation 2: a
// successful discovery offers the selected repo's default branch (refs/heads/
// stripped) as the base_branch prompt default, so a blank Enter confirms it.
func TestRunInteractiveInterview_DiscoveryPrefillsBaseBranch(t *testing.T) {
	t.Parallel()

	srv := fakeADOServer(t,
		`{"id":"11111111-0000-4000-8000-000000000001"}`,
		`{"count":1,"value":[{"accountId":"22222222-0000-4000-8000-000000000002","accountName":"contoso"}]}`,
		`{"count":1,"value":[{"id":"33333333-0000-4000-8000-000000000003","name":"mandat-pilot"}]}`,
		`{"count":1,"value":[{"id":"44444444-0000-4000-8000-000000000004","name":"mandat","remoteUrl":"https://dev.azure.com/contoso/mandat-pilot/_git/mandat","defaultBranch":"refs/heads/develop"}]}`,
	)

	lines := validInteractiveScriptLines()
	lines[0] = "" // tracker.org: accept discovered
	lines[1] = "" // tracker.project: accept discovered
	lines[7] = "" // repo url: accept discovered
	lines[8] = "" // base_branch: accept the discovered default branch

	var transcript strings.Builder
	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), &transcript, fakeADOTokenSource(testFakeToken), discovererFor(t, srv), nil, discoveryTenantPrefill())
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v (transcript: %s)", err, transcript.String())
	}
	if in.baseBranch != "develop" {
		t.Errorf("baseBranch = %q, want the discovered default branch 'develop' (refs/heads/ stripped)", in.baseBranch)
	}
	if !strings.Contains(transcript.String(), "[develop]") {
		t.Errorf("transcript = %q, want the base_branch prompt to show 'develop' in brackets", transcript.String())
	}
}

// TestRunInteractiveInterview_NullDefaultBranch_PromptsForBaseBranch proves the
// derivation-2 fallback: an empty repo reports a null defaultBranch (discovery
// leaves DefaultBranch ""), so base_branch has no bracketed default and is
// required — the value comes from the prompt, not discovery.
func TestRunInteractiveInterview_NullDefaultBranch_PromptsForBaseBranch(t *testing.T) {
	t.Parallel()

	srv := fakeADOServer(t,
		`{"id":"11111111-0000-4000-8000-000000000001"}`,
		`{"count":1,"value":[{"accountId":"22222222-0000-4000-8000-000000000002","accountName":"contoso"}]}`,
		`{"count":1,"value":[{"id":"33333333-0000-4000-8000-000000000003","name":"mandat-pilot"}]}`,
		`{"count":1,"value":[{"id":"44444444-0000-4000-8000-000000000004","name":"mandat","remoteUrl":"https://dev.azure.com/contoso/mandat-pilot/_git/mandat","defaultBranch":null}]}`,
	)

	lines := validInteractiveScriptLines()
	lines[0] = ""        // accept discovered org
	lines[1] = ""        // accept discovered project
	lines[7] = ""        // accept discovered repo url
	lines[8] = "release" // base_branch has no default → typed at the prompt

	in, err := runInteractiveInterview(context.Background(), newInteractiveScript(lines), new(strings.Builder), fakeADOTokenSource(testFakeToken), discovererFor(t, srv), nil, discoveryTenantPrefill())
	if err != nil {
		t.Fatalf("runInteractiveInterview() error = %v", err)
	}
	if in.baseBranch != "release" {
		t.Errorf("baseBranch = %q, want the typed 'release' (null default branch → required prompt)", in.baseBranch)
	}
}
