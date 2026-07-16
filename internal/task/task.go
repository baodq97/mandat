// Package task defines the TaskContract, the canonical work-item model every
// tracker adapter maps to and every downstream plane (orchestrator, runner,
// journal) consumes (RFC-0001 §Load-bearing contracts, spec §2). It is a pure
// core: the types plus their required-field and enum-membership validation, no
// I/O. The adapter is the only producer, and validation runs before dispatch so
// a malformed contract is a journaled skip, never a spawned run (RFC-0001 AC-09).
package task

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SchemaVersion pins the TaskContract shape. Evolution is additive only; a
// breaking change bumps this so the break is explicit and persisted contracts
// stay migratable (RFC-0001 §Impact).
const SchemaVersion = 1

// TrackerSystem is the tracker a work item originates from, carried in
// tracker_ref.system. It stays in lockstep with config.TrackerKind
// (internal/config): only azure-devops ships in the MVP skeleton, jira is the
// roadmap value (CLAUDE.md, spec §10).
type TrackerSystem string

// Enum values for TrackerSystem.
const (
	TrackerAzureDevOps TrackerSystem = "azure-devops"
	TrackerJira        TrackerSystem = "jira"
)

// Type is the work-item class that selects which RoleAgent playbook handles the
// task. dev-task is the only class the skeleton dispatches; story and prd are
// reserved for the PO/SA roles deferred out of the MVP (RFC-0001 §TaskContract).
type Type string

// Enum values for Type.
const (
	TypeDevTask Type = "dev-task"
	TypeStory   Type = "story"
	TypePRD     Type = "prd"
)

// State is the orchestrator pipeline state carried on the contract. It mirrors
// the six operational states of internal/orchestrator (RFC-0001 §Orchestrator
// state machine) and stays in lockstep with them; a new TaskContract enters at
// StateQueued. It omits orchestrator's empty StateStart, the pre-creation
// pseudo-state a persisted contract never occupies.
type State string

// Enum values for State.
const (
	StateQueued     State = "queued"
	StateInProgress State = "in-progress"
	StateInReview   State = "in-review"
	StateNeedsHuman State = "needs-human"
	StateDone       State = "done"
	StateFailed     State = "failed"
)

// TrackerRef locates the source work item. work_item_id is a string to stay
// tracker-neutral (ADO ids are numeric, Jira keys are not); the whole struct is
// persisted as JSON in tasks.tracker_ref (RFC-0001 §Journal).
type TrackerRef struct {
	System     TrackerSystem `json:"system"`
	Org        string        `json:"org"`
	Project    string        `json:"project"`
	WorkItemID string        `json:"work_item_id"`
	URL        string        `json:"url"`
}

// Remit is the mechanical edit boundary the workspace plane enforces via sparse
// checkout and the post-hoc diff-inside-remit check (spec §4.5). Paths is the
// allow-list, filled from the repo registry defaults rather than a per-work-item
// field in the skeleton (RFC-0001 decision 4).
type Remit struct {
	Repo       string   `json:"repo"`
	BaseBranch string   `json:"base_branch"`
	Paths      []string `json:"paths"`
}

// TaskContract is the canonical work-item model (spec §2). The adapter is its
// only producer; the orchestrator, runner, and journal are consumers. It is
// persisted whole as JSON in tasks.contract (RFC-0001 §Journal).
type TaskContract struct {
	ID            string     `json:"id"`
	TrackerRef    TrackerRef `json:"tracker_ref"`
	Type          Type       `json:"type"`
	Title         string     `json:"title"`
	Acceptance    string     `json:"acceptance"`
	Refs          []string   `json:"refs"`
	State         State      `json:"state"`
	Role          string     `json:"role"`
	Remit         Remit      `json:"remit"`
	AssignedTo    string     `json:"assigned_to"`
	SchemaVersion int        `json:"schema_version"`
}

// FieldError names one TaskContract field that failed validation, addressed by
// its dotted path (e.g. "remit.base_branch", "tracker_ref.system").
type FieldError struct {
	Path   string
	Reason string
}

