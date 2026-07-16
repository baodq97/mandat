---
id: US-0003
title: Journal, tasks, runs, and results SQLite store
status: done
owner: baodq97
date: 2026-07-16
priority: P0
---

# US-0003: Journal, tasks, runs, and results SQLite store

As a mandat contributor, I want an append-only SQLite store for tasks, runs, results, and
the journal, so that every transition and ground-truth probe is durably recorded and can
never be mutated after the fact.

## Source

RFC-0001 (accepted) §Journal and results schema (four table definitions, append-only
invariant), §Package layout (`internal/journal`), design spec §6 (D4: SQLite only, pure-Go
driver, additive migrations).

## Scope

`internal/journal`: the `tasks`, `runs`, `results`, and `journal` tables exactly as
specified in RFC-0001's schema tables, WAL mode, one file, additive migrations only, and
code that exposes no update or delete path for `journal` rows (backed by a DB trigger that
rejects both).

## Acceptance criteria

- [ ] AC-3.1 Given a dispatch event, observe a `journal` row is written with
      `acting_identity=system:orchestrator`, `act=dispatch`, `from_state` empty,
      `to_state=queued` (RFC-0001 AC-05; store-level contract test against a temp SQLite
      file — RFC-0001's §9 list names three doubles for adapter/workspace/runner only; no
      named double covers the journal seam, so this is the store's own I/O-seam test per
      the general §9 principle, flagged in the gaps list).
- [ ] AC-3.2 Given a completed gate re-run's command list and per-command exit codes,
      observe they land in `runs.gate_result` as JSON (RFC-0001 AC-25; store-level
      contract test).
- [ ] AC-3.3 Given a run whose ResultContract file is missing or schema-invalid, observe
      the raw bytes and `valid=0` land in the `results` table (RFC-0001 AC-21;
      store-level contract test).
- [ ] AC-3.4 Given the happy-path event sequence `dispatch, claim_ok, gate_rerun,
      pr_opened, probe_pr_exists, result_ok`, observe the `journal` table's rows, ordered
      by `seq`, reconstruct that exact sequence with no `needs-human` row, and every row
      carries `acting_identity` and a UTC `ts` (RFC-0001 AC-28; store-level contract
      test).
- [ ] AC-3.5 Given an attempt to `UPDATE` or `DELETE` a `journal` row via direct SQL
      (bypassing the Go API), observe the operation is rejected — no exposed update/delete
      path in code, and a trigger rejects both (RFC-0001 AC-28: "no journal row is ever
      updated or deleted"; store-level contract test).
- [ ] AC-3.6 Given a completed run's terminal `result` event fields (`total_cost_usd`,
      `usage`, `num_turns`, `is_error`), observe they persist in `runs` (RFC-0001 AC-28's
      "on the completed run, `total_cost_usd` and `usage`"; store-level contract test).

## Remit

File-disjoint allowed paths:

- `internal/journal/**`

## Dependencies

Soft dependency on US-0001 (TaskContract/ResultContract JSON payloads the `tasks.contract`
and `results.raw` columns carry) and US-0002 (the orchestrator state/event vocabulary the
`tasks.state`, `journal.act`, `journal.from_state`/`to_state` columns record). RFC-0001
does not mandate a Go import between these packages — columns are typed `TEXT` — so this
is a sequencing note, not a hard compile dependency.
