// Package journal is the durable record plane: a single-file SQLite store for
// the four tables RFC-0001 pins — tasks, runs, results, and the append-only
// journal (RFC-0001 §Journal and results schema, spec §6, D4). D4 fixes SQLite
// as the only database permanently, so the driver is pure-Go
// modernc.org/sqlite, which builds under CGO_ENABLED=0 and keeps the D3/D4
// static-binary invariant honest (a cgo driver would fail the make check
// static-build gate on arrival).
//
// The journal is append-only by construction: this package exposes no update
// or delete path for journal rows, and a BEFORE UPDATE / BEFORE DELETE trigger
// rejects both at the DB, so no transition or ground-truth probe can be
// rewritten after the fact (RFC-0001 AC-28). Schema evolution is additive only
// (spec §6): the migration is idempotent and runs at Open.
package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	// modernc.org/sqlite is the pure-Go, CGO-free driver (D4, ADR-0002 rung 3);
	// blank-imported to register the "sqlite" database/sql driver.
	_ "modernc.org/sqlite"

	"github.com/baodq97/mandat/internal/task"
)

// DefaultPath is the production database file (spec §6, D4: one file under
// /var/lib/mandat/). Tests pass a temp-file path instead.
const DefaultPath = "/var/lib/mandat/mandat.db"

// tsFormat is the on-disk timestamp encoding for every TEXT time column
// (RFC-0001 §Journal: RFC3339 UTC). Nano precision keeps closely-spaced
// appends distinguishable by value; seq remains the authoritative order.
const tsFormat = time.RFC3339Nano

// migrations is the idempotent, additive-only schema (spec §6), one DDL
// statement per element so each runs as a single prepared statement regardless
// of the driver's multi-statement handling. The two journal triggers are the
// append-only invariant's teeth: any UPDATE or DELETE against journal aborts the
// statement, so the guarantee holds even against raw SQL that bypasses this
// package's API. Cross-table references (runs.task_id, results.run_id) are plain
// TEXT, not enforced foreign keys — RFC-0001 keeps these columns typed TEXT so a
// run or event can be recorded on a crash-recovery path without strict
// parent-first ordering; the trigger guards the one invariant that is
// load-bearing.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS tasks (
	task_id     TEXT PRIMARY KEY,
	tracker_ref TEXT NOT NULL,
	role        TEXT NOT NULL,
	remit       TEXT NOT NULL,
	state       TEXT NOT NULL,
	contract    TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS runs (
	run_id          TEXT PRIMARY KEY,
	task_id         TEXT NOT NULL,
	session_id      TEXT,
	acting_identity TEXT,
	model           TEXT,
	started_at      TEXT,
	ended_at        TEXT,
	total_cost_usd  REAL,
	usage           TEXT,
	num_turns       INTEGER,
	is_error        INTEGER,
	exit_code       INTEGER,
	gate_result     TEXT,
	harness_version TEXT,
	config_version  TEXT
)`,
	`CREATE TABLE IF NOT EXISTS results (
	run_id      TEXT NOT NULL,
	task_id     TEXT NOT NULL,
	raw         TEXT NOT NULL,
	valid       INTEGER NOT NULL,
	recorded_at TEXT NOT NULL
)`,
	`CREATE TABLE IF NOT EXISTS journal (
	seq             INTEGER PRIMARY KEY AUTOINCREMENT,
	ts              TEXT NOT NULL,
	task_id         TEXT,
	run_id          TEXT,
	acting_identity TEXT NOT NULL,
	act             TEXT NOT NULL,
	from_state      TEXT,
	to_state        TEXT,
	detail          TEXT,
	config_version  TEXT,
	harness_version TEXT
)`,
	`CREATE TRIGGER IF NOT EXISTS journal_no_update BEFORE UPDATE ON journal
BEGIN
	SELECT RAISE(ABORT, 'journal is append-only: UPDATE rejected');
END`,
	`CREATE TRIGGER IF NOT EXISTS journal_no_delete BEFORE DELETE ON journal
BEGIN
	SELECT RAISE(ABORT, 'journal is append-only: DELETE rejected');
