package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/baodq97/mandat/internal/adapter/azuredevops"
	"github.com/baodq97/mandat/internal/config"
	"github.com/baodq97/mandat/internal/identity"
	"github.com/baodq97/mandat/internal/journal"
	"github.com/baodq97/mandat/internal/orchestrator"
	"github.com/baodq97/mandat/internal/result"
	"github.com/baodq97/mandat/internal/role"
	"github.com/baodq97/mandat/internal/runner"
	"github.com/baodq97/mandat/internal/task"
	"github.com/baodq97/mandat/internal/verify"
	"github.com/baodq97/mandat/internal/workspace"
)

// The two distinct agent-user principals the skeleton composes: the PR is opened by
// devUser (the writer) and confirmed by reviewerUser (the scorer), which is
// writer != scorer as an IAM property (RFC-0001 §4.1). The org/project fix the
// task id the adapter derives (ado-<org>-<id>) so the fake claude's ResultContract
// carries a matching task_id.
const (
	devUser         = "agent-user-dev-01@baotest.onmicrosoft.com"
	reviewerUser    = "agent-user-reviewer-01@baotest.onmicrosoft.com"
	skeletonOrg     = "baodo0220"
	skeletonProject = "mandat"
	skeletonPRURL   = "https://dev.azure.com/baodo0220/mandat/_git/mandat/pullrequest/7"
)

// completedResult is the exact ResultContract the fake claude writes on the happy
// path; the assertion compares the journaled raw bytes against it, so both sides
// share one source (mirrors the runner package's §9 fake-claude discipline).
const completedResult = `{"schema_version":1,"task_id":"ado-baodo0220-42","status":"completed","artifacts":[{"repo":"mandat","branch":"mandat/ado-baodo0220-42","pr_url":"https://dev.azure.com/baodo0220/mandat/_git/mandat/pullrequest/7"}]}`

// workItem42Body is the recorded-ADO work-item fixture: assigned to the Dev agent
// user, tagged for the in-registry `mandat` repo (RFC-0001 §9 recorded fixture).
const workItem42Body = `{
  "id": 42,
  "fields": {
    "System.Title": "Wire the version subcommand end to end",
    "System.State": "Active",
    "System.Tags": "repo:mandat",
    "Microsoft.VSTS.Common.AcceptanceCriteria": "mandat version prints the build version and exits 0",
    "System.AssignedTo": { "uniqueName": "agent-user-dev-01@baotest.onmicrosoft.com" }
  },
  "url": "https://example.test/_apis/wit/workItems/42"
}`

// pullRequest7Body is the recorded 201 draft-PR response; createdBy is the Dev agent
// user, the directory fact a PR opened under the delegated token carries (spike S3).
const pullRequest7Body = `{
  "pullRequestId": 7,
  "isDraft": true,
  "url": "https://dev.azure.com/baodo0220/mandat/_apis/git/repositories/mandat/pullRequests/7",
  "createdBy": { "uniqueName": "agent-user-dev-01@baotest.onmicrosoft.com" }
}`

