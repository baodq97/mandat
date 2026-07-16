// Package verify is the verification plane: the ground-truth scorer that decides
// whether a completed run may advance to in-review (RFC-0001 §Definition of done,
// AC-23..AC-27, §Verification). It turns a finished run's worktree and its
// TaskContract into one orchestrator.Event by checking three independent grounds
// of truth in the verifier's OWN context — the diff-inside-remit check, the gate
// re-run, then the PR-existence probe, in that order so a gate's own side effects
// (build artifacts a re-run writes into the worktree) can never be misread as the
// agent's out-of-remit escape — and yields the result_ok-eligible event only when
// all three pass.
//
// Two RFC-0001 invariants (§4.7, US-0007) shape this package and are worth
// stating because the code cannot show them on its own:
//
//   - Re-run ground truth, never trust the agent summary (RFC-0001 §4.7,
//     AC-23). The verifier's input is the worktree and the task, never a
//     ResultContract: this package does not import internal/result and Verify
//     takes no result argument, so a compromised or buggy agent's self-reported
//     test status cannot reach the verdict. The gates run as fresh subprocesses
//     in the verifier's trusted process, the diff is computed from git, and the
//     PR is confirmed by an out-of-band probe — three facts the agent cannot
//     fabricate into this plane.
//   - Writer != scorer is an IAM property, not a convention (RFC-0001 §4.1,
//     §4.7, AC-27). The PR-existence probe authenticates as the Reviewer agent
//     user, a principal distinct from the Dev agent user that opened the PR.
//     The distinctness is enforced structurally: Verify refuses to certify when
//     the probe's identity equals the task's Dev agent user, so a misconfigured
//     scorer that is also the writer fails closed rather than rubber-stamping.
//
// The Verifier derives the event and returns it in a Verdict; it never calls
// orchestrator.Next itself. Feeding the state machine and journaling the
// transition (and writing Verdict.Gates to runs.gate_result) belong to the
// caller that composes this plane with the runner.
package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/baodq97/mandat/internal/orchestrator"
	"github.com/baodq97/mandat/internal/task"
	"github.com/baodq97/mandat/internal/workspace"
)

// Check names the ground-truth check that produced a non-result_ok verdict, for
// the journal detail. It stays "" on a result_ok verdict.
type Check string

// Enum values for Check, one per verification ground of truth.
const (
	CheckGate  Check = "gate"
	CheckDiff  Check = "diff"
	CheckProbe Check = "probe"
)

// GateResult is one re-run gate command and the exit code the verifier observed.
// The slice of these is the shape RFC-0001 §runs stores in runs.gate_result
// ("JSON: command list, per-command exit codes", AC-25); the JSON tags fix that
// wire shape.
type GateResult struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
}

// PRRef locates the pull request the probe looks up: the repo it targets and the
// source branch the run opened it from. It carries no expected creator — the
// probe reports the PR's actual createdBy and the Verifier owns the policy that
// it must equal the Dev agent user.
type PRRef struct {
	Repo   string
	Branch string
}

// PRInfo is the probe's finding. Exists is false when no PR was found for the
// ref; CreatedBy is the principal the tracker records as the PR's creator, which
// the Verifier requires to equal the Dev agent user (AC-27).
type PRInfo struct {
	Exists    bool
	CreatedBy string
	URL       string
}

// PRProbe is the out-of-band PR-existence probe (RFC-0001 AC-27). An
// implementation authenticates as the Reviewer agent user — a principal DISTINCT
// from the Dev agent user that opened the PR — so the confirmation is a scorer's
// read, not the writer's self-report (writer != scorer as an IAM property). The
// real ADO implementation carries the Reviewer credential; provisioning that
// second agent user is an E2E concern, and tests inject a fake standing in for
// it. Identity returns the principal the probe acts as, which Verify checks is
// not the Dev agent user before it trusts the probe's answer.
type PRProbe interface {
	Identity() string
	FindPR(ctx context.Context, ref PRRef) (PRInfo, error)
}

// RemitChecker is the post-hoc diff-inside-remit check (US-0005): it reports a
// *workspace.RemitViolationError when the run's diff touches a path outside the
// remit and nil otherwise. *workspace.Workspace satisfies it in production; tests
// inject a fake so the diff outcome can be scripted independently of a live
// worktree.
type RemitChecker interface {
	DiffInsideRemit(ctx context.Context) error
}

