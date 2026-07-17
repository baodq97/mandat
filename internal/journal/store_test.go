package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/baodq97/mandat/internal/result"
	"github.com/baodq97/mandat/internal/task"
)

// openStore opens a Store on a fresh temp-file DB (spec §6: one file; tests use
// a temp path rather than /var/lib/mandat/) and closes it at test end.
func openStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mandat.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%q) error = %v, want nil", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return s
}

// devTask builds a fully populated TaskContract for the round-trip tests,
// mirroring the fixture shape internal/task uses.
func devTask() *task.TaskContract {
	return &task.TaskContract{
		ID: "ado-contoso-42",
		TrackerRef: task.TrackerRef{
			System:     task.TrackerAzureDevOps,
			Org:        "contoso",
			Project:    "mandat-dogfood",
			WorkItemID: "42",
			URL:        "https://dev.azure.com/contoso/mandat-dogfood/_workitems/edit/42",
		},
		Type:          task.TypeDevTask,
		Title:         "Add the version subcommand",
		Acceptance:    "mandat version prints the build version and exits 0",
		Refs:          []string{},
		State:         task.StateQueued,
		Role:          "dev",
		Remit:         task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}},
		AssignedTo:    "agent-user-dev-01@baotest.onmicrosoft.com",
		SchemaVersion: task.SchemaVersion,
	}
}

// TestOpen_CreatesSchema proves the migration creates the four tables and both
// append-only triggers and that the file is in WAL mode (RFC-0001 §Journal:
// WAL, one file).
func TestOpen_CreatesSchema(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	ctx := context.Background()

	for _, name := range []string{"tasks", "runs", "results", "journal"} {
		var got string
		err := s.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
		if err != nil {
			t.Fatalf("table %q not created: %v", name, err)
		}
	}
	for _, name := range []string{"journal_no_update", "journal_no_delete"} {
		var got string
		err := s.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='trigger' AND name=?`, name).Scan(&got)
		if err != nil {
			t.Fatalf("trigger %q not created: %v", name, err)
		}
	}

	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

// TestOpen_Idempotent proves the migration is additive and re-runnable: opening
// the same file twice does not fail on already-present tables or triggers.
func TestOpen_Idempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "mandat.db")
	ctx := context.Background()

	s1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open error = %v, want nil", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
	}

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("re-Open error = %v, want nil (migration must be idempotent)", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
}

// TestAppendEvent_RoundTrips is AC-3.1: a dispatch row lands with the right
// acting identity, empty from_state, to_state=queued, a DB-assigned seq, and a
// UTC ts.
func TestAppendEvent_RoundTrips(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	ctx := context.Background()
	before := time.Now().Add(-time.Second)

	if err := s.AppendEvent(ctx, JournalEvent{
		TaskID:         "task-1",
		ActingIdentity: "system:orchestrator",
		Act:            "dispatch",
		FromState:      "",
		ToState:        "queued",
	}); err != nil {
		t.Fatalf("AppendEvent error = %v, want nil", err)
	}

	got, err := s.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("Events len = %d, want 1", len(got))
	}
	e := got[0]
	if e.ActingIdentity != "system:orchestrator" {
		t.Errorf("ActingIdentity = %q, want %q", e.ActingIdentity, "system:orchestrator")
	}
	if e.Act != "dispatch" {
		t.Errorf("Act = %q, want %q", e.Act, "dispatch")
	}
	if e.FromState != "" {
		t.Errorf("FromState = %q, want empty", e.FromState)
	}
	if e.ToState != "queued" {
		t.Errorf("ToState = %q, want %q", e.ToState, "queued")
	}
	if e.Seq <= 0 {
		t.Errorf("Seq = %d, want a DB-assigned value > 0", e.Seq)
	}
	if e.Ts.IsZero() || e.Ts.Before(before) {
		t.Errorf("Ts = %v, want a recent timestamp after %v", e.Ts, before)
	}
	if e.Ts.Location() != time.UTC {
		t.Errorf("Ts location = %v, want UTC", e.Ts.Location())
	}
}

// TestJournal_AppendOnly is AC-3.5: the store exposes no update or delete path,
// and a raw UPDATE and a raw DELETE (bypassing the Go API entirely) are both
// rejected by the trigger, leaving the row intact.
func TestJournal_AppendOnly(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	ctx := context.Background()

	if err := s.AppendEvent(ctx, JournalEvent{
		TaskID:         "task-1",
		ActingIdentity: "system:orchestrator",
		Act:            "dispatch",
		ToState:        "queued",
	}); err != nil {
		t.Fatalf("AppendEvent error = %v, want nil", err)
	}

	// Reach past the Go API to the raw handle: even direct SQL cannot mutate a
	// journal row, because the trigger aborts the statement.
	if _, err := s.db.ExecContext(ctx, `UPDATE journal SET act = 'tampered' WHERE seq = 1`); err == nil {
		t.Error("raw UPDATE on journal: error = nil, want rejection by the append-only trigger")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM journal WHERE seq = 1`); err == nil {
		t.Error("raw DELETE on journal: error = nil, want rejection by the append-only trigger")
	}

	got, err := s.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("Events len = %d, want 1 (the row must survive both rejected mutations)", len(got))
	}
	if got[0].Act != "dispatch" {
		t.Errorf("Act = %q, want %q (unchanged)", got[0].Act, "dispatch")
	}
}