// TestWalkingSkeleton_HappyPath is the load-bearing proof that the twelve planes
// compose. It wires runTask with the three §9 doubles — a recorded-ADO fixture
// (httptest) for poll and CreatePR, a local bare git origin for the workspace, and a
// fake claude for the runner — plus a fake identity token source, a fake
// Reviewer-identity probe, and a temp-file journal. One polled TaskContract runs to
// in-review: the journal reconstructs queued -> in-progress -> in-review, a draft PR
// is opened under the Dev agent user, and the ResultContract is validated.
func TestWalkingSkeleton_HappyPath(t *testing.T) {
	deps, ado, tc := newSkeleton(t, "completed")
	ctx := context.Background()

	state, err := runTask(ctx, deps, tc)
	if err != nil {
		t.Fatalf("runTask() error = %v", err)
	}
	if state != orchestrator.StateInReview {
		t.Fatalf("final state = %q, want %q", state, orchestrator.StateInReview)
	}

	events, err := deps.Store.Events(ctx, tc.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}

	// The transition rows reconstruct queued -> in-progress -> in-review with no
	// needs-human hold (RFC-0001 AC-28, composed at the cmd-wiring seam).
	wantStates := []string{"queued", "in-progress", "in-review"}
	if got := transitionStates(events); !slices.Equal(got, wantStates) {
		t.Errorf("transition to_states = %v, want %v", got, wantStates)
	}
	// dispatch leaves the pre-creation pseudo-state with an empty from_state (AC-05).
	if d := findAct(events, "dispatch"); d == nil || d.FromState != "" || d.ToState != "queued" {
		t.Errorf("dispatch row = %+v, want empty from_state and to_state queued", d)
	}

	// The draft PR was opened under the Dev agent user (the writer side of
	// writer != scorer) and a real CreatePR call reached the recorded ADO fixture.
	pr := findAct(events, actPROpened)
	if pr == nil || pr.ActingIdentity != devUser {
		t.Fatalf("pr_opened row = %+v, want acting identity %q", pr, devUser)
	}
	if !strings.Contains(string(pr.Detail), devUser) {
		t.Errorf("pr_opened detail = %s, want createdBy = the Dev agent user", pr.Detail)
	}
	if !strings.Contains(string(pr.Detail), "_git/mandat/pullrequest/7") {
		t.Errorf("pr_opened detail = %s, want the human web URL, not the API self-link", pr.Detail)
	}
	if !ado.prCreated() {
		t.Error("no draft-PR POST reached the recorded ADO fixture")
	}

	// The gate re-run and the PR-existence probe ran under the Reviewer identity, not
	// the Dev identity (writer != scorer as an IAM property, AC-27's fixture half).
	if gr := findAct(events, actGateRerun); gr == nil || gr.ActingIdentity != reviewerUser {
		t.Errorf("gate_rerun row = %+v, want acting identity %q", gr, reviewerUser)
	}
	if pe := findAct(events, actProbePRExists); pe == nil || pe.ActingIdentity != reviewerUser {
		t.Errorf("probe_pr_exists row = %+v, want acting identity %q", pe, reviewerUser)
	}

	// The ResultContract file was read, schema-validated, and persisted verbatim.
	runID := runIDFromEvents(events)
	if runID == "" {
		t.Fatal("no journal row carried a run id")
	}
	results, err := deps.Store.Results(ctx, runID)
	if err != nil {
		t.Fatalf("Results() error = %v", err)
	}
	if len(results) != 1 || !results[0].Valid || string(results[0].Raw) != completedResult {
		t.Errorf("results = %+v, want one valid row carrying the completed ResultContract", results)
	}

	// The completed run's row carries the stream telemetry (RFC-0001 AC-10.2).
	run, err := deps.Store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun() error = %v", err)
	}
	if run.TotalCostUSD <= 0 || !strings.Contains(string(run.Usage), "input_tokens") {
		t.Errorf("run cost/usage = %v / %s, want the stream's telemetry", run.TotalCostUSD, run.Usage)
	}

	// Tracker lifecycle feedback (US-0011): the work item moved to the configured
	// in-progress state before the runner spawned, and the source work item got a
	// dispatch comment naming the task and role plus a comment carrying the PR URL.
	if statuses := ado.statusCalls(); len(statuses) != 1 || !strings.Contains(statuses[0], "Doing") {
		t.Errorf("ApplyStatus calls = %v, want exactly one call setting %q", statuses, "Doing")
	}
	comments := ado.commentCalls()
	if len(comments) != 2 {
		t.Fatalf("Comment calls = %v, want 2 (dispatch + PR opened)", comments)
	}
	if !strings.Contains(comments[0], tc.ID) || !strings.Contains(comments[0], "dev") {
		t.Errorf("dispatch comment = %q, want it to name task %s and role %q", comments[0], tc.ID, "dev")
	}
	if !strings.Contains(comments[1], "_git/mandat/pullrequest/7") {
		t.Errorf("PR comment = %q, want it to carry the created PR's human web URL", comments[1])
	}
}

