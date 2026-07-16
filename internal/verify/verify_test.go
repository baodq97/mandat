package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baodq97/mandat/internal/orchestrator"
	"github.com/baodq97/mandat/internal/task"
	"github.com/baodq97/mandat/internal/workspace"
)

// The two agent-user principals under test. They are DISTINCT on purpose: the
// PR is opened by devUser (the writer) and confirmed by reviewerUser (the
// scorer), which is writer != scorer as an IAM property (RFC-0001 §4.1).
const (
	devUser      = "agent-user-dev-01@baotest.onmicrosoft.com"
	reviewerUser = "agent-user-reviewer-01@baotest.onmicrosoft.com"
	taskID       = "ado-baodo0220-42"
	branch       = "mandat/ado-baodo0220-42"
	repo         = "mandat"
)

// fakeProbe is the §9 PR-existence double. It stands in for the Reviewer-identity
// probe: Identity reports which principal it acts as, and FindPR returns a scripted
// PRInfo while recording that it was called and with which ref, so a test can prove
// the probe did (or did not) run and that it looked the PR up by the run's branch.
type fakeProbe struct {
	identity string
	info     PRInfo
	err      error

	calls  int
	gotRef PRRef
}

func (f *fakeProbe) Identity() string { return f.identity }

func (f *fakeProbe) FindPR(_ context.Context, ref PRRef) (PRInfo, error) {
	f.calls++
	f.gotRef = ref
	return f.info, f.err
}

// fakeRemit is the §9 diff-inside-remit double. err nil means the diff stayed in
// the remit; a *workspace.RemitViolationError means it escaped. It counts calls so
// a test can prove whether (and how often) the diff check ran relative to the
// gate re-run and the probe.
type fakeRemit struct {
	err   error
	calls int
}

func (f *fakeRemit) DiffInsideRemit(_ context.Context) error {
	f.calls++
	return f.err
}

// fakeAncestry is the §9 ancestry-check double. err nil means HEAD shares a
// merge base with the base branch; a *workspace.AncestryViolationError means it
// does not. It counts calls so a test can prove whether (and how often) the
// ancestry check ran relative to the diff, the gate re-run, and the probe.
type fakeAncestry struct {
	err   error
	calls int
}

func (f *fakeAncestry) SharesMergeBase(_ context.Context) error {
	f.calls++
	return f.err
}

// orderedRemit is the diff-inside-remit double for TestVerify_DiffRunsBeforeGate.
// It writes a sentinel file when called, so a gate script run afterward can
// prove causally — not just by call count — that the diff already ran: the
// gate checks for the sentinel's existence and fails if it is missing.
type orderedRemit struct {
	sentinel string
}

func (o *orderedRemit) DiffInsideRemit(_ context.Context) error {
	return os.WriteFile(o.sentinel, []byte("diff ran"), 0o644)
}

// gateScript writes a script exiting exitCode into dir and returns the gate
// command line that runs it. Invoking it via `sh <name>` from the worktree proves
// the gate re-run happens with the worktree as its working directory (a wrong cwd
// would fail to find the script) and captures its exit code as ground truth.
func gateScript(t *testing.T, dir string, exitCode int) string {
	t.Helper()
	const name = "gate.sh"
	body := fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("write gate script: %v", err)
	}
	return "sh " + name
}

// plantResult writes a ResultContract into the worktree's control dir. The
// verifier must never read it; tests plant a LYING one to prove the verdict comes
// from re-run ground truth, not the agent's self-report.
func plantResult(t *testing.T, dir, body string) {
	t.Helper()
	p := filepath.Join(dir, ".mandat", "result.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir control dir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("plant result: %v", err)
	}
}

func baseTask() *task.TaskContract {
	return &task.TaskContract{
		ID:         taskID,
		AssignedTo: devUser,
		Remit:      task.Remit{Repo: repo, BaseBranch: "main", Paths: []string{"cmd/mandat/"}},
	}
}

