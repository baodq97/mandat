package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/baodq97/mandat/internal/adapter/azuredevops"
	"github.com/baodq97/mandat/internal/buildinfo"
	"github.com/baodq97/mandat/internal/config"
	"github.com/baodq97/mandat/internal/identity"
	"github.com/baodq97/mandat/internal/journal"
	"github.com/baodq97/mandat/internal/orchestrator"
	"github.com/baodq97/mandat/internal/role"
	"github.com/baodq97/mandat/internal/runner"
	"github.com/baodq97/mandat/internal/task"
	"github.com/baodq97/mandat/internal/verify"
	"github.com/baodq97/mandat/internal/workspace"
)

const (
	defaultConfigPath   = "/etc/mandat/config.yaml"
	defaultMaxBudgetUSD = 5.00
	defaultPollInterval = 30 * time.Second

	// adoBaseURL is the Azure DevOps host root every adapter path hangs off; a
	// contract test points the adapter at an httptest server instead (RFC-0001 §9).
	adoBaseURL = "https://dev.azure.com"

	// mandatStateRoot holds the per-role OS-user home, per-role Claude config store,
	// and the shared mirror cache the workspace plane borrows worktrees from
	// (spec §4.5, §6). Production layout; tests pass temp dirs.
	mandatStateRoot = "/var/lib/mandat"

	// systemOrchestrator is the acting identity the journal records for the
	// orchestrator's own state transitions; the runner and PR-open act as the Dev
	// agent user and the ground-truth probe as the Reviewer identity, which is how
	// writer != scorer shows up in the audit trail (RFC-0001 §Journal).
	systemOrchestrator = "system:orchestrator"

	// The ground-truth act names the journal records alongside the state
	// transitions (RFC-0001 AC-28); they carry no state change of their own.
	actPROpened      = "pr_opened"
	actGateRerun     = "gate_rerun"
	actProbePRExists = "probe_pr_exists"
)

// The pipeline plane seams runTask keys off, each narrowed to the methods the
// composition needs so the walking-skeleton test injects doubles (RFC-0001 §9): a
// recorded-ADO fixture behind taskTracker and prForge, a fake claude behind
// taskRunner, and a Reviewer-identity fake behind the verifier's own probe.
// taskTracker mirrors tracker.Tracker in full (not just Poll): runTask also
// writes tracker lifecycle feedback back onto the source work item (US-0018).
type (
	taskTracker interface {
		Poll(ctx context.Context) ([]task.TaskContract, error)
		Comment(ctx context.Context, workItemID, text string) error
		ApplyStatus(ctx context.Context, workItemID, status string) error
	}
	prForge interface {
		CreatePR(ctx context.Context, in azuredevops.CreatePRInput) (azuredevops.CreatePRResult, error)
	}
	taskRunner interface {
		Run(ctx context.Context, req runner.Request) (runner.Outcome, error)
	}
	runVerifier interface {
		Verify(ctx context.Context, req verify.Request) (verify.Verdict, error)
	}
)

// serveDeps is the composition root: the twelve planes wired into one runnable
// pipeline. The plane seams are interfaces (and Provision a function) so the
// walking-skeleton test injects the §9 doubles; the scalar fields are the
// installation-scoped values serve() resolves from config once and every task run
// reuses. Provision stays a function because workspace.Provision is a package
// function, not a method, and the skeleton's happy path exercises the real one
// against a local bare git origin rather than a stub.
type serveDeps struct {
	Store     *journal.Store
	Tracker   taskTracker
	Forge     prForge
	Runner    taskRunner
	Verifier  runVerifier
	Provision func(ctx context.Context, cfg workspace.Config) (*workspace.Workspace, error)

	Role             role.Role
	ReviewerIdentity string

	// InProgressState is the tracker.states.in_progress config value (US-0018):
	// the work-item state serve applies on dispatch, before the runner spawns.
	InProgressState string

	// RepoURL, Gates, and MirrorDir resolve per-repo installation config the task
	// contract does not carry: the git origin the worktree mirrors, the gate command
	// list the verifier re-runs, and the per-repo mirror cache location.
	RepoURL   func(repo string) (string, error)
	Gates     func(repo string) []string
	MirrorDir func(repo string) string

	TasksRoot           string
	RoleUser            string
	Home                string
	ConfigDir           string
	GitCredentialHelper string
	DenyToolHookCommand string

	HarnessVersion string
	ConfigVersion  string
}

