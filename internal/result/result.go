// Package result defines the ResultContract, the schema-validated JSON the
// runner subprocess writes as the single source of a run's outcome; the
// supervisor reads that file and never parses stream-json prose (RFC-0001
// §Load-bearing contracts, spec §4.6, ADR-0006). It is a pure core: types plus
// validation. Reading the file is the runner's job, so Parse takes bytes and
// this package performs no I/O (US-0001).
package result

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// SchemaVersion pins the ResultContract shape; additive evolution only, a break
// bumps it (RFC-0001 §Impact).
const SchemaVersion = 1

const (
	// Path is the fixed worktree-relative file the subprocess writes and the
	// supervisor reads. The .mandat/ control directory is excluded from the
	// diff-inside-remit check (RFC-0001 §Load-bearing contracts).
	Path = ".mandat/result.json"

	// EnvVar is the environment variable that carries Path into the child
	// process (RFC-0001 §Load-bearing contracts, AC-19).
	EnvVar = "MANDAT_RESULT_PATH"
)

// Status is the single enum the orchestrator routes a finished run on. There is
// deliberately no separate needs-human boolean: the needs-human outcome derives
// from StatusNeedsHuman, so no field can disagree with the enum (RFC-0001
// §Load-bearing contracts, the ResultContract red-team fix).
type Status string

// Enum values for Status.
const (
	StatusCompleted  Status = "completed"
	StatusNeedsHuman Status = "needs_human"
	StatusFailed     Status = "failed"
)

// Artifact is one produced work product. repo and branch are always required;
// pr_url is filled once the draft PR is opened (RFC-0001 §Load-bearing
// contracts: completed requires at least one artifact carrying repo and branch).
type Artifact struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	PRURL  string `json:"pr_url,omitempty"`
}

// ResultContract is the run outcome the subprocess writes to Path.
// additionalProperties is false at every level, so an unknown field is a decode
// rejection; status completed requires at least one artifact, needs_human and
// failed require a reason (RFC-0001 §Load-bearing contracts).
type ResultContract struct {
	SchemaVersion int        `json:"schema_version"`
	TaskID        string     `json:"task_id"`
	Status        Status     `json:"status"`
	Reason        string     `json:"reason,omitempty"`
	Artifacts     []Artifact `json:"artifacts,omitempty"`
}

// FieldError names one ResultContract field that failed validation, addressed by
// its dotted path (e.g. "artifacts[0].branch").
type FieldError struct {
	Path   string
	Reason string
}

func (e FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

// ValidationErrors is the typed error Parse and Validate return for a decodable
// contract that violates the schema. Any non-nil error from Parse routes the
// task to needs-human: a missing or schema-invalid ResultContract never retries
// (RFC-0001 §Load-bearing contracts, decision 3). It collects every violation in
// one pass (mirrors config.ValidationErrors).
type ValidationErrors []FieldError

func (e ValidationErrors) Error() string {
	msgs := make([]string, len(e))
	for i, fe := range e {
		msgs[i] = fe.Error()
	}
	return "result contract: invalid: " + strings.Join(msgs, "; ")
}

// Parse decodes the bytes the subprocess wrote and validates them. Unknown
// fields are rejected (the schema's additionalProperties: false), so a decode
// failure and a schema violation both yield a non-nil error the caller treats as
// needs-human; only the schema-violation path returns ValidationErrors.
func Parse(data []byte) (*ResultContract, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var rc ResultContract
	if err := dec.Decode(&rc); err != nil {
		return nil, fmt.Errorf("result contract: parse: %w", err)
	}
	if err := rc.Validate(); err != nil {
		return nil, err
	}
	return &rc, nil
}

// Validate reports required-field, enum-membership, and status-conditional
// violations against the RFC-0001 ResultContract schema, collecting all of them.
// It returns nil when the contract is a usable outcome.
func (rc *ResultContract) Validate() error {
	var errs ValidationErrors
	add := func(path, reason string) {
		errs = append(errs, FieldError{Path: path, Reason: reason})
	}

	if rc.SchemaVersion != SchemaVersion {
		add("schema_version", fmt.Sprintf("must be %d", SchemaVersion))
	}
	if rc.TaskID == "" {
		add("task_id", "is required")
	}
	switch rc.Status {
	case StatusCompleted:
		if len(rc.Artifacts) == 0 {
			add("artifacts", `at least one artifact is required when status is "completed"`)
		}
	case StatusNeedsHuman, StatusFailed:
		if rc.Reason == "" {
			add("reason", fmt.Sprintf("is required when status is %q", rc.Status))
		}
	case "":
		add("status", "is required")
	default:
		add("status", fmt.Sprintf("must be one of %q, %q, %q; got %q", StatusCompleted, StatusNeedsHuman, StatusFailed, rc.Status))
	}
	for i, a := range rc.Artifacts {
		if a.Repo == "" {
			add(fmt.Sprintf("artifacts[%d].repo", i), "is required")
		}
		if a.Branch == "" {
			add(fmt.Sprintf("artifacts[%d].branch", i), "is required")
		}
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}