// TestRun_RoundTrips is AC-3.2 and AC-3.6: gate_result JSON, plus
// total_cost_usd, usage, num_turns, and is_error from the terminal result
// event, persist in runs.
func TestRun_RoundTrips(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	ctx := context.Background()

	gate := []byte(`{"commands":["make check","npx govkit check"],"exit_codes":[0,0]}`)
	usage := []byte(`{"input_tokens":1200,"output_tokens":800}`)
	started := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	ended := started.Add(3 * time.Minute)

	want := Run{
		RunID:          "run-1",
		TaskID:         "task-1",
		SessionID:      "11111111-1111-1111-1111-111111111111",
		ActingIdentity: "agent-user-dev-01",
		Model:          "sonnet",
		StartedAt:      started,
		EndedAt:        ended,
		TotalCostUSD:   1.23,
		Usage:          usage,
		NumTurns:       7,
		IsError:        false,
		ExitCode:       0,
		GateResult:     gate,
		HarnessVersion: "h1",
		ConfigVersion:  "c1",
	}
	if err := s.RecordRun(ctx, want); err != nil {
		t.Fatalf("RecordRun error = %v, want nil", err)
	}

	got, err := s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun error = %v, want nil", err)
	}
	if got.SessionID != want.SessionID {
		t.Errorf("SessionID = %q, want %q", got.SessionID, want.SessionID)
	}
	if got.ActingIdentity != want.ActingIdentity {
		t.Errorf("ActingIdentity = %q, want %q", got.ActingIdentity, want.ActingIdentity)
	}
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
	if got.TotalCostUSD != want.TotalCostUSD {
		t.Errorf("TotalCostUSD = %v, want %v", got.TotalCostUSD, want.TotalCostUSD)
	}
	if got.NumTurns != want.NumTurns {
		t.Errorf("NumTurns = %d, want %d", got.NumTurns, want.NumTurns)
	}
	if got.IsError != want.IsError {
		t.Errorf("IsError = %v, want %v", got.IsError, want.IsError)
	}
	if got.ExitCode != want.ExitCode {
		t.Errorf("ExitCode = %d, want %d", got.ExitCode, want.ExitCode)
	}
	if !bytes.Equal(got.Usage, want.Usage) {
		t.Errorf("Usage = %s, want %s", got.Usage, want.Usage)
	}
	if !bytes.Equal(got.GateResult, want.GateResult) {
		t.Errorf("GateResult = %s, want %s", got.GateResult, want.GateResult)
	}
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	if !got.EndedAt.Equal(want.EndedAt) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, want.EndedAt)
	}
}

// TestResult_RoundTrips is AC-3.3: a valid ResultContract's raw bytes persist
// with valid=1, and a schema-invalid contract still persists its raw bytes with
// valid=0. The validity is grounded in internal/result.Parse, the same schema
// the verification plane uses.
func TestResult_RoundTrips(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	ctx := context.Background()

	validContract := result.ResultContract{
		SchemaVersion: result.SchemaVersion,
		TaskID:        "task-1",
		Status:        result.StatusCompleted,
		Artifacts:     []result.Artifact{{Repo: "mandat", Branch: "feat/version", PRURL: "https://dev.azure.com/contoso/_git/mandat/pullrequest/1"}},
	}
	validRaw, err := json.Marshal(validContract)
	if err != nil {
		t.Fatalf("marshal valid ResultContract: %v", err)
	}
	if _, err := result.Parse(validRaw); err != nil {
		t.Fatalf("fixture is not a valid ResultContract: %v", err)
	}

	// Truncated JSON missing the required status: a schema-invalid contract
	// whose raw bytes must still be recorded (RFC-0001 AC-21).
	invalidRaw := []byte(`{"schema_version":1,"task_id":"task-1"`)
	if _, err := result.Parse(invalidRaw); err == nil {
		t.Fatal("fixture must be an invalid ResultContract")
	}

	if err := s.StoreResult(ctx, Result{RunID: "run-1", TaskID: "task-1", Raw: validRaw, Valid: true}); err != nil {
		t.Fatalf("StoreResult (valid) error = %v, want nil", err)
	}
	if err := s.StoreResult(ctx, Result{RunID: "run-2", TaskID: "task-1", Raw: invalidRaw, Valid: false}); err != nil {
		t.Fatalf("StoreResult (invalid) error = %v, want nil", err)
	}

	gotValid, err := s.Results(ctx, "run-1")
	if err != nil {
		t.Fatalf("Results(run-1) error = %v, want nil", err)
	}
	if len(gotValid) != 1 {
		t.Fatalf("Results(run-1) len = %d, want 1", len(gotValid))
	}
	if !bytes.Equal(gotValid[0].Raw, validRaw) {
		t.Errorf("valid Raw = %s, want %s", gotValid[0].Raw, validRaw)
	}
	if !gotValid[0].Valid {
		t.Error("valid result: Valid = false, want true")
	}

	gotInvalid, err := s.Results(ctx, "run-2")
	if err != nil {
		t.Fatalf("Results(run-2) error = %v, want nil", err)
	}
	if len(gotInvalid) != 1 {
		t.Fatalf("Results(run-2) len = %d, want 1", len(gotInvalid))
	}
	if !bytes.Equal(gotInvalid[0].Raw, invalidRaw) {
		t.Errorf("invalid Raw = %s, want %s (exact bytes must survive)", gotInvalid[0].Raw, invalidRaw)
	}
	if gotInvalid[0].Valid {
		t.Error("invalid result: Valid = true, want false")
	}
}

