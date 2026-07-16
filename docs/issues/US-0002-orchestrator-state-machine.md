---
id: US-0002
title: Orchestrator — pure-function state machine (Next)
status: done
owner: baodq97
date: 2026-07-16
priority: P0
---

# US-0002: Orchestrator — pure-function state machine (Next)

As a mandat contributor, I want a pure `Next(state, event) -> (state, error)` function
implementing the six-state, twelve-transition table, so that every plane keys off one
deterministic, zero-token, zero-I/O state authority.

## Source

RFC-0001 (accepted) §Orchestrator state machine (state list, transition table, the
unlisted-pair invariant), §Package layout (`internal/orchestrator`).

## Scope

`internal/orchestrator`: states `queued`, `in-progress`, `in-review`, `needs-human`,
`done`, `failed`; the twelve transitions in RFC-0001's table (`dispatch`, `claim_ok`,
`setup_failed`, `result_ok`, `result_needs_human`, `result_invalid`, `gate_red`,
`remit_violation`, `probe_failed`, `human_requeue`, `human_abandon`, `human_ratify`); the
no-silent-transition invariant for unlisted pairs.

## Acceptance criteria

- [ ] AC-2.1 Given state `(start)` and event `dispatch`, observe `Next` returns `queued`
      (RFC-0001 transition table; pure-core unit test).
- [ ] AC-2.2 Given `queued` and `claim_ok`, observe `in-progress`; given `queued` and
      `setup_failed`, observe `needs-human` (RFC-0001 AC-18; pure-core unit test).
- [ ] AC-2.3 Given `in-progress` and each of `result_needs_human`, `result_invalid`,
      `gate_red`, `remit_violation`, `probe_failed`, observe `Next` returns `needs-human`
      for every one (RFC-0001 AC-21, AC-22, AC-24, AC-17, AC-27; pure-core table-driven
      unit test).
- [ ] AC-2.4 Given `in-progress` and `result_ok`, observe `Next` returns `in-review`
      (RFC-0001 AC-20; pure-core unit test).
- [ ] AC-2.5 Given `needs-human` and `human_requeue`, observe `queued`; given
      `needs-human` and `human_abandon`, observe `failed`; given `in-review` and
      `human_ratify`, observe `done` — enumerated for a total function but not driven
      operationally by the skeleton (RFC-0001 §Orchestrator state machine: "the skeleton
      drives the subset reaching in-review and needs-human"; pure-core unit test).
- [ ] AC-2.6 Given any `(state, event)` pair absent from the twelve-row table, observe
      `Next` returns a non-nil error with the state unchanged, never a silent transition
      (RFC-0001 §Orchestrator state machine transition-table note; pure-core unit test,
      exhaustive over the state×event product).

## Remit

File-disjoint allowed paths:

- `internal/orchestrator/**`

## Dependencies

None — pure core, buildable in parallel with US-0001 and US-0003. Feeds US-0003 (journal
stores orchestrator state), US-0006 and US-0010 (drive transitions at runtime).

## Notes

`human_requeue`, `human_abandon`, and `human_ratify` are covered here as pure-function
inputs because RFC-0001 states the function is "total over the enumerated inputs," but no
story in this backlog wires an operational trigger for them (retry, abandon, and merge
are all out of the RFC-0001 skeleton scope, decisions 2 and 3).