// runTask drives one TaskContract through the skeleton pipeline and returns the
// state it settled in (in-review on the happy path, needs-human on every
// deterministic hold). It composes the planes but keeps zero I/O of its own beyond
// the store: the orchestrator decides transitions, the runner and verifier derive
// events, and every transition plus every ground-truth probe lands in the
// append-only journal named by the acting identity (RFC-0001 §Journal, AC-28).
func runTask(ctx context.Context, d serveDeps, tc task.TaskContract) (orchestrator.State, error) {
	// dispatch: the pre-creation pseudo-state -> queued, journaled with an empty
	// from_state (RFC-0001 AC-05).
	state, err := orchestrator.Next(orchestrator.StateStart, orchestrator.EventDispatch)
	if err != nil {
		return orchestrator.StateStart, fmt.Errorf("serve: dispatch task %s: %w", tc.ID, err)
	}
	tc.State = task.State(state)
	if err := d.Store.UpsertTask(ctx, &tc); err != nil {
		return state, fmt.Errorf("serve: persist task %s: %w", tc.ID, err)
	}
	if err := d.journalTransition(ctx, &tc, orchestrator.StateStart, state, orchestrator.EventDispatch, "", nil); err != nil {
		return state, err
	}

	// claim: provision the isolated worktree, then queued -> in-progress. A setup
	// failure routes queued -> needs-human with no shared-checkout fallback and no
	// retry (spec §4.5, RFC-0001 AC-18).
	ws, err := d.provision(ctx, &tc)
	if err != nil {
		to, tErr := d.transition(ctx, &tc, state, orchestrator.EventSetupFailed, "",
			detailJSON(map[string]any{"error": err.Error()}))
		d.postHoldComment(ctx, &tc, err.Error())
		return to, tErr
	}
	state, err = d.transition(ctx, &tc, state, orchestrator.EventClaimOK, "", nil)
	if err != nil {
		return state, err
	}

	// Tracker lifecycle feedback: before the runner spawns, set the work item to
	// the configured in-progress state and name the task and acting role in a
	// comment (US-0018). Best-effort — a failed write logs a warning and never
	// holds up the run.
	d.applyTrackerStatus(ctx, &tc, d.InProgressState)
	d.postTrackerComment(ctx, &tc, fmt.Sprintf("mandat dispatched task %s to the %s role.", tc.ID, d.Role.Name))

	// run the agent once (ADR-0006). The runner derives one event from the
	// schema-validated ResultContract file, never from stream-json prose.
	out, err := d.Runner.Run(ctx, d.runRequest(&tc, ws))
	if err != nil {
		return state, fmt.Errorf("serve: run task %s: %w", tc.ID, err)
	}

	// A non-result_ok run (result_needs_human, result_invalid) holds for a human
	// with no PR advance and no auto-retry (decision 3): the composition never opens
	// a PR or runs verification for a run the file did not vouch for.
	if out.Event != orchestrator.EventResultOK {
		var detail map[string]any
		if out.Result != nil && out.Result.Reason != "" {
			detail = map[string]any{"reason": out.Result.Reason}
		}
		to, tErr := d.transition(ctx, &tc, state, out.Event, out.RunID, detailJSON(detail))
		d.postHoldComment(ctx, &tc, runReason(out))
		return to, tErr
	}

	// The runner reported a valid completed ResultContract. Open the draft PR under
	// the Dev agent user BEFORE verification runs, so the verifier's Reviewer-identity
	// probe has a real PR to confirm: the fake claude self-reports a pr_url in its
	// artifact but opens nothing, so the composition root is what actually opens the
	// PR here (the productionized runner opens it in-band; this seam stays the same).
	// WorkItemID passes the source work item so ADO links the PR at create time: this
	// populates the PR's own work-item refs AND writes the ArtifactLink relation back
	// onto the work item (verified live against ADO), which is what makes the work
	// item's UI show the PR without a second round-trip.
	pr, err := d.Forge.CreatePR(ctx, azuredevops.CreatePRInput{
		Repo:        tc.Remit.Repo,
		Branch:      ws.Branch,
		BaseBranch:  tc.Remit.BaseBranch,
		Title:       tc.Title,
		Description: prDescription(&tc),
		WorkItemID:  tc.TrackerRef.WorkItemID,
	})
	if err != nil {
		return state, fmt.Errorf("serve: open draft PR for task %s: %w", tc.ID, err)
	}
	if err := d.journalAct(ctx, &tc, out.RunID, d.Role.Mandate.AgentUserID, actPROpened,
		detailJSON(map[string]any{"pr_url": pr.URL, "pr_id": pr.ID, "created_by": pr.CreatedBy})); err != nil {
		return state, err
	}
	d.postTrackerComment(ctx, &tc, fmt.Sprintf("mandat opened a draft PR for task %s: %s", tc.ID, pr.URL))

	// verify: gate re-run + diff-inside-remit + Reviewer-identity PR probe, all in
	// the verifier's own trusted context, never trusting the agent's self-report
	// (RFC-0001 §4.7). The verdict's result_ok requires all three to pass.
	verdict, err := d.Verifier.Verify(ctx, verify.Request{
		Task:        &tc,
		WorktreeDir: ws.Dir,
		Branch:      ws.Branch,
		Gates:       d.Gates(tc.Remit.Repo),
		Remit:       ws,
	})
	if err != nil {
		return state, fmt.Errorf("serve: verify task %s: %w", tc.ID, err)
	}
	if len(verdict.Gates) > 0 {
		if err := d.journalAct(ctx, &tc, out.RunID, d.ReviewerIdentity, actGateRerun,
			detailJSON(map[string]any{"gates": verdict.Gates})); err != nil {
			return state, err
		}
	}

	// Compose the runner's and the verifier's result_ok: a red gate, an out-of-remit
	// diff, or a failed PR probe each hold the task for a human, even though the
	// runner already reported completed (RFC-0001 state machine: result_ok is
	// conditioned on gates green AND diff-in-remit AND the probe confirming).
	if verdict.Event != orchestrator.EventResultOK {
		to, tErr := d.transition(ctx, &tc, state, verdict.Event, out.RunID,
			detailJSON(map[string]any{"check": string(verdict.Failed), "detail": verdict.Detail}))
		d.postHoldComment(ctx, &tc, verdict.Detail)
		return to, tErr
	}
	if err := d.journalAct(ctx, &tc, out.RunID, d.ReviewerIdentity, actProbePRExists,
		detailJSON(map[string]any{"pr_url": verdict.PR.URL, "created_by": verdict.PR.CreatedBy})); err != nil {
		return state, err
	}

	// Both hold: in-progress -> in-review, the skeleton's definition of done.
	return d.transition(ctx, &tc, state, orchestrator.EventResultOK, out.RunID,
		detailJSON(map[string]any{"pr_url": verdict.PR.URL}))
}

