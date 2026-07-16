// Package config loads and validates /etc/mandat/config.yaml (spec §4.10):
// tracker target, auth and identity mode, the Entra installation scope, the
// repo registry, the role table, the budget ceiling, and notification
// targets. Load is the sole entry point; every exported type below is part
// of the schema it parses into and validates.
package config

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// TrackerKind is the tracker adapter a config targets. Mirrors
// TaskContract.tracker_ref's "system" field naming (RFC-0001 §TaskContract)
// so the two stay in lockstep; only azure-devops ships in the MVP skeleton,
// jira is the roadmap value (CLAUDE.md, spec §10).
type TrackerKind string

// Enum values for TrackerKind.
const (
	TrackerAzureDevOps TrackerKind = "azure-devops"
	TrackerJira        TrackerKind = "jira"
)

// AuthMode is the credential path mandat init records for token acquisition:
// the Arc-managed-identity production path or the client-certificate pilot
// fallback (spec §4.1, §4.10, D5).
type AuthMode string

// Enum values for AuthMode.
const (
	AuthArcManagedIdentity AuthMode = "arc-managed-identity"
	AuthClientCertificate  AuthMode = "client-certificate"
)

// IdentityMode selects which Entra principal shape carries a role's mandate
// (ADR-0005): the portable service-principal fallback, the agent-user-pair
// recommended for Azure DevOps today, or the still-blocked direct
// agent-identity subtype kept only as the retest target.
type IdentityMode string

// Enum values for IdentityMode.
const (
	IdentityServicePrincipal IdentityMode = "service-principal"
	IdentityAgentUserPair    IdentityMode = "agent-user-pair"
	IdentityAgentIdentity    IdentityMode = "agent-identity"
)

// AutonomyCeiling bounds how far a role may act without a human in the loop
// (spec §4.4). Config is the only place that can raise it — no runtime
// control does (spec §4.8) — so this type has no setter beyond YAML
// unmarshaling.
type AutonomyCeiling string

// Enum values for AutonomyCeiling. draft-pr is the MVP ceiling
// (RFC-0001 §Role config and playbook load).
const (
	CeilingReport     AutonomyCeiling = "report"
	CeilingDraftPR    AutonomyCeiling = "draft-pr"
	CeilingUnattended AutonomyCeiling = "unattended"
)

// ModelTier is the Claude Code --model value a role's runs pass (ADR-0006).
type ModelTier string

// Enum values for ModelTier.
const (
	ModelSonnet ModelTier = "sonnet"
	ModelOpus   ModelTier = "opus"
)

// DefaultInProgressState is what tracker.states.in_progress resolves to when
// config.yaml omits it (US-0011): the work-item state serve applies on
// dispatch, before the runner spawns. Exported so mandat init can state the
// same value in the comment it writes next to the omitted field (US-0013
// AC-13.2) without duplicating the literal.
const DefaultInProgressState = "Doing"

// DefaultPoolSize is what runner.pool_size resolves to when config.yaml
// omits it or sets it to zero (US-0012 AC-12.1). Exported for the same
// reason as DefaultInProgressState.
const DefaultPoolSize = 1

// TrackerStatesConfig names the work-item states serve writes back onto the
// source work item as a run's lifecycle advances (US-0011). It carries no
// done/completed state: mandat never writes one (RFC-0001 §Human plane —
// ratification is a human action, not something the pipeline sets).
type TrackerStatesConfig struct {
	InProgress string `yaml:"in_progress,omitempty"`
}

// TrackerConfig names the tracker instance a mandat installation polls:
// which adapter, and the org/project scope within it (spec §4.10).
type TrackerConfig struct {
	Kind    TrackerKind         `yaml:"kind"`
	Org     string              `yaml:"org"`
	Project string              `yaml:"project"`
	States  TrackerStatesConfig `yaml:"states,omitempty"`
}

// AuthConfig names the credential path token acquisition uses (spec §4.10).
type AuthConfig struct {
	Mode AuthMode `yaml:"mode"`
}

