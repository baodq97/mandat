package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/baodq97/mandat/internal/config"
)

// devPlaybookPath and reviewerPlaybookPath are the template-derived paths
// this slice writes into roles.dev.playbook / roles.reviewer.playbook
// (US-0013 AC-13.3(b), category (b): no flag names them). The embedded
// playbook templates that a later slice (AC-13.5) writes to these paths are
// out of this slice's scope.
const (
	devPlaybookPath      = "playbooks/dev.md"
	reviewerPlaybookPath = "playbooks/reviewer.md"
)

// reviewerAutonomyCeiling is a constant, not a flag (US-0013 AC-13.3(b)):
// the reviewer role always runs at the report ceiling (GETTING-STARTED §3,
// the read/probe/report role this story's design boundary describes), so
// --autonomy-ceiling only ever sets the dev role's.
const reviewerAutonomyCeiling = config.CeilingReport

// stringSliceFlag accumulates repeated flag occurrences (--remit-path,
// --gate) into an ordered slice, in the order the operator gave them.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// nonInteractiveInput is every irreducible field US-0013 AC-13.3(c) and
// AC-13.9 name, collected from --non-interactive flags.
type nonInteractiveInput struct {
	trackerOrg     string
	trackerProject string
	authMode       string
	entraTenant    string
	entraBlueprint string

	repoRaw    string // raw --repo value, "key=url"
	repoKey    string
	repoURL    string
	baseBranch string
	remitPaths []string
	gates      []string

	devIdentityID string
	devUserID     string
	devUserUPN    string

	reviewerIdentityID string
	reviewerUserID     string
	reviewerUserUPN    string

	autonomyCeiling string
	maxUSDPerRun    float64
}

// initCmd writes /etc/mandat/config.yaml from operator-supplied flags
// (US-0013 slice 1). Only the --non-interactive path is implemented: it
// validates every irreducible field is present as a flag, renders a fully
// commented config.yaml, and writes it in one shot. A TTY session without
// --non-interactive gets a clear "not implemented yet" error instead of a
// blocking prompt: the interactive interview itself is a later slice's
// scope, so this never reads stdin (AC-13.9's non-TTY autodetect holds
// trivially — every path here is non-interactive).
func initCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)

	configPath := fs.String("config", defaultConfigPath, "path to write config.yaml")
	nonInteractiveFlag := fs.Bool("non-interactive", false, "accept configuration from flags instead of an interactive interview")

	trackerOrg := fs.String("tracker-org", "", "Azure DevOps organization name")
	trackerProject := fs.String("tracker-project", "", "Azure DevOps project name")
	authMode := fs.String("auth-mode", "", "credential path: arc-managed-identity or client-certificate")
	entraTenant := fs.String("entra-tenant", "", "Entra tenant id")
	entraBlueprint := fs.String("entra-blueprint", "", "Entra agent-identity blueprint name")
	repo := fs.String("repo", "", "repo registry entry as key=url")
	baseBranch := fs.String("base-branch", "", "base branch for the repo")
	var remitPaths stringSliceFlag
	fs.Var(&remitPaths, "remit-path", "remit path for the repo (repeatable)")
	var gates stringSliceFlag
	fs.Var(&gates, "gate", "gate command the verifier re-runs after the agent's edits (repeatable)")

	devIdentityID := fs.String("dev-identity-id", "", "dev role Entra agent identity id")
	devUserID := fs.String("dev-user-id", "", "dev role Entra agent user object id")
	devUserUPN := fs.String("dev-user-upn", "", "dev role agent user UPN (the tracker assignment handle)")
	reviewerIdentityID := fs.String("reviewer-identity-id", "", "reviewer role Entra agent identity id")
	reviewerUserID := fs.String("reviewer-user-id", "", "reviewer role Entra agent user object id")
	reviewerUserUPN := fs.String("reviewer-user-upn", "", "reviewer role agent user UPN")
	autonomyCeiling := fs.String("autonomy-ceiling", "", "dev role autonomy ceiling: report, draft-pr, or unattended")
	maxUSDPerRun := fs.Float64("max-usd-per-run", 0, "per-run cost ceiling (budget.max_usd_per_run)")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !*nonInteractiveFlag && isTTY(stdin) {
		fmt.Fprintln(stderr, "mandat init: interactive setup is not implemented in this build; rerun with --non-interactive and the required flags")
		return 2
	}

	in := nonInteractiveInput{
		trackerOrg:     *trackerOrg,
		trackerProject: *trackerProject,
		authMode:       *authMode,
		entraTenant:    *entraTenant,
		entraBlueprint: *entraBlueprint,
		repoRaw:        *repo,
		baseBranch:     *baseBranch,
		remitPaths:     remitPaths,
		gates:          gates,

		devIdentityID: *devIdentityID,
		devUserID:     *devUserID,
		devUserUPN:    *devUserUPN,

		reviewerIdentityID: *reviewerIdentityID,
		reviewerUserID:     *reviewerUserID,
		reviewerUserUPN:    *reviewerUserUPN,

		autonomyCeiling: *autonomyCeiling,
		maxUSDPerRun:    *maxUSDPerRun,
	}

	if err := in.validate(); err != nil {
		fmt.Fprintf(stderr, "mandat init: %v\n", err)
		return 1
	}

	yamlText := in.render()

	if err := os.MkdirAll(filepath.Dir(*configPath), 0o750); err != nil {
		fmt.Fprintf(stderr, "mandat init: create %s: %v\n", filepath.Dir(*configPath), err)
		return 1
	}
	if err := os.WriteFile(*configPath, []byte(yamlText), 0o600); err != nil {
		fmt.Fprintf(stderr, "mandat init: write %s: %v\n", *configPath, err)
		return 1
	}
	fmt.Fprintf(stdout, "mandat init: wrote %s\n", *configPath)
	return 0
}

