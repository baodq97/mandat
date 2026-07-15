package verify

import (
	"context"
	"encoding/json"
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
// a test can prove the diff check ran only after the gates passed.
type fakeRemit struct {
	err   error
	calls int
}

func (f *fakeRemit) DiffInsideRemit(_ context.Context) error {
	f.calls++
	return f.err
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

func request(dir string, gates []string, remit RemitChecker) Request {
	return Request{
		Task:        baseTask(),
		WorktreeDir: dir,
		Branch:      branch,
		Gates:       gates,
		Remit:       remit,
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

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit))
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

// TestVerify_GateRed drives the gate_red outcome: a gate that exits non-zero holds
// the run for a human, names the failing command and its exit code, and stops
// before the diff and probe run (order: gate -> diff -> probe, AC-24).
func TestVerify_GateRed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gate := gateScript(t, dir, 1)
	remit := &fakeRemit{}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit))
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
	// A red gate short-circuits: the cheaper-truth checks after it never ran.
	if remit.calls != 0 {
		t.Errorf("diff-inside-remit calls = %d, want 0 (gate short-circuits)", remit.calls)
	}
	if probe.calls != 0 {
		t.Errorf("probe calls = %d, want 0 (gate short-circuits)", probe.calls)
	}
}

// TestVerify_RemitViolation drives the remit_violation outcome: with the gates
// green, an out-of-remit diff holds the run for a human, names the escaping path,
// and stops before the probe. The gate results are still recorded (they ran green
// first) for runs.gate_result (AC-17, AC-25).
func TestVerify_RemitViolation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gate := gateScript(t, dir, 0)
	remit := &fakeRemit{err: &workspace.RemitViolationError{Path: "secrets/leak.txt"}}
	probe := &fakeProbe{identity: reviewerUser, info: PRInfo{Exists: true, CreatedBy: devUser}}

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit))
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
	if probe.calls != 0 {
		t.Errorf("probe calls = %d, want 0 (remit violation short-circuits before the probe)", probe.calls)
	}
	if len(got.Gates) != 1 || got.Gates[0].ExitCode != 0 {
		t.Errorf("Gates = %+v, want the green gate recorded despite the later diff failure", got.Gates)
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

			got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit))
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

	got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit))
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

		got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit))
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

		got, err := New(probe).Verify(context.Background(), request(dir, []string{gate}, remit))
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

	got, err := New(probe).Verify(context.Background(), request(dir, []string{pass, fail}, remit))
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
		"no_task":     {WorktreeDir: t.TempDir(), Remit: &fakeRemit{}},
		"no_worktree": {Task: baseTask(), Remit: &fakeRemit{}},
		"no_remit":    {Task: baseTask(), WorktreeDir: t.TempDir()},
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