// TestWalkingSkeleton_NeedsHumanHold is the deterministic-edge variant: the fake
// claude withholds the ResultContract, so the composed pipeline routes the task to
// needs-human with no PR advance and no crash (RFC-0001 AC-21 / AC-10.4).
func TestWalkingSkeleton_NeedsHumanHold(t *testing.T) {
	deps, ado, tc := newSkeleton(t, "no_file")
	ctx := context.Background()

	state, err := runTask(ctx, deps, tc)
	if err != nil {
		t.Fatalf("runTask() error = %v", err)
	}
	if state != orchestrator.StateNeedsHuman {
		t.Fatalf("final state = %q, want %q", state, orchestrator.StateNeedsHuman)
	}

	events, err := deps.Store.Events(ctx, tc.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	// The missing result routes in-progress -> needs-human (result_invalid); the task
	// never reaches in-review.
	if ri := findAct(events, string(orchestrator.EventResultInvalid)); ri == nil || ri.ToState != string(orchestrator.StateNeedsHuman) {
		t.Errorf("result_invalid row = %+v, want to_state needs-human", ri)
	}
	if findAct(events, string(orchestrator.EventResultOK)) != nil {
		t.Error("a result_ok transition was journaled for an invalid run")
	}

	// No PR advance: the composition opens no PR for a run the file did not vouch for.
	if findAct(events, actPROpened) != nil {
		t.Error("pr_opened was journaled for an invalid run")
	}
	if ado.prCreated() {
		t.Error("a draft-PR POST reached ADO for an invalid run; needs-human must not advance")
	}

	// The raw (empty) result and valid=0 still landed in results (RFC-0001 AC-21).
	runID := runIDFromEvents(events)
	results, err := deps.Store.Results(ctx, runID)
	if err != nil {
		t.Fatalf("Results() error = %v", err)
	}
	if len(results) != 1 || results[0].Valid || len(results[0].Raw) != 0 {
		t.Errorf("results = %+v, want one row with valid=0 and empty raw", results)
	}

	// Tracker lifecycle feedback still ran (dispatch), and the hold posts a
	// comment naming the reason since the run produced no ResultContract (US-0011).
	comments := ado.commentCalls()
	if len(comments) != 2 {
		t.Fatalf("Comment calls = %v, want 2 (dispatch + needs-human hold)", comments)
	}
	if !strings.Contains(comments[1], "held task") || !strings.Contains(comments[1], "ResultContract") {
		t.Errorf("hold comment = %q, want it to name the held task and the runner reason", comments[1])
	}
}

// TestWalkingSkeleton_TrackerFeedbackBestEffort proves the best-effort invariant
// (US-0011): the source work item's state PATCH and comment POST both 500, yet
// the pipeline still reaches the same terminal outcome as the happy path — the
// journal, not the tracker, is the pipeline's own source of truth, so a tracker
// write failure logs a warning and never holds up or diverts the run.
func TestWalkingSkeleton_TrackerFeedbackBestEffort(t *testing.T) {
	deps, ado, tc := newSkeleton(t, "completed")
	ado.setFailTrackerWrites(true)
	ctx := context.Background()

	state, err := runTask(ctx, deps, tc)
	if err != nil {
		t.Fatalf("runTask() error = %v", err)
	}
	if state != orchestrator.StateInReview {
		t.Fatalf("final state = %q, want %q (tracker-write failures must not change the pipeline outcome)", state, orchestrator.StateInReview)
	}

	events, err := deps.Store.Events(ctx, tc.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	// Same transition sequence as the happy path: a 500'd tracker write never
	// journals a different event or holds the task for a human.
	wantStates := []string{"queued", "in-progress", "in-review"}
	if got := transitionStates(events); !slices.Equal(got, wantStates) {
		t.Errorf("transition to_states = %v, want %v", got, wantStates)
	}

	// The draft PR still opened even though the tracker writes around it 500'd.
	if pr := findAct(events, actPROpened); pr == nil || pr.ActingIdentity != devUser {
		t.Fatalf("pr_opened row = %+v, want acting identity %q", pr, devUser)
	}
	if !ado.prCreated() {
		t.Error("no draft-PR POST reached the recorded ADO fixture")
	}

	// The pipeline still attempted both tracker writes; the fixture just 500'd them.
	if statuses := ado.statusCalls(); len(statuses) != 1 {
		t.Errorf("ApplyStatus calls = %v, want exactly 1 attempted despite the 500", statuses)
	}
	if comments := ado.commentCalls(); len(comments) != 2 {
		t.Errorf("Comment calls = %v, want 2 attempted (dispatch + PR opened) despite the 500s", comments)
	}
}

// TestSelectSpawner_PilotEscapeHatch proves the pilot toggle: MANDAT_DIRECT_SPAWN
// (non-empty) swaps in the same-user DirectSpawner for pilot/dev VMs without root,
// and its absence keeps the OS-user isolation DefaultSpawner (spec §4.5). The two
// spawners are comparable empty-struct singletons, so the interface values compare
// by identity.
func TestSelectSpawner_PilotEscapeHatch(t *testing.T) {
	t.Run("direct when MANDAT_DIRECT_SPAWN set", func(t *testing.T) {
		t.Setenv("MANDAT_DIRECT_SPAWN", "1")
		if got := selectSpawner(); got != workspace.DirectSpawner {
			t.Errorf("selectSpawner() = %T, want workspace.DirectSpawner", got)
		}
	})
	t.Run("default OS-user isolation otherwise", func(t *testing.T) {
		t.Setenv("MANDAT_DIRECT_SPAWN", "")
		if got := selectSpawner(); got != workspace.DefaultSpawner {
			t.Errorf("selectSpawner() = %T, want workspace.DefaultSpawner", got)
		}
	})
}

// TestReviewerIdentity_ReturnsAgentUserNameNotID proves reviewerIdentity reads
// the reviewer role's AgentUserName, not its AgentUserID. verify.Verify compares
// the probe's returned identity against TaskContract.AssignedTo, which ADO
// populates as a UPN (System.AssignedTo.uniqueName) — wiring the object id here
// would make that comparison vacuous and never certify a real PR.
func TestReviewerIdentity_ReturnsAgentUserNameNotID(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Roles: map[string]config.RoleConfig{
			"reviewer": {
				AgentUserID:   "11111111-1111-1111-1111-111111111111",
				AgentUserName: reviewerUser,
			},
		},
	}
	if got := reviewerIdentity(cfg); got != reviewerUser {
		t.Errorf("reviewerIdentity() = %q, want the reviewer role's AgentUserName %q", got, reviewerUser)
	}
}

// TestBuildReviewerProbe_NoReviewerRoleReturnsStub proves the fail-closed default:
// with no `reviewer` role configured, buildReviewerProbe returns the stub whose
// FindPR always errors, holding a live run at needs-human rather than certifying
// an unprobed PR (RFC-0001 AC-27).
func TestBuildReviewerProbe_NoReviewerRoleReturnsStub(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{}

	probe, err := buildReviewerProbe(cfg, nil, reviewerUser)
	if err != nil {
		t.Fatalf("buildReviewerProbe() error = %v, want nil", err)
	}
	if probe.Identity() != reviewerUser {
		t.Errorf("probe.Identity() = %q, want %q", probe.Identity(), reviewerUser)
	}
	if _, err := probe.FindPR(context.Background(), verify.PRRef{Repo: "mandat", Branch: "mandat/task-42"}); err == nil {
		t.Error("probe.FindPR() error = nil, want the stub's always-error with no reviewer role configured")
	}
}

// TestBuildReviewerProbe_ReviewerRoleReturnsRealProbe proves the live-wiring
// branch: a configured `reviewer` role yields a probe carrying that role's own
// identity, the second azuredevops.Adapter instance that mints Reviewer tokens
// distinct from the Dev agent user (writer != scorer, RFC-0001 §4.1).
func TestBuildReviewerProbe_ReviewerRoleReturnsRealProbe(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Tracker: config.TrackerConfig{Org: skeletonOrg, Project: skeletonProject},
		Roles: map[string]config.RoleConfig{
			"reviewer": {AgentUserName: reviewerUser},
		},
	}
	// The broker is never minted from in this test (only Identity() is asserted),
	// so a throwaway secret credential is enough to satisfy buildReviewerProbe's
	// *identity.Broker parameter.
	broker := identity.NewBroker(&config.Config{}, identity.NewSecretCredential("unused"), identity.AzureDevOpsResource)

	probe, err := buildReviewerProbe(cfg, broker, reviewerUser)
	if err != nil {
		t.Fatalf("buildReviewerProbe() error = %v, want nil", err)
	}
	if probe.Identity() != reviewerUser {
		t.Errorf("probe.Identity() = %q, want the reviewer role's identity %q", probe.Identity(), reviewerUser)
	}
}