func request(dir string, gates []string, remit RemitChecker, ancestry AncestryChecker) Request {
	return Request{
		Task:        baseTask(),
		WorktreeDir: dir,
		Branch:      branch,
		Gates:       gates,
		Remit:       remit,
		Ancestry:    ancestry,
	}
}

// TestVerify_ResultOK_AllThreeChecksPass is the happy path: a green gate re-run,
// an in-remit diff, and a PR present under the Dev agent user yield the
// result_ok event the orchestrator advances to in-review on (AC-20, AC-24).
func TestVerify_ResultOK_AllThreeChecksPass(t *testing.T) {
	t.Parallel()
	if reviewerUser == devUser {
		t.Fatal("test setup: reviewer and dev principals must differ")
	}

	dir := t.TempDir()
	gate := gateScript(t, dir, 0)
	remit := &fakeRemit{}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser, URL: "https://dev.azure.com/baodo0220/mandat/_git/mandat/pullrequest/7"}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Event != orchestrator.EventResultOK {
		t.Errorf("event = %q, want %q", got.Event, orchestrator.EventResultOK)
	}
	if got.Failed != "" {
		t.Errorf("Failed = %q, want empty on result_ok", got.Failed)
	}
	if len(got.Gates) != 1 || got.Gates[0].Command != gate || got.Gates[0].ExitCode != 0 {
		t.Errorf("Gates = %+v, want one green %q", got.Gates, gate)
	}
	if !got.PR.Exists || got.PR.CreatedBy != devUser {
		t.Errorf("PR = %+v, want present and created by the dev agent user", got.PR)
	}
	if remit.calls != 1 {
		t.Errorf("diff-inside-remit calls = %d, want 1", remit.calls)
	}
	if probe.calls != 1 {
		t.Errorf("probe calls = %d, want 1", probe.calls)
	}
	if probe.gotRef != (PRRef{Repo: repo, Branch: branch}) {
		t.Errorf("probe ref = %+v, want the run's repo and branch", probe.gotRef)
	}

	// runs.gate_result shape: command list, per-command exit codes (AC-25).
	raw, err := json.Marshal(got.Gates)
	if err != nil {
		t.Fatalf("marshal gate result: %v", err)
	}
	if !strings.Contains(string(raw), `"exit_code":0`) || !strings.Contains(string(raw), `"command":`) {
		t.Errorf("gate_result JSON = %s, want command list with per-command exit codes", raw)
	}
}

// TestVerify_GateRed drives the gate_red outcome: a gate that exits non-zero
// holds the run for a human, names the failing command and its exit code, and
// stops before the probe. The diff-inside-remit check already ran (and passed)
// before the gate re-run started — order is diff -> gate -> probe — so
// remit.calls is 1; only the probe never runs (AC-24).
func TestVerify_GateRed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gate := gateScript(t, dir, 1)
	remit := &fakeRemit{}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Event != orchestrator.EventGateRed {
		t.Errorf("event = %q, want %q", got.Event, orchestrator.EventGateRed)
	}
	if got.Failed != CheckGate {
		t.Errorf("Failed = %q, want %q", got.Failed, CheckGate)
	}
	if len(got.Gates) != 1 || got.Gates[0].ExitCode != 1 {
		t.Errorf("Gates = %+v, want the failing command with exit code 1", got.Gates)
	}
	if !strings.Contains(got.Detail, "1") {
		t.Errorf("Detail = %q, want it to name the exit code", got.Detail)
	}
	if remit.calls != 1 {
		t.Errorf("diff-inside-remit calls = %d, want 1 (diff runs before the gate)", remit.calls)
	}
	// A red gate short-circuits before the probe.
	if probe.calls != 0 {
		t.Errorf("probe calls = %d, want 0 (gate short-circuits before the probe)", probe.calls)
	}
}