// provision resolves the git origin for the task's repo and hands it to the
// workspace plane; a missing repo URL and a workspace SetupError both surface as an
// error the caller maps to setup_failed.
func (d serveDeps) provision(ctx context.Context, tc *task.TaskContract) (*workspace.Workspace, error) {
	repoURL, err := d.RepoURL(tc.Remit.Repo)
	if err != nil {
		return nil, err
	}
	return d.Provision(ctx, workspace.Config{
		RepoURL:          repoURL,
		MirrorDir:        d.MirrorDir(tc.Remit.Repo),
		TasksRoot:        d.TasksRoot,
		TaskID:           tc.ID,
		Remit:            tc.Remit,
		CredentialHelper: d.GitCredentialHelper,
	})
}

// runRequest builds the runner request from the resolved role and the per-role OS
// isolation the composition supplies (config carries no OS-user field, so the runner
// is told them, ADR-0006 §Isolation).
func (d serveDeps) runRequest(tc *task.TaskContract, ws *workspace.Workspace) runner.Request {
	return runner.Request{
		Task:                tc,
		Role:                d.Role,
		Worktree:            ws,
		RoleUser:            d.RoleUser,
		Home:                d.Home,
		ConfigDir:           d.ConfigDir,
		GitCredentialHelper: d.GitCredentialHelper,
		DenyToolHookCommand: d.DenyToolHookCommand,
		HarnessVersion:      d.HarnessVersion,
		ConfigVersion:       d.ConfigVersion,
	}
}

