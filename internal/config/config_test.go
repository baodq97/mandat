package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeConfig materializes content as config.yaml under a fresh temp dir
// and returns its path, the shape Load(path string) consumes.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

const validYAML = `
tracker:
  kind: azure-devops
  org: baodo0220
  project: mandat-dogfood
auth:
  mode: arc-managed-identity
entra:
  tenant: d1a7b725-aaaa-bbbb-cccc-dddddddddddd
  blueprint: blueprint-01
  identity_mode: agent-user-pair
repos:
  mandat:
    url: https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat
    base_branch: main
    paths:
      - internal/
      - cmd/
    gates:
      - make check
      - npx govkit check
roles:
  dev:
    agent_identity_id: agent-identity-dev-01
    agent_user_id: agent-user-dev-01
    agent_user_name: dev-agent@baodo0220.onmicrosoft.com
    autonomy_ceiling: draft-pr
    model_tier: opus
    playbook: playbooks/dev.md
    skills:
      - go-testing
  qa:
    agent_identity_id: agent-identity-qa-01
    agent_user_id: agent-user-qa-01
    agent_user_name: qa-agent@baodo0220.onmicrosoft.com
    autonomy_ceiling: report
    playbook: playbooks/qa.md
budget:
  max_usd_per_run: 5
notifications:
  teams:
    - https://teams.webhook.example/xxxx
`

func TestLoad_ValidRoundTrips(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, validYAML)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}

	want := &Config{
		Tracker: TrackerConfig{
			Kind:    TrackerAzureDevOps,
			Org:     "baodo0220",
			Project: "mandat-dogfood",
			States:  TrackerStatesConfig{InProgress: defaultInProgressState},
		},
		Auth: AuthConfig{Mode: AuthArcManagedIdentity},
		Entra: EntraConfig{
			Tenant:       "d1a7b725-aaaa-bbbb-cccc-dddddddddddd",
			Blueprint:    "blueprint-01",
			IdentityMode: IdentityAgentUserPair,
		},
		Repos: map[string]RepoConfig{
			"mandat": {
				URL:        "https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat",
				BaseBranch: "main",
				Paths:      []string{"internal/", "cmd/"},
				Gates:      []string{"make check", "npx govkit check"},
			},
		},
		Roles: map[string]RoleConfig{
			"dev": {
				AgentIdentityID: "agent-identity-dev-01",
				AgentUserID:     "agent-user-dev-01",
				AgentUserName:   "dev-agent@baodo0220.onmicrosoft.com",
				AutonomyCeiling: CeilingDraftPR,
				ModelTier:       ModelOpus,
				Playbook:        "playbooks/dev.md",
				Skills:          []string{"go-testing"},
			},
			"qa": {
				AgentIdentityID: "agent-identity-qa-01",
				AgentUserID:     "agent-user-qa-01",
				AgentUserName:   "qa-agent@baodo0220.onmicrosoft.com",
				AutonomyCeiling: CeilingReport,
				Playbook:        "playbooks/qa.md",
			},
		},
		Runner:        RunnerConfig{PoolSize: 1},
		Budget:        BudgetConfig{MaxUSDPerRun: 5},
		Notifications: NotificationConfig{Teams: []string{"https://teams.webhook.example/xxxx"}},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load(%q) =\n%+v\nwant\n%+v", path, got, want)
	}
}

func TestConfig_RemitDefaultsFor(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, validYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q) error = %v, want nil", path, err)
	}

	got, err := cfg.RemitDefaultsFor("mandat")
	if err != nil {
		t.Fatalf(`RemitDefaultsFor("mandat") error = %v, want nil`, err)
	}
	want := RemitDefaults{Repo: "mandat", BaseBranch: "main", Paths: []string{"internal/", "cmd/"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf(`RemitDefaultsFor("mandat") = %+v, want %+v`, got, want)
	}

	if _, err := cfg.RemitDefaultsFor("unregistered"); err == nil {
		t.Error(`RemitDefaultsFor("unregistered") error = nil, want a not-in-registry error`)
	}
}