// newSkeleton composes runTask's dependencies from the §9 doubles and polls the one
// fixture work item, returning the wired deps, the ADO fixture (for PR-call
// assertions), and the polled TaskContract.
func newSkeleton(t *testing.T, scenario string) (serveDeps, *fakeADO, task.TaskContract) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required to exercise the workspace plane")
	}
	ctx := context.Background()

	origin := newBareOrigin(t)
	ado := newFakeADO(t)
	registry := &config.Config{
		Repos: map[string]config.RepoConfig{
			"mandat": {
				URL:        origin,
				BaseBranch: "main",
				Paths:      []string{"cmd/mandat/", "internal/buildinfo/"},
				Gates:      []string{"true"},
			},
		},
	}
	adapter, err := azuredevops.New(azuredevops.Config{
		BaseURL:       ado.srv.URL,
		Org:           skeletonOrg,
		Project:       skeletonProject,
		Role:          "dev",
		AgentUserName: devUser,
		Tokens:        &fakeTokenProvider{token: "fake-delegated-token"},
		Remits:        registry,
	})
	if err != nil {
		t.Fatalf("azuredevops.New() error = %v", err)
	}

	store := openStore(t)
	mirror := filepath.Join(t.TempDir(), "mirror.git")

	devRole := role.Role{
		Name:            "dev",
		Mandate:         role.MandateRef{AgentIdentityID: "11111111-1111-1111-1111-111111111111", AgentUserID: devUser},
		Playbook:        "/etc/mandat/playbooks/dev.md",
		AutonomyCeiling: config.CeilingDraftPR,
		ModelTier:       config.ModelSonnet,
	}

	deps := serveDeps{
		Store:            store,
		Tracker:          adapter,
		Forge:            adapter,
		Runner:           runner.New(store, &fakeClaudeSpawner{scenario: scenario}, runner.Config{ClaudePath: os.Args[0], MaxBudgetUSD: 5}),
		Verifier:         verify.New(&fakeProbe{identity: reviewerUser, info: verify.PRInfo{Exists: true, CreatedBy: devUser, URL: skeletonPRURL}}),
		Provision:        workspace.Provision,
		Role:             devRole,
		ReviewerIdentity: reviewerUser,
		InProgressState:  "Doing",
		RepoURL:          func(repo string) (string, error) { return registry.Repos[repo].URL, nil },
		Gates:            func(repo string) []string { return registry.Repos[repo].Gates },
		MirrorDir:        func(string) string { return mirror },
		TasksRoot:        t.TempDir(),
		RoleUser:         "mandat-dev",
		Home:             t.TempDir(),
		ConfigDir:        t.TempDir(),
		HarnessVersion:   "test-harness",
	}

	contracts, err := adapter.Poll(ctx)
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if len(contracts) != 1 {
		t.Fatalf("Poll() returned %d contracts, want exactly 1", len(contracts))
	}
	return deps, ado, contracts[0]
}