// Request is one completed run to verify. It is deliberately the worktree and the
// task, never a ResultContract: the verifier re-runs ground truth and never reads
// the agent's self-report (ADR-0003).
type Request struct {
	// Task carries the ground-truth expectations: Remit.Repo names the repo the
	// PR targets, and AssignedTo is the Dev agent user the PR's createdBy must
	// equal (consent = assigned_to == dev agent user, RFC-0001 §TaskContract).
	Task *task.TaskContract

	// WorktreeDir is the provisioned worktree the completed run edited. The gate
	// re-run executes there so it sees exactly the agent's changes.
	WorktreeDir string

	// Branch is the source branch the run opened the PR from
	// (workspace.Workspace.Branch); the probe looks the PR up by it.
	Branch string

	// Gates is the per-repo gate command list (config.RepoConfig.Gates for the
	// task's repo; the dogfood target's list is `make check` then `npx govkit
	// check`, RFC-0001 §Gate re-run). An empty list has no command to fail and so
	// passes vacuously — configuring the gates is the config plane's concern.
	Gates []string

	// Remit is the diff-inside-remit check for this run's worktree
	// (*workspace.Workspace in production).
	Remit RemitChecker
}

func (r Request) validate() error {
	switch {
	case r.Task == nil:
		return errors.New("verify: request has no task")
	case r.WorktreeDir == "":
		return errors.New("verify: request has no worktree dir")
	case r.Remit == nil:
		return errors.New("verify: request has no diff-inside-remit checker")
	default:
		return nil
	}
}

// Verdict is the verification outcome: the derived event the caller feeds to
// orchestrator.Next plus the structured record the journal needs.
type Verdict struct {
	// Event is orchestrator.EventResultOK only when all three checks pass;
	// otherwise it is the first failing check's event (gate_red, remit_violation,
	// or probe_failed).
	Event orchestrator.Event

	// Failed is the check that produced a non-result_ok event, "" on result_ok.
	// Detail names the specifics for the journal (the failing command and its
	// exit code, the out-of-remit path, or the probe mismatch).
	Failed Check
	Detail string

	// Gates is the per-command gate re-run result for runs.gate_result (AC-25).
	// It is populated once the gate re-run has started — gate_red and every
	// verdict after it (a later probe failure, or result_ok) — but stays empty on
	// remit_violation, since the diff-inside-remit check now runs before the
	// gates and short-circuits before they start.
	//
	// Gates also carries whatever results were collected when Verify returns a
	// non-nil operational error (AC-25): non-empty when the error surfaces after
	// the gate re-run started (e.g. the probe's transport fails once gates are
	// green), empty when the failure precedes it (e.g. a diff-check transport
	// error). The caller journals these partial results so gates that DID run are
	// never dropped just because a later check errored.
	Gates []GateResult

	// PR is the probe's finding, populated once the probe ran (gates green and
	// diff in remit). It is the zero PRInfo on an earlier failure.
	PR PRInfo
}

// Verifier scores completed runs against ground truth. It holds the installation-
// scoped Reviewer-identity probe (mirroring how the runner holds its spawner) and
// verifies one task per Verify call.
type Verifier struct {
	probe PRProbe
}

// New builds a Verifier bound to the Reviewer-identity PR probe. probe must be a
// distinct principal from every role's Dev agent user; Verify enforces that per
// task and fails closed otherwise.
func New(probe PRProbe) *Verifier {
	return &Verifier{probe: probe}
}