// TestVerify_RemitViolation drives the remit_violation outcome: an out-of-remit
// diff holds the run for a human, names the escaping path, and stops before the
// gate re-run and the probe even start. The diff runs first precisely so a
// gate's own side effects (e.g., a test run's __pycache__) can never be misread
// as the agent's escape — so the gates never ran and Gates is empty (AC-17).
func TestVerify_RemitViolation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gate := gateScript(t, dir, 0)
	remit := &fakeRemit{err: &workspace.RemitViolationError{Path: "secrets/leak.txt"}}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Event != orchestrator.EventRemitViolation {
		t.Errorf("event = %q, want %q", got.Event, orchestrator.EventRemitViolation)
	}
	if got.Failed != CheckDiff {
		t.Errorf("Failed = %q, want %q", got.Failed, CheckDiff)
	}
	if !strings.Contains(got.Detail, "secrets/leak.txt") {
		t.Errorf("Detail = %q, want it to name the out-of-remit path", got.Detail)
	}
	if remit.calls != 1 {
		t.Errorf("diff-inside-remit calls = %d, want 1", remit.calls)
	}
	if len(got.Gates) != 0 {
		t.Errorf("Gates = %+v, want empty: the gate re-run never starts once the diff has already failed", got.Gates)
	}
	if probe.calls != 0 {
		t.Errorf("probe calls = %d, want 0 (remit violation short-circuits before the gate and the probe)", probe.calls)
	}
}

// TestVerify_AncestryViolation drives the ancestry-check failure: an orphan or
// unrelated-history branch holds the run for a human on the reused
// remit_violation event (no new orchestrator event) and stops before the diff,
// the gate re-run, and the probe even start.
func TestVerify_AncestryViolation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gate := gateScript(t, dir, 0)
	ancestry := &fakeAncestry{err: &workspace.AncestryViolationError{Branch: branch, Base: "main"}}
	remit := &fakeRemit{}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, ancestry))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Event != orchestrator.EventRemitViolation {
		t.Errorf("event = %q, want %q", got.Event, orchestrator.EventRemitViolation)
	}
	if got.Failed != CheckAncestry {
		t.Errorf("Failed = %q, want %q", got.Failed, CheckAncestry)
	}
	if !strings.Contains(got.Detail, branch) || !strings.Contains(got.Detail, "main") {
		t.Errorf("Detail = %q, want it to name the branch and the base branch", got.Detail)
	}
	if ancestry.calls != 1 {
		t.Errorf("ancestry calls = %d, want 1", ancestry.calls)
	}
	if remit.calls != 0 {
		t.Errorf("diff-inside-remit calls = %d, want 0 (ancestry short-circuits before the diff)", remit.calls)
	}
	if len(got.Gates) != 0 {
		t.Errorf("Gates = %+v, want empty: the gate re-run never starts once ancestry has already failed", got.Gates)
	}
	if probe.calls != 0 {
		t.Errorf("probe calls = %d, want 0 (ancestry violation short-circuits before the gate and the probe)", probe.calls)
	}
}

// TestVerify_DiffRunsBeforeGate proves the diff-before-gate invariant causally,
// not just by call count: the gate command is a script that checks for a
// sentinel file, which the diff-inside-remit double writes when it runs. If the
// gate ever ran before the diff, the sentinel would be missing and the script
// would exit non-zero — turning a regressed ordering into a gate_red rather than
// a silently-wrong result_ok.
func TestVerify_DiffRunsBeforeGate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sentinel := filepath.Join(dir, "diff-ran-first")
	const script = "gate-order-check.sh"
	body := fmt.Sprintf("#!/bin/sh\ntest -f %q && exit 0\nexit 9\n", sentinel)
	if err := os.WriteFile(filepath.Join(dir, script), []byte(body), 0o755); err != nil {
		t.Fatalf("write gate order-check script: %v", err)
	}
	gate := "sh " + script

	remit := &orderedRemit{sentinel: sentinel}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Event != orchestrator.EventResultOK {
		t.Errorf("event = %q, want %q: the gate must observe the diff's sentinel, proving the diff ran first", got.Event, orchestrator.EventResultOK)
	}
	if len(got.Gates) != 1 || got.Gates[0].ExitCode != 0 {
		t.Errorf("Gates = %+v, want the order-check gate to pass", got.Gates)
	}
}

