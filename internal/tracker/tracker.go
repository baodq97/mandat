// Package tracker defines the tracker-neutral seam the pipeline ingests work
// through and writes back to (spec §4.2, RFC-0001 §Package layout). It carries
// no vendor detail: the Azure DevOps implementation lives in
// internal/adapter/azuredevops and a future Jira adapter drops in behind the
// same interface without touching any consumer (spec §4.2: "Jira later without
// touching core"). The interface is the whole package — the adapter owns every
// REST call, and the orchestrator keys off task.TaskContract, never a tracker
// type.
package tracker

import (
	"context"

	"github.com/baodq97/mandat/internal/task"
)

// Tracker is the ingestion-and-write contract every tracker adapter satisfies.
// Poll is the 30s dispatch source (RFC-0001 §Scope): it surfaces the work items
// a role's agent user is assigned and maps each to a task.TaskContract, the
// canonical model every downstream plane consumes. Comment and ApplyStatus are
// the write path back onto the source work item.
//
// workItemID is the tracker's own identifier as a string (ADO ids are numeric,
// Jira keys are not) — the same value carried in TaskContract.tracker_ref, so a
// caller holding a contract can address its source item without a tracker-typed
// handle. status is the target tracker state in that tracker's own vocabulary
// (an ADO System.State value, a Jira transition name); mapping a mandat pipeline
// state to it is the adapter's job.
type Tracker interface {
	Poll(ctx context.Context) ([]task.TaskContract, error)
	Comment(ctx context.Context, workItemID, text string) error
	ApplyStatus(ctx context.Context, workItemID, status string) error
}