// Verify runs the three ground-truth checks in order and returns the derived
// event. A non-nil error is an operational failure (a gate that could not be
// launched, a diff or probe transport error) distinct from a failed check, which
// is reported as a Verdict with the matching event. Order is diff-inside-remit,
// then gate re-run, then probe, short-circuiting on the first failure; all three
// must pass to return EventResultOK. The diff runs first, on the worktree exactly
// as the agent left it, so gate side effects (e.g., a test run's __pycache__)
// exist only after the diff has already been read and can never register as an
// out-of-remit escape.
//
// On an operational error, the returned Verdict is not the zero value: it
// carries whatever Gates results the gate re-run had already collected (AC-25),
// so a later transport error (e.g. the PR probe) never drops gates that DID run.
// See Verdict.Gates.
func (v *Verifier) Verify(ctx context.Context, req Request) (Verdict, error) {
	if err := req.validate(); err != nil {
		return Verdict{}, err
	}

	// Writer != scorer, enforced before any check: a probe that shares the Dev
	// agent user's identity is not an independent scorer, so the verifier refuses
	// to certify rather than let the writer confirm its own PR (RFC-0001 §4.1).
	if v.probe == nil {
		return Verdict{}, errors.New("verify: no PR probe configured")
	}
	if id := v.probe.Identity(); id == req.Task.AssignedTo {
		return Verdict{}, fmt.Errorf("verify: probe identity %q equals the Dev agent user; writer must differ from scorer (RFC-0001 §4.1, §4.7)", id)
	}

	if err := req.Remit.DiffInsideRemit(ctx); err != nil {
		var rv *workspace.RemitViolationError
		if errors.As(err, &rv) {
			return Verdict{
				Event:  orchestrator.EventRemitViolation,
				Failed: CheckDiff,
				Detail: fmt.Sprintf("change to %q is outside the remit", rv.Path),
			}, nil
		}
		return Verdict{}, fmt.Errorf("verify: diff-inside-remit check: %w", err)
	}

	gates, event, detail, err := v.rerunGates(ctx, req)
	if err != nil {
		return Verdict{Gates: gates}, err
	}
	if event != "" {
		return Verdict{Event: event, Failed: CheckGate, Detail: detail, Gates: gates}, nil
	}

	info, err := v.probe.FindPR(ctx, PRRef{Repo: req.Task.Remit.Repo, Branch: req.Branch})
	if err != nil {
		return Verdict{Gates: gates}, fmt.Errorf("verify: pr probe: %w", err)
	}
	switch {
	case !info.Exists:
		return Verdict{
			Event:  orchestrator.EventProbeFailed,
			Failed: CheckProbe,
			Detail: "Reviewer-identity probe found no PR for the run's branch",
			Gates:  gates,
			PR:     info,
		}, nil
	case info.CreatedBy != req.Task.AssignedTo:
		return Verdict{
			Event:  orchestrator.EventProbeFailed,
			Failed: CheckProbe,
			Detail: fmt.Sprintf("PR createdBy %q is not the Dev agent user %q", info.CreatedBy, req.Task.AssignedTo),
			Gates:  gates,
			PR:     info,
		}, nil
	}

	return Verdict{Event: orchestrator.EventResultOK, Gates: gates, PR: info}, nil
}

// rerunGates executes the configured gate command list in the worktree and stops
// at the first non-zero exit. It returns the per-command results collected so far,
// EventGateRed with a naming detail on a red gate (AC-24), or a zero event when
// every gate passed. An operational error (a gate that could not be launched at
// all) is returned separately from a red exit code.
func (v *Verifier) rerunGates(ctx context.Context, req Request) ([]GateResult, orchestrator.Event, string, error) {
	results := make([]GateResult, 0, len(req.Gates))
	for _, command := range req.Gates {
		code, err := runGate(ctx, req.WorktreeDir, command)
		if err != nil {
			return results, "", "", err
		}
		results = append(results, GateResult{Command: command, ExitCode: code})
		if code != 0 {
			return results, orchestrator.EventGateRed, fmt.Sprintf("gate %q exited %d", command, code), nil
		}
	}
	return results, "", "", nil
}

// runGate re-runs one gate command line and returns its exit code. Each gate is
// the repo's own CI command line (e.g. "make check"), run through the system
// shell so PATH resolution and shell operators match how CI runs it (the gate-
// config format is an open question, RFC-0001; this is the skeleton's
// interpretation). It runs in the verifier's OWN trusted process — not the agent
// sandbox — so it inherits the verifier's environment to resolve tools on PATH,
// which is exactly the "ground truth in its own context" property (AC-23). A
// non-zero exit is returned as its code (a red gate, not an error); only a
// failure to launch the shell at all is an error.
func runGate(ctx context.Context, dir, command string) (int, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return -1, fmt.Errorf("verify: gate %q could not run: %w", command, err)
	}
	return 0, nil
}