// newSkeletonWithReviewerAdapter mirrors newSkeleton but wires the Verifier's
// probe through a second, real azuredevops.Adapter under Role="reviewer" that
// calls FindPR against the same fakeADO fixture (AC-27's live wiring), rather
// than the fakeProbe double newSkeleton injects. The fixture's GET pullrequests
// answer is scripted via ado.setFindPRResult (defaulting to the PR the fixture's
// POST pullrequests handler reports, under the Dev agent user), so a test can
// drive both the happy path and a createdBy mismatch through the real
// adapter-and-verifier composition.
func newSkeletonWithReviewerAdapter(t *testing.T, scenario string) (serveDeps, *fakeADO, task.TaskContract) {
	t.Helper()
	deps, ado, tc := newSkeleton(t, scenario)

	reviewerAdapter, err := azuredevops.New(azuredevops.Config{
		BaseURL:       ado.srv.URL,
		Org:           skeletonOrg,
		Project:       skeletonProject,
		Role:          "reviewer",
		AgentUserName: reviewerUser,
		Tokens:        &fakeTokenProvider{token: "fake-delegated-reviewer-token"},
		Remits:        &config.Config{},
	})
	if err != nil {
		t.Fatalf("azuredevops.New() reviewer adapter error = %v", err)
	}
	deps.Verifier = verify.New(reviewerAdapterProbe{adapter: reviewerAdapter, identity: reviewerUser})
	return deps, ado, tc
}