func (e FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// ValidationErrors is the typed error Validate and Parse return for a decodable
// contract that violates the schema. It collects every violation in one pass
// (required fields and enum membership) so a producer bug surfaces as a full
// list, not a fix-one-rerun loop (mirrors config.ValidationErrors).
type ValidationErrors []FieldError

func (e ValidationErrors) Error() string {
	msgs := make([]string, len(e))
	for i, fe := range e {
		msgs[i] = fe.Error()
	}
	return "task contract: invalid: " + strings.Join(msgs, "; ")
}

// Parse decodes JSON into a TaskContract and validates it. Unknown fields are
// tolerated so an additive schema_version-1 field from a newer producer does not
// break an older consumer (RFC-0001 §Impact); structural violations return
// ValidationErrors, a JSON-syntax failure a wrapped error.
func Parse(data []byte) (*TaskContract, error) {
	var tc TaskContract
	if err := json.Unmarshal(data, &tc); err != nil {
		return nil, fmt.Errorf("task contract: parse: %w", err)
	}
	if err := tc.Validate(); err != nil {
		return nil, err
	}
	return &tc, nil
}

// Validate reports required-field and enum-membership violations against the
// RFC-0001 TaskContract field table, collecting all of them. It returns nil when
// the contract is dispatchable.
func (tc *TaskContract) Validate() error {
	var errs ValidationErrors
	add := func(path, reason string) {
		errs = append(errs, FieldError{Path: path, Reason: reason})
	}

	if tc.SchemaVersion != SchemaVersion {
		add("schema_version", fmt.Sprintf("must be %d", SchemaVersion))
	}
	if tc.ID == "" {
		add("id", "is required")
	}
	switch tc.TrackerRef.System {
	case TrackerAzureDevOps, TrackerJira:
	case "":
		add("tracker_ref.system", "is required")
	default:
		add("tracker_ref.system", fmt.Sprintf("must be one of %q, %q; got %q", TrackerAzureDevOps, TrackerJira, tc.TrackerRef.System))
	}
	if tc.TrackerRef.Org == "" {
		add("tracker_ref.org", "is required")
	}
	if tc.TrackerRef.Project == "" {
		add("tracker_ref.project", "is required")
	}
	if tc.TrackerRef.WorkItemID == "" {
		add("tracker_ref.work_item_id", "is required")
	}
	if tc.TrackerRef.URL == "" {
		add("tracker_ref.url", "is required")
	}
	switch tc.Type {
	case TypeDevTask, TypeStory, TypePRD:
	case "":
		add("type", "is required")
	default:
		add("type", fmt.Sprintf("must be one of %q, %q, %q; got %q", TypeDevTask, TypeStory, TypePRD, tc.Type))
	}
	if tc.Title == "" {
		add("title", "is required")
	}
	if tc.Acceptance == "" {
		add("acceptance", "is required")
	}
	switch tc.State {
	case StateQueued, StateInProgress, StateInReview, StateNeedsHuman, StateDone, StateFailed:
	case "":
		add("state", "is required")
	default:
		add("state", fmt.Sprintf("must be a pipeline state; got %q", tc.State))
	}
	if tc.Role == "" {
		add("role", "is required")
	}
	if tc.Remit.Repo == "" {
		add("remit.repo", "is required")
	}
	if tc.Remit.BaseBranch == "" {
		add("remit.base_branch", "is required")
	}
	if len(tc.Remit.Paths) == 0 {
		add("remit.paths", "at least one remit path is required")
	}
	for i, p := range tc.Remit.Paths {
		fieldPath := fmt.Sprintf("remit.paths[%d]", i)
		if p == "" {
			add(fieldPath, "must not be empty")
		}
		if strings.HasPrefix(p, "/") {
			add(fieldPath, "must not be an absolute path")
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				add(fieldPath, "must not contain a parent-directory (\"..\") segment")
				break
			}
		}
	}
	if tc.AssignedTo == "" {
		add("assigned_to", "is required")
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}