// isTTY reports whether stdin is a live terminal session. A non-*os.File
// reader (tests inject a strings.Reader or an in-memory buffer) is never a
// TTY, so it always resolves to non-interactive.
func isTTY(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

// requiredFlag names one non-interactive input against the flag that sets
// it and whether it was supplied.
type requiredFlag struct {
	flag    string
	present bool
}

// validate checks every irreducible field US-0013 AC-13.3(c)/AC-13.9 name is
// present, then parses --repo and checks the two enum-valued flags, in flag
// order, so the first violation is what the operator sees (a missing flag
// or a bad enum value never gets bundled behind an earlier one). Nothing is
// written until this returns nil (AC-13.9).
func (in *nonInteractiveInput) validate() error {
	checks := []requiredFlag{
		{"--tracker-org", in.trackerOrg != ""},
		{"--tracker-project", in.trackerProject != ""},
		{"--auth-mode", in.authMode != ""},
		{"--entra-tenant", in.entraTenant != ""},
		{"--entra-blueprint", in.entraBlueprint != ""},
		{"--repo", in.repoRaw != ""},
		{"--base-branch", in.baseBranch != ""},
		{"--remit-path", len(in.remitPaths) > 0},
		{"--gate", len(in.gates) > 0},
		{"--dev-identity-id", in.devIdentityID != ""},
		{"--dev-user-id", in.devUserID != ""},
		{"--dev-user-upn", in.devUserUPN != ""},
		{"--reviewer-identity-id", in.reviewerIdentityID != ""},
		{"--reviewer-user-id", in.reviewerUserID != ""},
		{"--reviewer-user-upn", in.reviewerUserUPN != ""},
		{"--autonomy-ceiling", in.autonomyCeiling != ""},
		{"--max-usd-per-run", in.maxUSDPerRun > 0},
	}
	for _, c := range checks {
		if !c.present {
			return fmt.Errorf("%s is required in --non-interactive mode", c.flag)
		}
	}

	key, url, ok := strings.Cut(in.repoRaw, "=")
	if !ok || key == "" || url == "" {
		return fmt.Errorf("--repo must be key=url; got %q", in.repoRaw)
	}
	in.repoKey, in.repoURL = key, url

	switch config.AuthMode(in.authMode) {
	case config.AuthArcManagedIdentity, config.AuthClientCertificate:
	default:
		return fmt.Errorf("--auth-mode must be %q or %q; got %q", config.AuthArcManagedIdentity, config.AuthClientCertificate, in.authMode)
	}

	switch config.AutonomyCeiling(in.autonomyCeiling) {
	case config.CeilingReport, config.CeilingDraftPR, config.CeilingUnattended:
	default:
		return fmt.Errorf("--autonomy-ceiling must be %q, %q, or %q; got %q",
			config.CeilingReport, config.CeilingDraftPR, config.CeilingUnattended, in.autonomyCeiling)
	}

	return nil
}

// render writes the config.yaml text config.Load parses. Every field it
// sets from a flag is written directly; every optional (omitempty) field in
// config.go that this slice takes no flag for is left out of the document
// and instead named in an adjacent comment stating its default, its derive
// rule, or its no-default omission behavior (US-0013 AC-13.2), so a
// completed run explains the whole schema even though it only ever
// populates the irreducible subset.
func (in nonInteractiveInput) render() string {
	var b strings.Builder

	b.WriteString("tracker:\n")
	fmt.Fprintf(&b, "  kind: %s\n", config.TrackerAzureDevOps)
	fmt.Fprintf(&b, "  org: %s\n", in.trackerOrg)
	fmt.Fprintf(&b, "  project: %s\n", in.trackerProject)
	fmt.Fprintf(&b, "  # states.in_progress default: %s (US-0011); the work-item state serve applies on dispatch, before the runner spawns\n", config.DefaultInProgressState)
	b.WriteString("  # states:\n")
	fmt.Fprintf(&b, "  #   in_progress: %s\n", config.DefaultInProgressState)
	b.WriteString("\n")

	b.WriteString("auth:\n")
	fmt.Fprintf(&b, "  mode: %s\n\n", in.authMode)

	b.WriteString("entra:\n")
	fmt.Fprintf(&b, "  tenant: %s\n", in.entraTenant)
	fmt.Fprintf(&b, "  blueprint: %s\n", in.entraBlueprint)
	fmt.Fprintf(&b, "  identity_mode: %s\n\n", config.IdentityAgentUserPair)

	b.WriteString("repos:\n")
	fmt.Fprintf(&b, "  %s:\n", in.repoKey)
	fmt.Fprintf(&b, "    url: %s\n", in.repoURL)
	fmt.Fprintf(&b, "    base_branch: %s\n", in.baseBranch)
	b.WriteString("    paths:\n")
	for _, p := range in.remitPaths {
		fmt.Fprintf(&b, "      - %s\n", p)
	}
	b.WriteString("    # gates: no default; an empty list means the verifier re-runs no gate commands after the agent's edits\n")
	b.WriteString("    gates:\n")
	for _, g := range in.gates {
		fmt.Fprintf(&b, "      - %s\n", g)
	}
	b.WriteString("\n")

	b.WriteString("roles:\n")
	b.WriteString(renderRole("dev", in.devIdentityID, in.devUserID, in.devUserUPN, in.autonomyCeiling, devPlaybookPath))
	b.WriteString(renderRole("reviewer", in.reviewerIdentityID, in.reviewerUserID, in.reviewerUserUPN, string(reviewerAutonomyCeiling), reviewerPlaybookPath))
	b.WriteString("\n")

	fmt.Fprintf(&b, "# runner.pool_size default: %d (US-0012 AC-12.1); bounds concurrent in-flight tasks\n", config.DefaultPoolSize)
	b.WriteString("# runner:\n")
	fmt.Fprintf(&b, "#   pool_size: %d\n\n", config.DefaultPoolSize)

	b.WriteString("budget:\n")
	fmt.Fprintf(&b, "  max_usd_per_run: %s\n", strconv.FormatFloat(in.maxUSDPerRun, 'f', -1, 64))
	b.WriteString("  # max_usd_in_flight: no default; derives as runner.pool_size * budget.max_usd_per_run when omitted (US-0012 AC-12.8)\n\n")

	b.WriteString("# notifications.teams: no default; omitted means no Teams webhook targets are notified\n")
	b.WriteString("# notifications:\n")
	b.WriteString("#   teams: []\n")

	return b.String()
}

// renderRole writes one roles.<name> entry, including the adjacent comments
// AC-13.2 requires for the three role-scoped omitempty fields this slice
// takes no flag for: model_tier, skills, and remit_paths.
func renderRole(name, identityID, userID, userUPN, ceiling, playbook string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s:\n", name)
	fmt.Fprintf(&b, "    agent_identity_id: %s\n", identityID)
	fmt.Fprintf(&b, "    agent_user_id: %s\n", userID)
	fmt.Fprintf(&b, "    agent_user_name: %s\n", userUPN)
	fmt.Fprintf(&b, "    autonomy_ceiling: %s\n", ceiling)
	b.WriteString("    # model_tier: no default; omitted means no --model flag is passed\n")
	fmt.Fprintf(&b, "    playbook: %s\n", playbook)
	b.WriteString("    # skills: no default; omitted means the role runs no named skills\n")
	b.WriteString("    # remit_paths: no default; omitted means no per-role remit override (the repo registry's paths apply)\n")
	return b.String()
}