END`,
}

// Store is the SQLite-backed record plane. It owns the connection pool; callers
// share one Store and Close it once at shutdown.
type Store struct {
	db *sql.DB
}

// Open connects to the SQLite file at path in WAL mode and runs the idempotent
// migration. WAL and busy_timeout are set per connection via the DSN so they
// apply across the pool; journal_mode persists in the file header. It errors if
// the file cannot be opened or the migration fails.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("journal: connect %s: %w", path, err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("journal: migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// migrate applies every DDL statement in one transaction so a partial schema is
// never committed; the IF NOT EXISTS clauses make a re-run a no-op (spec §6).
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range migrations {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close releases the connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// JournalEvent is one append-only journal row (RFC-0001 §journal table). Seq
// and Ts are output-only on AppendEvent (the DB assigns seq; ts defaults to
// append time) and populated on read. Detail is an opaque JSON payload (pr_url,
// gate exit codes, session_id, cost). There is deliberately no update or delete
// counterpart: the append-only invariant lives in the absence of those methods
// plus the DB trigger.
type JournalEvent struct {
	Seq            int64
	Ts             time.Time
	TaskID         string
	RunID          string
	ActingIdentity string
	Act            string
	FromState      string
	ToState        string
	Detail         []byte
	ConfigVersion  string
	HarnessVersion string
}

// AppendEvent writes one journal row. acting_identity and act are required;
// from_state, to_state, run_id, and detail are optional and stored NULL when
// empty (an empty from_state is how dispatch records leaving the pre-creation
// pseudo-state, RFC-0001 AC-05). The row's ts is UTC.
func (s *Store) AppendEvent(ctx context.Context, e JournalEvent) error {
	ts := e.Ts
	if ts.IsZero() {
		ts = time.Now()
	}
	const q = `INSERT INTO journal
		(ts, task_id, run_id, acting_identity, act, from_state, to_state, detail, config_version, harness_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q,
		ts.UTC().Format(tsFormat), text(e.TaskID), text(e.RunID), e.ActingIdentity, e.Act,
		text(e.FromState), text(e.ToState), blob(e.Detail), text(e.ConfigVersion), text(e.HarnessVersion))
	if err != nil {
		return fmt.Errorf("journal: append event %q: %w", e.Act, err)
	}
	return nil
}

// Events returns taskID's journal rows ordered by seq, so the caller can
// reconstruct the exact transition sequence (RFC-0001 AC-28).
func (s *Store) Events(ctx context.Context, taskID string) ([]JournalEvent, error) {
	const q = `SELECT seq, ts, task_id, run_id, acting_identity, act, from_state, to_state, detail, config_version, harness_version
		FROM journal WHERE task_id = ? ORDER BY seq`
	rows, err := s.db.QueryContext(ctx, q, taskID)
	if err != nil {
		return nil, fmt.Errorf("journal: read events for %q: %w", taskID, err)
	}
	defer rows.Close()

	var out []JournalEvent
	for rows.Next() {
		var e JournalEvent
		var ts string
		var taskCol, runID, fromState, toState, detail, configV, harnessV sql.NullString
		if err := rows.Scan(&e.Seq, &ts, &taskCol, &runID, &e.ActingIdentity, &e.Act,
			&fromState, &toState, &detail, &configV, &harnessV); err != nil {
			return nil, fmt.Errorf("journal: scan event for %q: %w", taskID, err)
		}
		if e.Ts, err = time.Parse(tsFormat, ts); err != nil {
			return nil, fmt.Errorf("journal: parse event ts %q: %w", ts, err)
		}
		e.TaskID = taskCol.String
		e.RunID = runID.String
		e.FromState = fromState.String
		e.ToState = toState.String
		if detail.Valid {
			e.Detail = []byte(detail.String)
		}
		e.ConfigVersion = configV.String
		e.HarnessVersion = harnessV.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal: iterate events for %q: %w", taskID, err)
	}
	return out, nil
}