// RunnerConfig bounds how many runs a mandat installation drives at once
// (RFC-0001 post-acceptance amendment 2026-07-16: single-VM concurrent
// dispatch).
type RunnerConfig struct {
	// PoolSize bounds concurrent in-flight tasks; 1 is bit-compatible with
	// the pre-amendment single-in-flight scope (RFC-0001 post-acceptance
	// amendment 2026-07-16).
	PoolSize int `yaml:"pool_size,omitempty"`
}

// EntraConfig names the Entra tenant, agent-identity blueprint, and
// identity mode an installation runs under (ADR-0005, spec §4.1, §4.10).
// Per-role agent identity and agent user ids live on each role's
// RoleConfig entry, not here: they are role-scoped, the tenant and
// blueprint are installation-scoped.
type EntraConfig struct {
	Tenant       string       `yaml:"tenant"`
	Blueprint    string       `yaml:"blueprint"`
	IdentityMode IdentityMode `yaml:"identity_mode"`
}

// RepoConfig is one repo registry entry: url, base branch, and the remit
// defaults every task on this repo inherits (RFC-0001 decision 4: remit
// comes from the repo registry, not a per-work-item field), plus the gate
// command list the verification plane re-runs after the agent's edits
// (RFC-0001 §Gate re-run; the general format is an open question, the
// dogfood target's list is `make check` then `npx govkit check`).
type RepoConfig struct {
	URL        string   `yaml:"url"`
	BaseBranch string   `yaml:"base_branch"`
	Paths      []string `yaml:"paths"`
	Gates      []string `yaml:"gates,omitempty"`
}

// RoleConfig is one role table entry: a RoleAgent as config, never code
// (RFC-0001 §Role config and playbook load, spec glossary). A role name
// absent from the table is not enabled; there is no separate enabled flag.
type RoleConfig struct {
	AgentIdentityID string `yaml:"agent_identity_id"`
	AgentUserID     string `yaml:"agent_user_id,omitempty"`
	// AgentUserName is the agent user's tracker-facing assignee handle (the
	// UPN for Azure DevOps), distinct from AgentUserID (the Entra object id
	// the token chain mints under): the tracker matches assignment on this,
	// not the object id.
	AgentUserName   string          `yaml:"agent_user_name,omitempty"`
	AutonomyCeiling AutonomyCeiling `yaml:"autonomy_ceiling"`
	ModelTier       ModelTier       `yaml:"model_tier,omitempty"`
	Playbook        string          `yaml:"playbook"`
	Skills          []string        `yaml:"skills,omitempty"`
	RemitPaths      []string        `yaml:"remit_paths,omitempty"`
}

// BudgetConfig is the per-run cost ceiling passed to the runner as
// --max-budget-usd (ADR-0006; RFC-0001's default of 5.00 is an MVP
// placeholder, the productionized per-role value is an open question).
type BudgetConfig struct {
	MaxUSDPerRun float64 `yaml:"max_usd_per_run"`
	// MaxUSDInFlight caps total cost across all concurrently in-flight runs
	// (US-0012 AC-12.8). Zero means "derive": consumers treat the effective
	// ceiling as runner.pool_size * budget.max_usd_per_run when unset.
	MaxUSDInFlight float64 `yaml:"max_usd_in_flight,omitempty"`
}

// NotificationConfig names where the human plane's notifications go
// (spec §4.8); approval itself always happens on the tracker, never here.
type NotificationConfig struct {
	Teams []string `yaml:"teams,omitempty"`
}

// Config is the parsed and validated /etc/mandat/config.yaml (spec §4.10):
// tracker target, auth and identity mode, the Entra installation scope, the
// repo registry, the role table, the budget ceiling, and notification
// targets.
type Config struct {
	Tracker       TrackerConfig         `yaml:"tracker"`
	Auth          AuthConfig            `yaml:"auth"`
	Entra         EntraConfig           `yaml:"entra"`
	Repos         map[string]RepoConfig `yaml:"repos"`
	Roles         map[string]RoleConfig `yaml:"roles"`
	Runner        RunnerConfig          `yaml:"runner,omitempty"`
	Budget        BudgetConfig          `yaml:"budget"`
	Notifications NotificationConfig    `yaml:"notifications,omitempty"`
}