// TestWalkingSkeleton_ReviewerProbe_HappyPath wires the real FindPR probe end to
// end (RFC-0001 AC-27, US-0007): with a reviewer-role adapter in place of the
// fakeProbe double, the probe's GET pullrequests call reaches the same
// recorded-ADO fixture the Dev adapter opened the PR against, under the
// Reviewer role's own token, and the run still reaches in-review.
func TestWalkingSkeleton_ReviewerProbe_HappyPath(t *testing.T) {
	deps, _, tc := newSkeletonWithReviewerAdapter(t, "completed")
	ctx := context.Background()

	state, err := runTask(ctx, deps, tc)
	if err != nil {
		t.Fatalf("runTask() error = %v", err)
	}
	if state != orchestrator.StateInReview {
		t.Fatalf("final state = %q, want %q", state, orchestrator.StateInReview)
	}

	events, err := deps.Store.Events(ctx, tc.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	if pe := findAct(events, actProbePRExists); pe == nil || pe.ActingIdentity != reviewerUser {
		t.Errorf("probe_pr_exists row = %+v, want acting identity %q", pe, reviewerUser)
	}
}

// TestWalkingSkeleton_ReviewerProbe_CreatedByMismatchHolds proves the probe's
// createdBy check fails closed through the real FindPR path: when the fixture's
// GET pullrequests reports a createdBy that is not the Dev agent user, the run
// holds at needs-human via probe_failed even though the runner reported
// completed and the gate re-run and diff-inside-remit both passed.
func TestWalkingSkeleton_ReviewerProbe_CreatedByMismatchHolds(t *testing.T) {
	deps, ado, tc := newSkeletonWithReviewerAdapter(t, "completed")
	ado.setFindPRResult(true, "someone-else@baotest.onmicrosoft.com")
	ctx := context.Background()

	state, err := runTask(ctx, deps, tc)
	if err != nil {
		t.Fatalf("runTask() error = %v", err)
	}
	if state != orchestrator.StateNeedsHuman {
		t.Fatalf("final state = %q, want %q", state, orchestrator.StateNeedsHuman)
	}

	events, err := deps.Store.Events(ctx, tc.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	pf := findAct(events, string(orchestrator.EventProbeFailed))
	if pf == nil || pf.ToState != string(orchestrator.StateNeedsHuman) {
		t.Errorf("probe_failed row = %+v, want to_state needs-human", pf)
	}
	if !strings.Contains(string(pf.Detail), "createdBy") {
		t.Errorf("probe_failed detail = %s, want it to name the createdBy mismatch", pf.Detail)
	}
	if findAct(events, actProbePRExists) != nil {
		t.Error("probe_pr_exists was journaled for a createdBy mismatch")
	}
}

// TestWalkingSkeleton_VerifyOperationalErrorHolds proves the silent-hold gap is
// closed: when the probe's FindPR fails as a transport error (distinct from a
// failed-check Verdict), the task must not stay in-progress forever with no
// journal act and no tracker feedback. It must journal a verify_error act
// carrying the error text, transition to needs-human, and attempt a hold
// comment — the same shape the setup-failed path uses. It must also journal a
// gate_rerun act carrying the green gates the verifier collected before the
// probe's transport error (AC-25): the observability gap this closes is that a
// later check's operational error must not drop gates that DID run.
func TestWalkingSkeleton_VerifyOperationalErrorHolds(t *testing.T) {
	deps, ado, tc := newSkeleton(t, "completed")
	deps.Verifier = verify.New(&fakeProbe{identity: reviewerUser, err: errors.New("transport: connection reset by peer")})
	ctx := context.Background()

	state, err := runTask(ctx, deps, tc)
	if err != nil {
		t.Fatalf("runTask() error = %v", err)
	}
	if state != orchestrator.StateNeedsHuman {
		t.Fatalf("final state = %q, want %q", state, orchestrator.StateNeedsHuman)
	}

	events, err := deps.Store.Events(ctx, tc.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	gr := findAct(events, actGateRerun)
	if gr == nil || gr.ActingIdentity != reviewerUser {
		t.Fatalf("gate_rerun row = %+v, want acting identity %q", gr, reviewerUser)
	}
	if !strings.Contains(string(gr.Detail), `"command":"true"`) || !strings.Contains(string(gr.Detail), `"exit_code":0`) {
		t.Errorf("gate_rerun detail = %s, want the green gate the verifier collected before the probe error", gr.Detail)
	}

	ve := findAct(events, actVerifyError)
	if ve == nil {
		t.Fatal("verify_error act was not journaled")
	}
	if !strings.Contains(string(ve.Detail), "transport: connection reset by peer") {
		t.Errorf("verify_error detail = %s, want it to carry the probe's error text", ve.Detail)
	}
	if gr.Seq >= ve.Seq {
		t.Errorf("gate_rerun seq %d, verify_error seq %d, want gate_rerun to journal first (mirrors the success path)", gr.Seq, ve.Seq)
	}

	ri := findAct(events, string(orchestrator.EventResultInvalid))
	if ri == nil || ri.ToState != string(orchestrator.StateNeedsHuman) {
		t.Errorf("result_invalid row = %+v, want to_state needs-human", ri)
	}

	comments := ado.commentCalls()
	if len(comments) != 3 {
		t.Fatalf("Comment calls = %v, want 3 (dispatch + PR opened + verify-error hold)", comments)
	}
	if !strings.Contains(comments[2], "held task") || !strings.Contains(comments[2], "transport: connection reset by peer") {
		t.Errorf("hold comment = %q, want it to name the held task and the verify error", comments[2])
	}
}

func openStore(t *testing.T) *journal.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mandat.db")
	s, err := journal.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("journal.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func transitionStates(events []journal.JournalEvent) []string {
	var out []string
	for i := range events {
		if events[i].ToState != "" {
			out = append(out, events[i].ToState)
		}
	}
	return out
}

func findAct(events []journal.JournalEvent, act string) *journal.JournalEvent {
	for i := range events {
		if events[i].Act == act {
			return &events[i]
		}
	}
	return nil
}

func runIDFromEvents(events []journal.JournalEvent) string {
	for i := range events {
		if events[i].RunID != "" {
			return events[i].RunID
		}
	}
	return ""
}

// fakeADO is the recorded-ADO double: it replays canned WIQL, work-item, and draft-PR
// responses so no test dials dev.azure.com, and records whether a PR POST arrived so
// the needs-human variant can prove the pipeline never advanced. It also records
// every ApplyStatus and Comment call so the tracker-feedback tests (US-0011) can
// assert on the writes serve makes back onto the source work item.
type fakeADO struct {
	srv               *httptest.Server
	mu                sync.Mutex
	pr                bool
	statuses          []string
	comments          []string
	failTrackerWrites bool
	findPRExists      bool
	findPRCreatedBy   string
}

func newFakeADO(t *testing.T) *fakeADO {
	t.Helper()
	f := &fakeADO{
		// Defaults match the happy path: the PR the fixture's POST pullrequests
		// handler reports as opened, under the same Dev agent user, so a test that
		// wires the real reviewer probe without scripting a mismatch still finds it.
		findPRExists:    true,
		findPRCreatedBy: devUser,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeADO) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_apis/wit/wiql"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"queryType":"flat","workItems":[{"id":42,"url":"https://example.test/_apis/wit/workItems/42"}]}`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_apis/wit/workitems/42"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(workItem42Body))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pullrequests"):
		f.mu.Lock()
		f.pr = true
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(pullRequest7Body))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pullrequests"):
		f.mu.Lock()
		exists, createdBy := f.findPRExists, f.findPRCreatedBy
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !exists {
			_, _ = w.Write([]byte(`{"count":0,"value":[]}`))
			return
		}
		fmt.Fprintf(w, `{"count":1,"value":[{"pullRequestId":7,"createdBy":{"uniqueName":%q}}]}`, createdBy)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/_apis/wit/workitems/42"):
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.statuses = append(f.statuses, string(body))
		fail := f.failTrackerWrites
		f.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/workitems/42/comments"):
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.comments = append(f.comments, string(body))
		fail := f.failTrackerWrites
		f.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeADO) prCreated() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pr
}

func (f *fakeADO) statusCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statuses...)
}