// Run is one execution record per spawn (RFC-0001 §runs table). Usage and
// GateResult are opaque JSON the store never parses; the verification plane
// produces GateResult and the runner reads TotalCostUSD, Usage, NumTurns, and
// IsError from the subprocess's terminal result event.
type Run struct {
	RunID          string
	TaskID         string
	SessionID      string
	ActingIdentity string
	Model          string
	StartedAt      time.Time
	EndedAt        time.Time
	TotalCostUSD   float64
	Usage          []byte
	NumTurns       int
	IsError        bool
	ExitCode       int
	GateResult     []byte
	HarnessVersion string
	ConfigVersion  string
}

// RecordRun inserts one run record. run_id is the primary key, so recording the
// same run twice is an error, not a silent overwrite.
func (s *Store) RecordRun(ctx context.Context, r Run) error {
	const q = `INSERT INTO runs
		(run_id, task_id, session_id, acting_identity, model, started_at, ended_at,
		 total_cost_usd, usage, num_turns, is_error, exit_code, gate_result, harness_version, config_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q,
		r.RunID, r.TaskID, text(r.SessionID), text(r.ActingIdentity), text(r.Model),
		tsText(r.StartedAt), tsText(r.EndedAt),
		r.TotalCostUSD, blob(r.Usage), r.NumTurns, boolToInt(r.IsError), r.ExitCode,
		blob(r.GateResult), text(r.HarnessVersion), text(r.ConfigVersion))
	if err != nil {
		return fmt.Errorf("journal: record run %q: %w", r.RunID, err)
	}
	return nil
}

// LoadRun returns the run keyed by runID.
func (s *Store) LoadRun(ctx context.Context, runID string) (Run, error) {
	const q = `SELECT run_id, task_id, session_id, acting_identity, model, started_at, ended_at,
		total_cost_usd, usage, num_turns, is_error, exit_code, gate_result, harness_version, config_version
		FROM runs WHERE run_id = ?`
	var r Run
	var sessionID, actingID, model sql.NullString
	var startedAt, endedAt sql.NullString
	var usage, gateResult, harnessV, configV sql.NullString
	var isError int64
	err := s.db.QueryRowContext(ctx, q, runID).Scan(
		&r.RunID, &r.TaskID, &sessionID, &actingID, &model, &startedAt, &endedAt,
		&r.TotalCostUSD, &usage, &r.NumTurns, &isError, &r.ExitCode, &gateResult, &harnessV, &configV)
	if err != nil {
		return Run{}, fmt.Errorf("journal: load run %q: %w", runID, err)
	}
	r.SessionID = sessionID.String
	r.ActingIdentity = actingID.String
	r.Model = model.String
	if r.StartedAt, err = parseTS(startedAt); err != nil {
		return Run{}, fmt.Errorf("journal: parse run started_at: %w", err)
	}
	if r.EndedAt, err = parseTS(endedAt); err != nil {
		return Run{}, fmt.Errorf("journal: parse run ended_at: %w", err)
	}
	r.IsError = isError != 0
	if usage.Valid {
		r.Usage = []byte(usage.String)
	}
	if gateResult.Valid {
		r.GateResult = []byte(gateResult.String)
	}
	r.HarnessVersion = harnessV.String
	r.ConfigVersion = configV.String
	return r, nil
}

// Result is the raw ResultContract the subprocess wrote, stored verbatim and
// never parsed as prose (RFC-0001 §results table). Valid carries the
// verification plane's schema-validation outcome; a missing or schema-invalid
// contract still persists its raw bytes with Valid false (RFC-0001 AC-21).
type Result struct {
	RunID      string
	TaskID     string
	Raw        []byte
	Valid      bool
	RecordedAt time.Time
}

// StoreResult inserts one result record, storing Raw exactly as given.
// RecordedAt defaults to now (UTC) when zero.
func (s *Store) StoreResult(ctx context.Context, r Result) error {
	recordedAt := r.RecordedAt
	if recordedAt.IsZero() {
		recordedAt = time.Now()
	}
	const q = `INSERT INTO results (run_id, task_id, raw, valid, recorded_at) VALUES (?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q,
		r.RunID, r.TaskID, string(r.Raw), boolToInt(r.Valid), recordedAt.UTC().Format(tsFormat))
	if err != nil {
		return fmt.Errorf("journal: store result for run %q: %w", r.RunID, err)
	}
	return nil
}

