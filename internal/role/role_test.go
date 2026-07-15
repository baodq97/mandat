package role

import (
	"errors"
	"reflect"
	"testing"

	"github.com/baodq97/mandat/internal/config"
)

// testConfig is a loaded-config stand-in built directly in Go (Resolve
// takes *config.Config, not a path) so these stay pure-core unit tests of
// resolution, independent of internal/config's own YAML-parsing tests.
func testConfig() *config.Config {
	return &config.Config{
		Entra: config.EntraConfig{IdentityMode: config.IdentityAgentUserPair},
		Roles: map[string]config.RoleConfig{
			"dev": {
				AgentIdentityID: "agent-identity-dev-01",
				AgentUserID:     "agent-user-dev-01",
				AutonomyCeiling: config.CeilingDraftPR,
				ModelTier:       config.ModelOpus,
				Playbook:        "playbooks/dev.md",
				Skills:          []string{"go-testing"},
				RemitPaths:      []string{"internal/"},
			},
			"qa": {
				AgentIdentityID: "agent-identity-qa-01",
				AgentUserID:     "agent-user-qa-01",
				AutonomyCeiling: config.CeilingReport,
				Playbook:        "playbooks/qa.md",
			},
		},
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()

	cfg := testConfig()

	tests := []struct {
		name     string
		roleName string
		want     Role
	}{
		{
			// AC-9.2 (override branch): a per-role model_tier: opus is honored.
			name:     "per-role model tier override honored",
			roleName: "dev",
			want: Role{
				Name:            "dev",
				Mandate:         MandateRef{AgentIdentityID: "agent-identity-dev-01", AgentUserID: "agent-user-dev-01"},
				Playbook:        "playbooks/dev.md",
				Skills:          []string{"go-testing"},
				RemitPaths:      []string{"internal/"},
				AutonomyCeiling: config.CeilingDraftPR,
				ModelTier:       config.ModelOpus,
			},
		},
		{
			// AC-9.2 (default branch) + AC-9.3: no model_tier override
			// resolves to sonnet, and the configured draft-pr-equivalent
			// (here "report") ceiling passes through unchanged.
			name:     "no override defaults to sonnet, ceiling passes through unchanged",
			roleName: "qa",
			want: Role{
				Name:            "qa",
				Mandate:         MandateRef{AgentIdentityID: "agent-identity-qa-01", AgentUserID: "agent-user-qa-01"},
				Playbook:        "playbooks/qa.md",
				AutonomyCeiling: config.CeilingReport,
				ModelTier:       config.ModelSonnet,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Resolve(cfg, tt.roleName)
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v, want nil", tt.roleName, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve(%q) = %+v, want %+v", tt.roleName, got, tt.want)
			}
		})
	}
}

// TestResolve_AC93_CeilingUnchanged pins AC-9.3 directly: the MVP ceiling
// draft-pr configured on a role surfaces unchanged, with no code path
// between config and Role able to raise it.
func TestResolve_AC93_CeilingUnchanged(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	got, err := Resolve(cfg, "dev")
	if err != nil {
		t.Fatalf(`Resolve("dev") error = %v, want nil`, err)
	}
	if got.AutonomyCeiling != config.CeilingDraftPR {
		t.Errorf("Resolve(\"dev\").AutonomyCeiling = %q, want %q", got.AutonomyCeiling, config.CeilingDraftPR)
	}
}

func TestResolve_UnknownRole(t *testing.T) {
	t.Parallel()

	cfg := testConfig()

	_, err := Resolve(cfg, "sa")
	if err == nil {
		t.Fatal(`Resolve("sa") error = nil, want *UnknownRoleError`)
	}

	var unknown *UnknownRoleError
	if !errors.As(err, &unknown) {
		t.Fatalf("Resolve(\"sa\") error type = %T, want *role.UnknownRoleError", err)
	}
	if unknown.Name != "sa" {
		t.Errorf("UnknownRoleError.Name = %q, want %q", unknown.Name, "sa")
	}
}