// TestTask_RoundTrips proves a stored TaskContract loads back identical.
func TestTask_RoundTrips(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	ctx := context.Background()

	want := devTask()
	if err := s.UpsertTask(ctx, want); err != nil {
		t.Fatalf("UpsertTask error = %v, want nil", err)
	}

	got, err := s.LoadTask(ctx, want.ID)
	if err != nil {
		t.Fatalf("LoadTask error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadTask =\n%+v\nwant\n%+v", got, want)
	}
}

// TestUpsertTask_UpdatesOnConflict proves upsert overwrites state on a repeat
// task_id rather than inserting a duplicate, and keeps the denormalized state
// column in step with the contract.
func TestUpsertTask_UpdatesOnConflict(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	ctx := context.Background()

	tc := devTask()
	if err := s.UpsertTask(ctx, tc); err != nil {
		t.Fatalf("UpsertTask error = %v, want nil", err)
	}

	tc.State = task.StateInProgress
	if err := s.UpsertTask(ctx, tc); err != nil {
		t.Fatalf("UpsertTask (update) error = %v, want nil", err)
	}

	got, err := s.LoadTask(ctx, tc.ID)
	if err != nil {
		t.Fatalf("LoadTask error = %v, want nil", err)
	}
	if got.State != task.StateInProgress {
		t.Errorf("State = %q, want %q", got.State, task.StateInProgress)
	}

	var col string
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, tc.ID).Scan(&col); err != nil {
		t.Fatalf("read state column: %v", err)
	}
	if col != string(task.StateInProgress) {
		t.Errorf("tasks.state column = %q, want %q", col, task.StateInProgress)
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE task_id = ?`, tc.ID).Scan(&count); err != nil {
		t.Fatalf("count task rows: %v", err)
	}
	if count != 1 {
		t.Errorf("task row count = %d, want 1 (upsert, not duplicate insert)", count)
	}
}

// TestJournal_HappyPathSequence is AC-3.4: the happy-path events, read back
// ordered by seq, reconstruct the exact sequence with no needs-human hold, and
// every row carries an acting identity and a UTC ts. Identities vary by actor
// so writer != scorer shows up in the trail (the PR probe acts as the Reviewer,
// not the Dev identity).
func TestJournal_HappyPathSequence(t *testing.T) {
	t.Parallel()

	s := openStore(t)
	ctx := context.Background()

	steps := []struct {
		act, from, to, identity string
	}{
		{"dispatch", "", "queued", "system:orchestrator"},
		{"claim_ok", "queued", "in-progress", "system:orchestrator"},
		{"gate_rerun", "in-progress", "in-progress", "system:orchestrator"},
		{"pr_opened", "in-progress", "in-progress", "agent-user-dev-01"},
		{"probe_pr_exists", "in-progress", "in-progress", "agent-user-reviewer-01"},
		{"result_ok", "in-progress", "in-review", "system:orchestrator"},
	}
	for _, step := range steps {
		if err := s.AppendEvent(ctx, JournalEvent{
			TaskID:         "task-1",
			ActingIdentity: step.identity,
			Act:            step.act,
			FromState:      step.from,
			ToState:        step.to,
		}); err != nil {
			t.Fatalf("AppendEvent %q error = %v, want nil", step.act, err)
		}
	}

	got, err := s.Events(ctx, "task-1")
	if err != nil {
		t.Fatalf("Events error = %v, want nil", err)
	}
	if len(got) != len(steps) {
		t.Fatalf("Events len = %d, want %d", len(got), len(steps))
	}

	var lastSeq int64
	for i, e := range got {
		if e.Act != steps[i].act {
			t.Errorf("row %d Act = %q, want %q", i, e.Act, steps[i].act)
		}
		if e.ToState == "needs-human" {
			t.Errorf("row %d ToState = needs-human, want no hold on the happy path", i)
		}
		if e.ActingIdentity == "" {
			t.Errorf("row %d missing acting_identity", i)
		}
		if e.Ts.IsZero() || e.Ts.Location() != time.UTC {
			t.Errorf("row %d Ts = %v (%v), want a UTC timestamp", i, e.Ts, e.Ts.Location())
		}
		if e.Seq <= lastSeq {
			t.Errorf("row %d Seq = %d, want strictly greater than previous %d", i, e.Seq, lastSeq)
		}
		lastSeq = e.Seq
	}
}