// RemitDefaults mirrors the TaskContract.remit shape (RFC-0001
// §TaskContract: "{ repo, base_branch, paths }") so the tracker adapter can
// assign it directly when a work item's repo has no per-item remit.
type RemitDefaults struct {
	Repo       string
	BaseBranch string
	Paths      []string
}

// RemitDefaultsFor returns repo's registry entry as RemitDefaults for the
// adapter to fill TaskContract.remit (RFC-0001 AC-07). It errors when repo
// is absent from the registry; the adapter journals that as a skip
// (RFC-0001 AC-08), it never silently defaults.
func (c *Config) RemitDefaultsFor(repo string) (RemitDefaults, error) {
	rc, ok := c.Repos[repo]
	if !ok {
		return RemitDefaults{}, fmt.Errorf("config: repo %q is not in the repo registry", repo)
	}
	return RemitDefaults{Repo: repo, BaseBranch: rc.BaseBranch, Paths: rc.Paths}, nil
}

// FieldError names one config.yaml field that failed validation, addressed
// by its dotted path (e.g. "roles.dev.autonomy_ceiling").
type FieldError struct {
	Path   string
	Reason string
}

func (e FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// ValidationErrors is the typed error Load returns when config.yaml parses
// but fails validation. It collects every violation found in one pass
// (required fields and enum membership) instead of stopping at the first,
// because /etc/mandat/config.yaml is hand- or wizard-edited and a full
// violation list is cheaper to act on than a fix-one-rerun loop.
type ValidationErrors []FieldError

func (e ValidationErrors) Error() string {
	msgs := make([]string, len(e))
	for i, fe := range e {
		msgs[i] = fe.Error()
	}
	return "config: invalid: " + strings.Join(msgs, "; ")
}

// Load reads path, parses it as YAML into a Config, and validates required
// fields and enum values. A read or YAML-syntax failure returns a plain
// wrapped error; a structurally valid document that fails validation
// returns ValidationErrors.
func Load(path string) (*Config, error) {
	// path is deploy-time config supplied by the operator or mandat init
	// (/etc/mandat/config.yaml), never attacker-controlled input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg.applyDefaults()

	if errs := cfg.validate(); len(errs) > 0 {
		return nil, errs
	}
	return &cfg, nil
}

// applyDefaults fills in optional fields Load leaves unset before validation
// runs, so an omitted field never surfaces as a violation. It runs ahead of
// validate so validation always sees the resolved value.
func (c *Config) applyDefaults() {
	if c.Tracker.States.InProgress == "" {
		c.Tracker.States.InProgress = DefaultInProgressState
	}
	if c.Runner.PoolSize == 0 {
		c.Runner.PoolSize = DefaultPoolSize
	}
}

func (c *Config) validate() ValidationErrors {
	var errs ValidationErrors
	add := func(path, reason string) {
		errs = append(errs, FieldError{Path: path, Reason: reason})
	}

	c.validateTracker(add)
	c.validateAuth(add)
	c.validateEntra(add)
	c.validateRepos(add)
	c.validateRoles(add)

	if c.Runner.PoolSize < 0 {
		add("runner.pool_size", "must not be negative")
	}

	if c.Budget.MaxUSDPerRun <= 0 {
		add("budget.max_usd_per_run", "must be greater than zero")
	}
	if c.Budget.MaxUSDInFlight != 0 && c.Budget.MaxUSDInFlight < c.Budget.MaxUSDPerRun {
		add("budget.max_usd_in_flight", "must be greater than or equal to budget.max_usd_per_run")
	}

	return errs
}

func (c *Config) validateTracker(add func(path, reason string)) {
	switch c.Tracker.Kind {
	case TrackerAzureDevOps, TrackerJira:
	case "":
		add("tracker.kind", "is required")
	default:
		add("tracker.kind", fmt.Sprintf("must be one of %q, %q; got %q", TrackerAzureDevOps, TrackerJira, c.Tracker.Kind))
	}
	if c.Tracker.Org == "" {
		add("tracker.org", "is required")
	}
	if c.Tracker.Project == "" {
		add("tracker.project", "is required")
	}
}

func (c *Config) validateAuth(add func(path, reason string)) {
	switch c.Auth.Mode {
	case AuthArcManagedIdentity, AuthClientCertificate:
	case "":
		add("auth.mode", "is required")
	default:
		add("auth.mode", fmt.Sprintf("must be one of %q, %q; got %q", AuthArcManagedIdentity, AuthClientCertificate, c.Auth.Mode))
	}
}

func (c *Config) validateEntra(add func(path, reason string)) {
	if c.Entra.Tenant == "" {
		add("entra.tenant", "is required")
	}
	if c.Entra.Blueprint == "" {
		add("entra.blueprint", "is required")
	}
	switch c.Entra.IdentityMode {
	case IdentityServicePrincipal, IdentityAgentUserPair, IdentityAgentIdentity:
	case "":
		add("entra.identity_mode", "is required")
	default:
		add("entra.identity_mode", fmt.Sprintf("must be one of %q, %q, %q; got %q",
			IdentityServicePrincipal, IdentityAgentUserPair, IdentityAgentIdentity, c.Entra.IdentityMode))
	}
}

func (c *Config) validateRepos(add func(path, reason string)) {
	if len(c.Repos) == 0 {
		add("repos", "at least one repo registry entry is required")
	}
	for name, rc := range c.Repos {
		p := "repos." + name
		if rc.URL == "" {
			add(p+".url", "is required")
		}
		if rc.BaseBranch == "" {
			add(p+".base_branch", "is required")
		}
		if len(rc.Paths) == 0 {
			// Remit is the mechanical isolation boundary (spec §4.5); an
			// empty path list would leave the agent's worktree with nothing
			// materialized, not a permissive default (ADR-0004 exception 2:
			// security boundaries invest past minimal).
			add(p+".paths", "at least one remit path is required")
		}
		// A malformed registry path today surfaces only at poll time, as a
		// silent per-work-item skip (RFC-0001 AC-08). Mirroring
		// task.Validate's remit.paths checks here catches it at config load
		// instead, before any work item ever resolves through this entry.
		for i, path := range rc.Paths {
			fieldPath := fmt.Sprintf("%s.paths[%d]", p, i)
			if strings.HasPrefix(path, "/") {
				add(fieldPath, "must not be an absolute path")
			}
			if slices.Contains(strings.Split(path, "/"), "..") {
				add(fieldPath, "must not contain a parent-directory (\"..\") segment")
			}
		}
	}
}

func (c *Config) validateRoles(add func(path, reason string)) {
	if len(c.Roles) == 0 {
		add("roles", "at least one enabled role is required")
	}
	for name, rc := range c.Roles {
		p := "roles." + name
		if rc.AgentIdentityID == "" {
			add(p+".agent_identity_id", "is required")
		}
		if c.Entra.IdentityMode == IdentityAgentUserPair && rc.AgentUserID == "" {
			add(p+".agent_user_id", "is required when entra.identity_mode is agent-user-pair")
		}
		if c.Entra.IdentityMode == IdentityAgentUserPair && rc.AgentUserName == "" {
			add(p+".agent_user_name", "is required when entra.identity_mode is agent-user-pair (the tracker matches assignment on the agent user's UPN, not its object id)")
		}
		switch rc.AutonomyCeiling {
		case CeilingReport, CeilingDraftPR, CeilingUnattended:
		case "":
			add(p+".autonomy_ceiling", "is required")
		default:
			add(p+".autonomy_ceiling", fmt.Sprintf("must be one of %q, %q, %q; got %q",
				CeilingReport, CeilingDraftPR, CeilingUnattended, rc.AutonomyCeiling))
		}
		switch rc.ModelTier {
		case "", ModelSonnet, ModelOpus:
		default:
			add(p+".model_tier", fmt.Sprintf("must be one of %q, %q; got %q", ModelSonnet, ModelOpus, rc.ModelTier))
		}
		if rc.Playbook == "" {
			add(p+".playbook", "is required")
		}
	}
}