// TestLoad_TrackerStatesInProgress covers tracker.states.in_progress (US-0018):
// an omitted field resolves to the "Doing" default, and an explicit value
// round-trips unchanged; both cases parse without a validation error.
func TestLoad_TrackerStatesInProgress(t *testing.T) {
	t.Parallel()

	t.Run("defaults to Doing when unset", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, baseYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q) error = %v, want nil", path, err)
		}
		if got := cfg.Tracker.States.InProgress; got != "Doing" {
			t.Errorf("Tracker.States.InProgress = %q, want %q", got, "Doing")
		}
	})

	t.Run("explicit value round-trips", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, strings.Replace(baseYAML,
			"tracker:\n  kind: azure-devops\n  org: baodo0220\n  project: mandat-dogfood\n",
			"tracker:\n  kind: azure-devops\n  org: baodo0220\n  project: mandat-dogfood\n  states:\n    in_progress: In Development\n",
			1))
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q) error = %v, want nil", path, err)
		}
		if got := cfg.Tracker.States.InProgress; got != "In Development" {
			t.Errorf("Tracker.States.InProgress = %q, want %q", got, "In Development")
		}
	})
}

// TestLoad_RunnerPoolSize covers runner.pool_size (US-0012 AC-12.1): an
// omitted or zero field resolves to the 1 default, and an explicit value
// round-trips unchanged.
func TestLoad_RunnerPoolSize(t *testing.T) {
	t.Parallel()

	t.Run("defaults to 1 when unset", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, baseYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q) error = %v, want nil", path, err)
		}
		if got := cfg.Runner.PoolSize; got != 1 {
			t.Errorf("Runner.PoolSize = %d, want 1", got)
		}
	})

	t.Run("explicit value round-trips", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, baseYAML+"runner:\n  pool_size: 4\n")
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q) error = %v, want nil", path, err)
		}
		if got := cfg.Runner.PoolSize; got != 4 {
			t.Errorf("Runner.PoolSize = %d, want 4", got)
		}
	})
}

// TestLoad_BudgetMaxUSDInFlight covers budget.max_usd_in_flight
// (US-0012 AC-12.8): an omitted field resolves to 0 ("derive"), and an
// explicit value at or above max_usd_per_run round-trips unchanged.
func TestLoad_BudgetMaxUSDInFlight(t *testing.T) {
	t.Parallel()

	t.Run("defaults to 0 when unset", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, baseYAML)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q) error = %v, want nil", path, err)
		}
		if got := cfg.Budget.MaxUSDInFlight; got != 0 {
			t.Errorf("Budget.MaxUSDInFlight = %v, want 0", got)
		}
	})

	t.Run("explicit value round-trips", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, strings.Replace(baseYAML,
			"budget:\n  max_usd_per_run: 5\n",
			"budget:\n  max_usd_per_run: 5\n  max_usd_in_flight: 20\n",
			1))
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q) error = %v, want nil", path, err)
		}
		if got := cfg.Budget.MaxUSDInFlight; got != 20 {
			t.Errorf("Budget.MaxUSDInFlight = %v, want 20", got)
		}
	})
}

// baseYAML is a minimal config that passes validation on its own; every
// TestLoad_RejectsInvalid case mutates exactly one field out of it so each
// case isolates one violation.
const baseYAML = `
tracker:
  kind: azure-devops
  org: baodo0220
  project: mandat-dogfood
auth:
  mode: arc-managed-identity
entra:
  tenant: tenant-01
  blueprint: blueprint-01
  identity_mode: agent-user-pair
repos:
  mandat:
    url: https://example.invalid/repo
    base_branch: main
    paths:
      - internal/
roles:
  dev:
    agent_identity_id: agent-identity-dev-01
    agent_user_id: agent-user-dev-01
    agent_user_name: dev-agent@baodo0220.onmicrosoft.com
    autonomy_ceiling: draft-pr
    playbook: playbooks/dev.md
budget:
  max_usd_per_run: 5
`