// TestVerify_ProbeFailed drives the probe_failed outcome under both grounds
// RFC-0001 AC-27 names: the Reviewer-identity probe finds no PR, or finds a PR
// whose createdBy is not the Dev agent user.
func TestVerify_ProbeFailed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		info PRInfo
	}{
		{"no_pr", PRInfo{Exists: false}},
		{"wrong_creator", PRInfo{Exists: true, CreatedBy: "some-other-user@baotest.onmicrosoft.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			gate := gateScript(t, dir, 0)
			remit := &fakeRemit{}
			probe := &fakeProbe{identity: reviewerUser, info: tc.info}

			got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if got.Event != orchestrator.EventProbeFailed {
				t.Errorf("event = %q, want %q", got.Event, orchestrator.EventProbeFailed)
			}
			if got.Failed != CheckProbe {
				t.Errorf("Failed = %q, want %q", got.Failed, CheckProbe)
			}
			if probe.calls != 1 {
				t.Errorf("probe calls = %d, want 1", probe.calls)
			}
		})
	}
}

// TestVerify_ProbeTransportErrorAfterGreenGates closes the AC-25 observability
// gap: when the probe fails as an operational (transport) error after the gate
// re-run already went green, Verify's error is non-nil AND the returned Verdict
// still carries the gate results collected before the failure, so the caller can
// journal the green gates instead of silently dropping them.
func TestVerify_ProbeTransportErrorAfterGreenGates(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gate1 := gateScript(t, dir, 0)
	failName := "gate2.sh"
	if err := os.WriteFile(filepath.Join(dir, failName), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write second gate: %v", err)
	}
	gate2 := "sh " + failName
	remit := &fakeRemit{}
	probe := &fakeProbe{identity: reviewerUser, err: errors.New("transport: connection reset by peer")}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate1, gate2}, remit, &fakeAncestry{}))
	if err == nil {
		t.Fatal("Verify() error = nil, want the probe's transport error")
	}
	if len(got.Gates) != 2 || got.Gates[0].ExitCode != 0 || got.Gates[1].ExitCode != 0 {
		t.Errorf("Gates = %+v, want both green gate results carried alongside the operational error", got.Gates)
	}
}

// TestVerify_DiffCheckTransportError proves the converse: when the operational
// error happens in the diff-inside-remit check, before the gate re-run has even
// started, Gates stays empty — there is nothing to carry.
func TestVerify_DiffCheckTransportError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gate := gateScript(t, dir, 0)
	remit := &fakeRemit{err: errors.New("transport: diff-check connection reset")}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
	if err == nil {
		t.Fatal("Verify() error = nil, want the diff-check's transport error")
	}
	if len(got.Gates) != 0 {
		t.Errorf("Gates = %+v, want empty: the gate re-run never started", got.Gates)
	}
}

// TestVerify_WriterMustDifferFromScorer proves writer != scorer is enforced, not
// merely documented: a probe whose identity equals the Dev agent user is refused
// before any check runs, so the writer can never confirm its own PR.
func TestVerify_WriterMustDifferFromScorer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gate := gateScript(t, dir, 0)
	remit := &fakeRemit{}
	// The probe acts as the SAME principal that opened the PR — not a distinct
	// scorer. Everything else is green, so only the identity guard can hold it.
	probe := &fakeProbe{identity: devUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
	if err == nil {
		t.Fatalf("Verify() error = nil, want a writer==scorer refusal; verdict = %+v", got)
	}
	if !strings.Contains(err.Error(), "writer must differ from scorer") {
		t.Errorf("error = %v, want the writer != scorer refusal", err)
	}
	if probe.calls != 0 {
		t.Errorf("probe calls = %d, want 0 (refused before probing)", probe.calls)
	}
	if remit.calls != 0 {
		t.Errorf("diff-inside-remit calls = %d, want 0 (refused before any check)", remit.calls)
	}
}

