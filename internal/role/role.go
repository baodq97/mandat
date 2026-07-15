// Package role resolves one RoleAgent from a loaded config.Config: mandate
// reference, playbook and skill set, remit defaults, autonomy ceiling, and
// model tier. A RoleAgent is config plus a playbook, never code (RFC-0001
// §Role config and playbook load, spec §4.4) — this package only reads
// fields internal/config already validated, it never establishes them.
package role

import (
	"fmt"

	"github.com/baodq97/mandat/internal/config"
)

// defaultModelTier is what a role resolves to when its config entry omits
// model_tier (RFC-0001 AC-12: "no per-role override... returns the default
// model tier sonnet").
const defaultModelTier = config.ModelSonnet

// MandateRef names the Entra principal a role's mandate is granted to
// (spec glossary: Mandate = Entra agent identity + sponsor + scoped
// permissions + autonomy ceiling). The sponsor and the permission scope
// live in Entra/tracker group membership, not in mandat's own config, so
// this struct carries only the principal ids config.Load already
// validated are present for the installation's identity_mode.
type MandateRef struct {
	AgentIdentityID string
	AgentUserID     string
}

// Role is one RoleAgent resolved from config: mandate reference, playbook
// and skill set, remit defaults, autonomy ceiling, and model tier. Resolve
// is the only constructor; nothing here can raise the configured autonomy
// ceiling (RFC-0001: "no code path raises a ceiling").
type Role struct {
	Name            string
	Mandate         MandateRef
	Playbook        string
	Skills          []string
	RemitPaths      []string
	AutonomyCeiling config.AutonomyCeiling
	ModelTier       config.ModelTier
}

// UnknownRoleError reports that a role name has no entry in the loaded
// config's role table. A role absent from the table is not enabled; there
// is no separate enabled flag to disagree with table membership.
type UnknownRoleError struct {
	Name string
}

func (e *UnknownRoleError) Error() string {
	return fmt.Sprintf("role: %q is not an enabled role in the loaded config", e.Name)
}

// Resolve returns the Role for name from cfg's role table, applying the
// sonnet model-tier default when the table entry omits model_tier
// (RFC-0001 AC-12). It returns *UnknownRoleError when name has no entry.
func Resolve(cfg *config.Config, name string) (Role, error) {
	rc, ok := cfg.Roles[name]
	if !ok {
		return Role{}, &UnknownRoleError{Name: name}
	}

	tier := rc.ModelTier
	if tier == "" {
		tier = defaultModelTier
	}

	return Role{
		Name: name,
		Mandate: MandateRef{
			AgentIdentityID: rc.AgentIdentityID,
			AgentUserID:     rc.AgentUserID,
		},
		Playbook:        rc.Playbook,
		Skills:          rc.Skills,
		RemitPaths:      rc.RemitPaths,
		AutonomyCeiling: rc.AutonomyCeiling,
		ModelTier:       tier,
	}, nil
}