// transition advances the orchestrator, persists the task's new state, and journals
// the transition as system:orchestrator. An unlisted (state, event) pair returns the
// input state and an error, so the pipeline never silently advances (RFC-0001
// §Orchestrator: Next is total over the enumerated inputs).
func (d serveDeps) transition(ctx context.Context, tc *task.TaskContract, from orchestrator.State, event orchestrator.Event, runID string, detail []byte) (orchestrator.State, error) {
	to, err := orchestrator.Next(from, event)
	if err != nil {
		return from, fmt.Errorf("serve: task %s transition on %q: %w", tc.ID, event, err)
	}
	tc.State = task.State(to)
	if err := d.Store.UpsertTask(ctx, tc); err != nil {
		return from, fmt.Errorf("serve: persist task %s: %w", tc.ID, err)
	}
	if err := d.journalTransition(ctx, tc, from, to, event, runID, detail); err != nil {
		return from, err
	}
	return to, nil
}

func (d serveDeps) journalTransition(ctx context.Context, tc *task.TaskContract, from, to orchestrator.State, event orchestrator.Event, runID string, detail []byte) error {
	return d.writeEvent(ctx, journal.JournalEvent{
		TaskID:         tc.ID,
		RunID:          runID,
		ActingIdentity: systemOrchestrator,
		Act:            string(event),
		FromState:      string(from),
		ToState:        string(to),
		Detail:         detail,
		ConfigVersion:  d.ConfigVersion,
		HarnessVersion: d.HarnessVersion,
	})
}

func (d serveDeps) journalAct(ctx context.Context, tc *task.TaskContract, runID, acting, act string, detail []byte) error {
	return d.writeEvent(ctx, journal.JournalEvent{
		TaskID:         tc.ID,
		RunID:          runID,
		ActingIdentity: acting,
		Act:            act,
		Detail:         detail,
		ConfigVersion:  d.ConfigVersion,
		HarnessVersion: d.HarnessVersion,
	})
}

func (d serveDeps) writeEvent(ctx context.Context, e journal.JournalEvent) error {
	if err := d.Store.AppendEvent(ctx, e); err != nil {
		return fmt.Errorf("serve: journal %q for task %s: %w", e.Act, e.TaskID, err)
	}
	return nil
}

// applyTrackerStatus sets the source work item's tracker state. Tracker feedback
// is best-effort (US-0018): a failed write logs a warning and never fails the
// task or changes its pipeline outcome, since the journal (not the tracker) is
// the pipeline's own source of truth.
func (d serveDeps) applyTrackerStatus(ctx context.Context, tc *task.TaskContract, status string) {
	if err := d.Tracker.ApplyStatus(ctx, tc.TrackerRef.WorkItemID, status); err != nil {
		slog.Warn("serve: tracker ApplyStatus failed", "task", tc.ID, "work_item_id", tc.TrackerRef.WorkItemID, "status", status, "error", err)
	}
}

