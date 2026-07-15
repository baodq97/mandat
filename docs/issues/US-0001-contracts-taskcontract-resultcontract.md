---
id: US-0001
title: Contracts — TaskContract and ResultContract Go types with JSON-schema validation
status: open
owner: TBD
date: 2026-07-16
priority: P0
---

# US-0001: Contracts — TaskContract and ResultContract Go types with JSON-schema validation

As a mandat contributor, I want TaskContract and ResultContract as validated Go types, so
that every downstream plane (adapter, orchestrator, runner, journal) shares one
schema-checked contract instead of ad hoc JSON.

## Source

RFC-0001 (accepted) §Load-bearing contracts (TaskContract field table, ResultContract JSON
schema), §Package layout (`internal/task`, `internal/result`).

## Scope

- `internal/task`: the `TaskContract` type and validation — `id`, `tracker_ref`, `type`,
  `title`, `acceptance`, `refs`, `state`, `role`, `remit`, `assigned_to`, `schema_version`
  (RFC-0001 field table).
- `internal/result`: the `ResultContract` type, JSON-schema validation matching the schema
  block in RFC-0001 §Load-bearing contracts, and the `.mandat/result.json` path constant
  passed to the child as `MANDAT_RESULT_PATH`.

## Acceptance criteria

- [ ] AC-1.1 Given a TaskContract JSON with every required field present and well-typed,
      observe validation succeeds and `schema_version` pins to `1` (RFC-0001 AC-09;
      pure-core unit test — no §9 double needed, §9: "pure cores get exhaustive unit
      tests").
- [ ] AC-1.2 Given a TaskContract JSON missing a required field, observe validation fails
      before dispatch and the error names the missing field (RFC-0001 AC-09; pure-core
      unit test).
- [ ] AC-1.3 Given a ResultContract JSON with `status: completed` and no `artifacts`,
      observe schema validation rejects it; given the same with one artifact carrying
      `repo` and `branch`, observe it validates (underlies RFC-0001 AC-20/AC-21; pure-core
      unit test against the schema block).
- [ ] AC-1.4 Given a ResultContract JSON with `status: needs_human` or `status: failed`
      and no `reason`, observe validation rejects it; given the same with `reason` set,
      observe it validates (underlies RFC-0001 AC-22; pure-core unit test).
- [ ] AC-1.5 Given a ResultContract JSON with an unknown top-level property, observe
      validation rejects it (`additionalProperties: false`, RFC-0001 §Load-bearing
      contracts; pure-core unit test).
- [ ] AC-1.6 Given `internal/result`'s exported path constant, observe it equals
      `.mandat/result.json` and matches the documented `MANDAT_RESULT_PATH` env var name
      (underlies RFC-0001 AC-19; pure-core unit test).

## Remit

File-disjoint allowed paths:

- `internal/task/**`
- `internal/result/**`

## Dependencies

None — pure core, first in the build spine. Feeds US-0003 (journal persists typed
payloads), US-0004 (adapter produces TaskContract), US-0006 (runner reads/validates
ResultContract).

## Notes

So I want to keep this concise: this story is types plus validation only. It carries no
I/O and no adapter, orchestrator, or runner wiring — those are separate stories.