// Results returns runID's result rows in insertion order.
func (s *Store) Results(ctx context.Context, runID string) ([]Result, error) {
	const q = `SELECT run_id, task_id, raw, valid, recorded_at FROM results WHERE run_id = ? ORDER BY rowid`
	rows, err := s.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("journal: read results for run %q: %w", runID, err)
	}
	defer rows.Close()

	var out []Result
	for rows.Next() {
		var r Result
		var raw string
		var valid int64
		var recordedAt string
		if err := rows.Scan(&r.RunID, &r.TaskID, &raw, &valid, &recordedAt); err != nil {
			return nil, fmt.Errorf("journal: scan result for run %q: %w", runID, err)
		}
		r.Raw = []byte(raw)
		r.Valid = valid != 0
		if r.RecordedAt, err = time.Parse(tsFormat, recordedAt); err != nil {
			return nil, fmt.Errorf("journal: parse result recorded_at %q: %w", recordedAt, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal: iterate results for run %q: %w", runID, err)
	}
	return out, nil
}

// UpsertTask inserts or updates one task row, persisting the whole TaskContract
// as JSON in contract and denormalizing tracker_ref, role, remit, and state as
// their own columns (RFC-0001 §tasks table). created_at is set once on insert
// and preserved on update; updated_at advances every call.
func (s *Store) UpsertTask(ctx context.Context, tc *task.TaskContract) error {
	contract, err := json.Marshal(tc)
	if err != nil {
		return fmt.Errorf("journal: marshal task %q: %w", tc.ID, err)
	}
	trackerRef, err := json.Marshal(tc.TrackerRef)
	if err != nil {
		return fmt.Errorf("journal: marshal task %q tracker_ref: %w", tc.ID, err)
	}
	remit, err := json.Marshal(tc.Remit)
	if err != nil {
		return fmt.Errorf("journal: marshal task %q remit: %w", tc.ID, err)
	}
	now := time.Now().UTC().Format(tsFormat)
	const q = `INSERT INTO tasks
		(task_id, tracker_ref, role, remit, state, contract, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			tracker_ref = excluded.tracker_ref,
			role        = excluded.role,
			remit       = excluded.remit,
			state       = excluded.state,
			contract    = excluded.contract,
			updated_at  = excluded.updated_at`
	_, err = s.db.ExecContext(ctx, q,
		tc.ID, string(trackerRef), tc.Role, string(remit), string(tc.State), string(contract), now, now)
	if err != nil {
		return fmt.Errorf("journal: upsert task %q: %w", tc.ID, err)
	}
	return nil
}

// LoadTask returns the TaskContract stored under taskID, decoded from the
// contract column so the round-trip is byte-faithful to what UpsertTask wrote.
func (s *Store) LoadTask(ctx context.Context, taskID string) (*task.TaskContract, error) {
	const q = `SELECT contract FROM tasks WHERE task_id = ?`
	var contract string
	if err := s.db.QueryRowContext(ctx, q, taskID).Scan(&contract); err != nil {
		return nil, fmt.Errorf("journal: load task %q: %w", taskID, err)
	}
	var tc task.TaskContract
	if err := json.Unmarshal([]byte(contract), &tc); err != nil {
		return nil, fmt.Errorf("journal: unmarshal task %q: %w", taskID, err)
	}
	return &tc, nil
}

// text maps an empty string to a SQL NULL so optional TEXT columns stay NULL
// rather than storing "".
func text(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// blob maps empty JSON bytes to a SQL NULL, storing populated payloads as TEXT.
func blob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// tsText encodes a time as RFC3339 UTC, mapping the zero time to a SQL NULL.
func tsText(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(tsFormat)
}

// parseTS decodes an RFC3339 timestamp column, mapping NULL/empty to the zero
// time.
func parseTS(ns sql.NullString) (time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return time.Time{}, nil
	}
	return time.Parse(tsFormat, ns.String)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