// postTrackerComment posts a comment onto the source work item. Best-effort, like
// applyTrackerStatus: a failed post logs a warning and never fails the task.
func (d serveDeps) postTrackerComment(ctx context.Context, tc *task.TaskContract, text string) {
	if err := d.Tracker.Comment(ctx, tc.TrackerRef.WorkItemID, text); err != nil {
		slog.Warn("serve: tracker Comment failed", "task", tc.ID, "work_item_id", tc.TrackerRef.WorkItemID, "error", err)
	}
}

// postHoldComment names the reason a run held for a human — a gate detail, a
// remit violation, a probe finding, or the runner's own reason — on the source
// work item (US-0018).
func (d serveDeps) postHoldComment(ctx context.Context, tc *task.TaskContract, reason string) {
	d.postTrackerComment(ctx, tc, fmt.Sprintf("mandat held task %s for a human: %s", tc.ID, reason))
}

// runReason resolves the reason a non-result_ok run holds for a human: the
// runner-reported ResultContract reason when one was produced, or a fallback
// noting no valid ResultContract landed at all.
func runReason(out runner.Outcome) string {
	if out.Result != nil && out.Result.Reason != "" {
		return out.Result.Reason
	}
	return "the run did not produce a valid ResultContract"
}

// detailJSON encodes a journal detail payload, mapping an empty payload to a nil
// (NULL) detail. A marshal failure of these flat maps is unreachable, so it drops
// the advisory detail rather than failing a transition on it.
func detailJSON(fields map[string]any) []byte {
	if len(fields) == 0 {
		return nil
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return b
}

func prDescription(tc *task.TaskContract) string {
	return fmt.Sprintf("Opened by mandat under the Dev RoleAgent mandate for %s (%s).", tc.ID, tc.TrackerRef.URL)
}

// serve is the 30s poll/dispatch daemon: it builds the real planes from config and
// loops, running each polled TaskContract through runTask. It is the thin
// real-deps wrapper around the testable runTask; the walking-skeleton proof lives at
// runTask with the §9 doubles, not here, because this path needs live ADO/Entra.
func serve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	dbPath := fs.String("db", journal.DefaultPath, "path to the SQLite journal file")
	roleName := fs.String("role", "dev", "the RoleAgent to dispatch (MVP skeleton: dev)")
	maxBudget := fs.Float64("max-budget-usd", defaultMaxBudgetUSD, "per-run cost ceiling passed to the runner")
	interval := fs.Duration("poll-interval", defaultPollInterval, "WIQL poll interval")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "mandat serve: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := journal.Open(ctx, *dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "mandat serve: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	deps, err := buildServeDeps(cfg, store, *roleName, *maxBudget)
	if err != nil {
		fmt.Fprintf(stderr, "mandat serve: %v\n", err)
		return 1
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		dispatchCycle(ctx, deps, stdout, stderr)
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

// dispatchCycle polls once and runs every not-yet-dispatched contract. Re-polling an
// already-dispatched work item is idempotent on the stable tracker-derived id
// (RFC-0001 AC-03): the task is already in the store, so it is skipped.
func dispatchCycle(ctx context.Context, d serveDeps, stdout, stderr io.Writer) {
	contracts, err := d.Tracker.Poll(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "mandat serve: poll: %v\n", err)
		return
	}
	for _, tc := range contracts {
		if _, err := d.Store.LoadTask(ctx, tc.ID); err == nil {
			continue
		}
		state, err := runTask(ctx, d, tc)
		if err != nil {
			fmt.Fprintf(stderr, "mandat serve: task %s: %v\n", tc.ID, err)
			continue
		}
		fmt.Fprintf(stdout, "mandat serve: task %s reached %s\n", tc.ID, state)
	}
}

// buildServeDeps wires the real planes from the loaded config. Several seams are
// gated stubs pending their spikes/stories — the managed-identity blueprint
// credential (ADR-0005 prod path), the Reviewer-identity PR probe (needs an ADO
// FindPR), and the remit-guard deny hook — so a live run holds at needs-human rather
// than certifying an unprobed PR; the walking-skeleton test proves the composed
// pipeline with doubles in their place.
//
// GitCredentialHelper points git at the binary re-invoked as its credential helper
// (the `!` marks a shell command, so git appends the get/store/erase operation as
// the final argument, RFC-0001 §Identity injection); the delegated token is minted
// per get and never written to the worktree (S-credential-delivery).
// DenyToolHookCommand is the PreToolUse mechanical deny hook (remit-guard, a separate
// story); the runner owns only the settings-JSON shape, the command is the caller's.
func buildServeDeps(cfg *config.Config, store *journal.Store, roleName string, maxBudget float64) (serveDeps, error) {
	r, err := role.Resolve(cfg, roleName)
	if err != nil {
		return serveDeps{}, fmt.Errorf("serve: resolve role: %w", err)
	}

	broker, err := buildBroker(cfg)
	if err != nil {
		return serveDeps{}, fmt.Errorf("serve: build broker: %w", err)
	}
	adapter, err := azuredevops.New(azuredevops.Config{
		BaseURL:          adoBaseURL,
		Org:              cfg.Tracker.Org,
		Project:          cfg.Tracker.Project,
		Role:             roleName,
		DevAgentUserName: r.Mandate.AgentUserName,
		Tokens:           broker,
		Remits:           cfg,
	})
	if err != nil {
		return serveDeps{}, fmt.Errorf("serve: build tracker adapter: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		self = "mandat"
	}

	reviewer := reviewerIdentity(cfg)
	probe, err := buildReviewerProbe(cfg, broker, reviewer)
	if err != nil {
		return serveDeps{}, err
	}

	return serveDeps{
		Store:               store,
		Tracker:             adapter,
		Forge:               adapter,
		Runner:              runner.New(store, selectSpawner(), runner.Config{ClaudePath: "claude", MaxBudgetUSD: maxBudget}),
		Verifier:            verify.New(probe),
		Provision:           workspace.Provision,
		Role:                r,
		ReviewerIdentity:    reviewer,
		InProgressState:     cfg.Tracker.States.InProgress,
		RepoURL:             repoURLResolver(cfg),
		Gates:               gatesResolver(cfg),
		MirrorDir:           func(repo string) string { return filepath.Join(mandatStateRoot, "mirror", repo+".git") },
		TasksRoot:           workspace.DefaultTasksRoot,
		RoleUser:            "mandat-" + roleName,
		Home:                filepath.Join(mandatStateRoot, "roles", roleName, "home"),
		ConfigDir:           filepath.Join(mandatStateRoot, "roles", roleName, "claude-config"),
		GitCredentialHelper: "!" + self + " git-credential --role " + roleName,
		DenyToolHookCommand: self + ` remit-guard --worktree "$CLAUDE_PROJECT_DIR"`,
		HarnessVersion:      buildinfo.Version(),
	}, nil
}

// selectSpawner picks the runner's process spawner. MANDAT_DIRECT_SPAWN is the
// pilot escape hatch: it swaps the OS-user isolation spawner for a same-user
// direct exec on pilot/dev VMs that lack root and a provisioned mandat-<role>
// user (spec §4.5). The default stays the OS-user isolation path;
// sparse-checkout, the diff-inside-remit check, and the remit-guard hook remain
// the active remit layers either way.
func selectSpawner() workspace.Spawner {
	if os.Getenv("MANDAT_DIRECT_SPAWN") != "" {
		return workspace.DirectSpawner
	}
	return workspace.DefaultSpawner
}

func repoURLResolver(cfg *config.Config) func(string) (string, error) {
	return func(repo string) (string, error) {
		rc, ok := cfg.Repos[repo]
		if !ok {
			return "", fmt.Errorf("serve: repo %q is not in the registry", repo)
		}
		return rc.URL, nil
	}
}

func gatesResolver(cfg *config.Config) func(string) []string {
	return func(repo string) []string {
		return cfg.Repos[repo].Gates
	}
}

// reviewerIdentity resolves the Reviewer agent-user principal the ground-truth PR
// probe acts as (writer != scorer, RFC-0001 §4.1). verify.Verify compares this
// against TaskContract.AssignedTo, which is the UPN ADO stores (not the Entra
// object id), so this reads AgentUserName, not AgentUserID. The Reviewer identity
// is provisioned even though its LLM playbook is deferred (PRD §Scope), so the
// skeleton reads it from a `reviewer` role entry when present.
func reviewerIdentity(cfg *config.Config) string {
	if rc, ok := cfg.Roles["reviewer"]; ok {
		return rc.AgentUserName
	}
	return ""
}

// buildReviewerProbe wires the Reviewer-identity PR-existence probe (RFC-0001
// AC-27). When a `reviewer` role is configured, it builds a second azuredevops
// Adapter instance under Role="reviewer" (same broker, base URL, org, and
// project as the Dev adapter) so FindPR mints Reviewer tokens, a principal
// distinct from the Dev agent user that opened the PR — the probe's own role
// is what makes writer != scorer an IAM property rather than a convention.
// Absent a `reviewer` role entry, it falls back to the stub that always errors,
// so a live run holds at needs-human rather than certifying an unprobed PR.
func buildReviewerProbe(cfg *config.Config, broker *identity.Broker, reviewerIdentity string) (verify.PRProbe, error) {
	rc, ok := cfg.Roles["reviewer"]
	if !ok {
		return reviewerProbeStub{identity: reviewerIdentity}, nil
	}
	adapter, err := azuredevops.New(azuredevops.Config{
		BaseURL:          adoBaseURL,
		Org:              cfg.Tracker.Org,
		Project:          cfg.Tracker.Project,
		Role:             "reviewer",
		DevAgentUserName: rc.AgentUserName,
		Tokens:           broker,
		Remits:           cfg,
	})
	if err != nil {
		return nil, fmt.Errorf("serve: build reviewer adapter: %w", err)
	}
	return reviewerAdapterProbe{adapter: adapter, identity: reviewerIdentity}, nil
}

// reviewerAdapterProbe adapts *azuredevops.Adapter.FindPR to verify.PRProbe,
// mapping the adapter-local PRFinding into verify.PRInfo at this composition
// root — the adapter itself never imports internal/verify.
type reviewerAdapterProbe struct {
	adapter  *azuredevops.Adapter
	identity string
}

func (p reviewerAdapterProbe) Identity() string { return p.identity }

func (p reviewerAdapterProbe) FindPR(ctx context.Context, ref verify.PRRef) (verify.PRInfo, error) {
	finding, err := p.adapter.FindPR(ctx, ref.Repo, ref.Branch)
	if err != nil {
		return verify.PRInfo{}, err
	}
	return verify.PRInfo{Exists: finding.Exists, CreatedBy: finding.CreatedBy, URL: finding.URL}, nil
}

// reviewerProbeStub is the Reviewer-identity PR-existence probe seam used when
// no `reviewer` role is configured. FindPR errors rather than certifying, which
// holds the live path at needs-human by design.
type reviewerProbeStub struct {
	identity string
}

func (p reviewerProbeStub) Identity() string { return p.identity }

func (p reviewerProbeStub) FindPR(_ context.Context, _ verify.PRRef) (verify.PRInfo, error) {
	return verify.PRInfo{}, errors.New("serve: reviewer-identity PR probe is not wired (configure a `reviewer` role entry)")
}