func (f *fakeADO) commentCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.comments...)
}

// setFailTrackerWrites flips the work-item state PATCH and comment POST endpoints
// to a 500, so a test can exercise the tracker-feedback best-effort path (US-0011)
// without touching the poll/work-item-read/PR endpoints the same fixture serves.
func (f *fakeADO) setFailTrackerWrites(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failTrackerWrites = fail
}

// setFindPRResult scripts the GET pullrequests fixture's answer, so a test can
// drive the real FindPR probe path to either a found PR (with a chosen
// createdBy) or an empty result.
func (f *fakeADO) setFindPRResult(exists bool, createdBy string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findPRExists = exists
	f.findPRCreatedBy = createdBy
}

// fakeTokenProvider is the injected identity seam: a fixed token, no live mint, so
// the adapter's Bearer auth is exercised without Entra (RFC-0001 §9, AC-15).
type fakeTokenProvider struct {
	token string
}

func (f *fakeTokenProvider) Token(_ context.Context, _ string) (string, error) {
	return f.token, nil
}

// fakeProbe stands in for the Reviewer-identity PR-existence probe: it reports the
// principal it acts as (distinct from the Dev agent user) and a scripted finding, so
// the verifier's writer != scorer certification runs without a second live agent.
// A non-nil err scripts a transport failure instead of a finding, standing in for
// the FindPR probe erroring out rather than returning a Verdict.
type fakeProbe struct {
	identity string
	info     verify.PRInfo
	err      error
}