func TestLoad_RejectsInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{
			name:    "missing tracker.kind",
			mutate:  func(s string) string { return strings.Replace(s, "  kind: azure-devops\n", "", 1) },
			wantSub: "tracker.kind",
		},
		{
			name:    "bad tracker.kind enum",
			mutate:  func(s string) string { return strings.Replace(s, "kind: azure-devops", "kind: github", 1) },
			wantSub: "tracker.kind",
		},
		{
			name:    "missing auth.mode",
			mutate:  func(s string) string { return strings.Replace(s, "  mode: arc-managed-identity\n", "", 1) },
			wantSub: "auth.mode",
		},
		{
			name:    "bad auth.mode enum",
			mutate:  func(s string) string { return strings.Replace(s, "mode: arc-managed-identity", "mode: password", 1) },
			wantSub: "auth.mode",
		},
		{
			name:    "missing entra.identity_mode",
			mutate:  func(s string) string { return strings.Replace(s, "  identity_mode: agent-user-pair\n", "", 1) },
			wantSub: "entra.identity_mode",
		},
		{
			name: "bad entra.identity_mode enum",
			mutate: func(s string) string {
				return strings.Replace(s, "identity_mode: agent-user-pair", "identity_mode: bearer-token", 1)
			},
			wantSub: "entra.identity_mode",
		},
		{
			name: "empty repo registry",
			mutate: func(s string) string {
				i := strings.Index(s, "repos:")
				j := strings.Index(s, "roles:")
				return s[:i] + "repos: {}\n" + s[j:]
			},
			wantSub: "repos",
		},
		{
			name: "absolute repo path",
			mutate: func(s string) string {
				return strings.Replace(s, "      - internal/\n", "      - /internal/\n", 1)
			},
			wantSub: "repos.mandat.paths[0]",
		},
		{
			name: "parent-directory repo path",
			mutate: func(s string) string {
				return strings.Replace(s, "      - internal/\n", "      - ../internal/\n", 1)
			},
			wantSub: "repos.mandat.paths[0]",
		},
		{
			name: "empty role table",
			mutate: func(s string) string {
				i := strings.Index(s, "roles:")
				j := strings.Index(s, "budget:")
				return s[:i] + "roles: {}\n" + s[j:]
			},
			wantSub: "roles",
		},
		{
			name: "bad autonomy_ceiling enum",
			mutate: func(s string) string {
				return strings.Replace(s, "autonomy_ceiling: draft-pr", "autonomy_ceiling: full-auto", 1)
			},
			wantSub: "roles.dev.autonomy_ceiling",
		},
		{
			name: "bad model_tier enum",
			mutate: func(s string) string {
				return strings.Replace(s, "playbook: playbooks/dev.md", "playbook: playbooks/dev.md\n    model_tier: haiku", 1)
			},
			wantSub: "roles.dev.model_tier",
		},
		{
			name:    "missing agent_user_id under agent-user-pair",
			mutate:  func(s string) string { return strings.Replace(s, "    agent_user_id: agent-user-dev-01\n", "", 1) },
			wantSub: "roles.dev.agent_user_id",
		},
		{
			name: "missing agent_user_name under agent-user-pair",
			mutate: func(s string) string {
				return strings.Replace(s, "    agent_user_name: dev-agent@baodo0220.onmicrosoft.com\n", "", 1)
			},
			wantSub: "roles.dev.agent_user_name",
		},
		{
			name:    "non-positive budget",
			mutate:  func(s string) string { return strings.Replace(s, "max_usd_per_run: 5", "max_usd_per_run: 0", 1) },
			wantSub: "budget.max_usd_per_run",
		},
		{
			name: "negative pool_size",
			mutate: func(s string) string {
				return s + "runner:\n  pool_size: -1\n"
			},
			wantSub: "runner.pool_size",
		},
		{
			name: "max_usd_in_flight below max_usd_per_run",
			mutate: func(s string) string {
				return strings.Replace(s, "budget:\n  max_usd_per_run: 5\n", "budget:\n  max_usd_per_run: 5\n  max_usd_in_flight: 4\n", 1)
			},
			wantSub: "budget.max_usd_in_flight",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tt.mutate(baseYAML))
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load(%q) error = nil, want a validation error containing %q", path, tt.wantSub)
			}

			var verrs ValidationErrors
			if !errors.As(err, &verrs) {
				t.Fatalf("Load(%q) error type = %T, want ValidationErrors", path, err)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Load(%q) error = %q, want it to contain %q", path, err.Error(), tt.wantSub)
			}
		})
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("Load() on a missing file: error = nil, want non-nil")
	}
	var verrs ValidationErrors
	if errors.As(err, &verrs) {
		t.Error("Load() on a missing file returned ValidationErrors, want a plain read error")
	}
}