// TestVerify_IgnoresResultContractSelfReport is the load-bearing proof that the
// verifier re-runs ground truth and never trusts the agent's self-report
// (ADR-0003, AC-23). Each case plants a ResultContract in the worktree that
// DISAGREES with ground truth; the verdict must follow ground truth, not the file.
func TestVerify_IgnoresResultContractSelfReport(t *testing.T) {
	t.Parallel()

	t.Run("self_reported_failure_is_ignored_when_truth_is_green", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		// The agent claims it failed, yet every ground-truth check is green.
		plantResult(t, dir, `{"schema_version":1,"task_id":"ado-baodo0220-42","status":"failed","reason":"agent self-reports failure"}`)
		gate := gateScript(t, dir, 0)
		remit := &fakeRemit{}
		probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

		got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if got.Event != orchestrator.EventResultOK {
			t.Errorf("event = %q, want %q: the self-reported failure must be ignored", got.Event, orchestrator.EventResultOK)
		}
	})

	t.Run("self_reported_success_is_ignored_when_truth_is_red", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		// The agent claims a completed run with an opened PR, yet the gate is red.
		plantResult(t, dir, `{"schema_version":1,"task_id":"ado-baodo0220-42","status":"completed","artifacts":[{"repo":"mandat","branch":"mandat/ado-baodo0220-42","pr_url":"https://dev.azure.com/baodo0220/mandat/_git/mandat/pullrequest/7"}]}`)
		gate := gateScript(t, dir, 1)
		remit := &fakeRemit{}
		probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

		got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit, &fakeAncestry{}))
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if got.Event != orchestrator.EventGateRed {
			t.Errorf("event = %q, want %q: the self-reported success must not override a red gate", got.Event, orchestrator.EventGateRed)
		}
	})
}

// TestVerify_MultipleGatesAllRun confirms the whole configured list is re-run when
// each passes and every command's exit code lands in the result (AC-25), and that
// a red gate stops the list at the first failure.
func TestVerify_MultipleGatesAllRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pass := gateScript(t, dir, 0)
	// A second, distinct failing script so the list has two commands.
	failName := "gate2.sh"
	if err := os.WriteFile(filepath.Join(dir, failName), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatalf("write second gate: %v", err)
	}
	fail := "sh " + failName

	remit := &fakeRemit{}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{pass, fail}, remit, &fakeAncestry{}))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if got.Event != orchestrator.EventGateRed {
		t.Errorf("event = %q, want %q", got.Event, orchestrator.EventGateRed)
	}
	if len(got.Gates) != 2 {
		t.Fatalf("Gates = %+v, want both commands recorded up to the failure", got.Gates)
	}
	if got.Gates[0].ExitCode != 0 || got.Gates[1].ExitCode != 3 {
		t.Errorf("gate exit codes = [%d %d], want [0 3]", got.Gates[0].ExitCode, got.Gates[1].ExitCode)
	}
}

// TestVerify_RequestValidation rejects a request missing a required input before
// spending any subprocess or probe call.
func TestVerify_RequestValidation(t *testing.T) {
	t.Parallel()

	cases := map[string]Request{
		"no_task":     {WorktreeDir: t.TempDir(), Remit: &fakeRemit{}, Ancestry: &fakeAncestry{}},
		"no_worktree": {Task: baseTask(), Remit: &fakeRemit{}, Ancestry: &fakeAncestry{}},
		"no_remit":    {Task: baseTask(), WorktreeDir: t.TempDir(), Ancestry: &fakeAncestry{}},
		"no_ancestry": {Task: baseTask(), WorktreeDir: t.TempDir(), Remit: &fakeRemit{}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			probe := &fakeProbe{identity: reviewerUser}
			if _, err := New(probe).Verify(context.Background(), req); err == nil {
				t.Errorf("Verify() error = nil, want a validation error for %q", name)
			}
		})
	}
}