func (f *fakeProbe) Identity() string { return f.identity }

func (f *fakeProbe) FindPR(_ context.Context, _ verify.PRRef) (verify.PRInfo, error) {
	if f.err != nil {
		return verify.PRInfo{}, f.err
	}
	return f.info, nil
}

// fakeClaudeSpawner is the direct-exec Spawner double: instead of dropping to a
// per-role OS user via systemd (needs root, absent on CI), it re-execs this test
// binary as the fake claude (TestHelperProcess), passing the supervisor's argv after
// `--`, exactly as the runner package's own §9 fake does.
type fakeClaudeSpawner struct {
	scenario string
}

func (f *fakeClaudeSpawner) Spawn(ctx context.Context, spec workspace.SpawnSpec) error {
	args := append([]string{"-test.run=TestHelperProcess", "--"}, spec.Argv...)
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(append([]string(nil), spec.Env...),
		"GO_WANT_HELPER_PROCESS=1",
		"MANDAT_FAKE_SCENARIO="+f.scenario,
	)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	return cmd.Run()
}

// TestHelperProcess is the §9 fake claude. Under GO_WANT_HELPER_PROCESS it acts as a
// scripted `claude`: it emits stream-json telemetry to stdout and then, per
// MANDAT_FAKE_SCENARIO, writes (completed) or withholds (no_file) the ResultContract.
// Both scenarios print the SAME success stream, so any outcome difference proves the
// file — not stdout — is the contract (ADR-0006). It is not a real test.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := helperArgs()
	session := flagValue(args, "--session-id")
	if session == "" {
		session = flagValue(args, "--resume")
	}
	emitSuccessStream(session)

	switch os.Getenv("MANDAT_FAKE_SCENARIO") {
	case "completed":
		editInRemit()
		writeResultFile(completedResult)
	case "no_file":
		// stdout above already claimed success and this prose doubles down; the file
		// is absent, so the outcome must be result_invalid.
		fmt.Fprintln(os.Stdout, `{"type":"assistant","message":{"content":"All done — draft PR opened."}}`)
	}
	os.Exit(0)
}

func emitSuccessStream(session string) {
	fmt.Fprintf(os.Stdout, `{"type":"system","subtype":"init","session_id":%q,"model":"claude-sonnet","tools":[]}`+"\n", session)
	fmt.Fprintf(os.Stdout, `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"num_turns":3,"total_cost_usd":0.4212,"usage":{"input_tokens":1200,"output_tokens":340,"cache_read_input_tokens":800},"session_id":%q}`+"\n", session)
}

// editInRemit makes a scripted change inside the remit so the diff-inside-remit check
// sees a real in-remit edit (RFC-0001 AC-16). cwd is the worktree and cmd/mandat/ is
// in the remit; an edit failure is left to the vacuous-pass path rather than aborting
// the fake.
func editInRemit() {
	f, err := os.OpenFile(filepath.Join("cmd", "mandat", "main.go"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString("\n// edited inside the remit by the fake claude\n")
	_ = f.Close()
}

func writeResultFile(content string) {
	path := os.Getenv(result.EnvVar)
	if path == "" {
		fmt.Fprintln(os.Stderr, "fake claude: no "+result.EnvVar)
		os.Exit(3)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "fake claude: mkdir:", err)
		os.Exit(3)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake claude: write:", err)
		os.Exit(3)
	}
}

func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func flagValue(argv []string, name string) string {
	for i := range len(argv) - 1 {
		if argv[i] == name {
			return argv[i+1]
		}
	}
	return ""
}

// newBareOrigin builds the §9 local bare git origin with a known tree: two in-remit
// paths (cmd/mandat, internal/buildinfo) plus an out-of-remit README, so the sparse
// checkout scoped to the remit provably omits the latter.
func newBareOrigin(t *testing.T) string {
	t.Helper()

	work := t.TempDir()
	gitRun(t, work, "init", "-b", "main")
	writeFile(t, filepath.Join(work, "cmd/mandat/main.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(work, "internal/buildinfo/build.go"), "package buildinfo\n")
	writeFile(t, filepath.Join(work, "README.md"), "# out of remit\n")
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-m", "seed")

	origin := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, "", "clone", "--bare", work, origin)
	return origin
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=mandat-test",
		"GIT_AUTHOR_EMAIL=test@mandat.invalid",
		"GIT_COMMITTER_NAME=mandat-test",
		"GIT_COMMITTER_EMAIL=test@mandat.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}
