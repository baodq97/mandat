---
id: US-0010
title: cmd wiring — serve dispatch loop running the skeleton end to end
status: open
owner: TBD
date: 2026-07-16
priority: P3
---

# US-0010: cmd wiring — serve dispatch loop running the skeleton end to end

As a mandat contributor, I want `mandat serve` to run the poll-dispatch daemon composing
every plane, so that one ADO work item reaches `in-review` with a draft PR, a green gate
re-run, and a complete journal, matching RFC-0001's definition of done.

## Source

RFC-0001 (accepted) §Definition of done, §Package layout (`cmd/mandat` adds `serve`
"beside `version`"), AC-21, AC-28.

## Scope

`cmd/mandat`'s `serve` subcommand: the 30s poll/dispatch daemon wiring `internal/config`,
`internal/task`, `internal/orchestrator`, `internal/tracker` +
`internal/adapter/azuredevops`, `internal/workspace`, `internal/runner`,
`internal/verify`, `internal/identity`, and `internal/journal` into one loop.

## Acceptance criteria

- [ ] AC-10.1 Given a recorded ADO fixture (one work item assigned to
      `<dev-agent-user>`), a local bare git origin, and the fake `claude` binary composed
      together, observe the end-to-end happy path produces a journal whose ordered rows
      reconstruct `dispatch -> claim_ok -> gate_rerun -> pr_opened -> probe_pr_exists ->
      result_ok -> in-review` with no `needs-human` hold (RFC-0001 AC-28; §9 doubles: all
      three composed — recorded ADO fixture, local bare git origin, fake `claude`).
- [ ] AC-10.2 Given the same composed run, observe every journal row carries acting
      identity and a UTC timestamp, and the completed run's row carries
      `total_cost_usd` and `usage` (RFC-0001 AC-28; same composed doubles).
- [ ] AC-10.3 Given the composed run reaches `in-review`, observe the per-repo gate
      re-run is green, the post-hoc diff stays inside the remit, and a draft PR is
      recorded under the Dev agent user, per RFC-0001's Definition of done (composed
      doubles; note AC-26/AC-27's live `createdBy`/probe-identity confirmation stays out
      of this story's reach per RFC-0001's explicit live-check-only statement).
- [ ] AC-10.4 Given a fixture run where the fake `claude` is scripted to omit the
      ResultContract file, observe `mandat serve`'s composed pipeline routes the task to
      `needs-human` with a journaled `result_invalid` reason and no crash (RFC-0001
      AC-21, composed at the cmd-wiring seam; §9 double: fake `claude`, missing-file
      variant).

## Remit

File-disjoint allowed paths:

- `cmd/mandat/main.go`
- `cmd/mandat/serve.go`
- `cmd/mandat/serve_test.go`
- `cmd/mandat/main_test.go`

## Dependencies

Depends on US-0001 through US-0009 all being wired in; intentionally last in the build
spine. US-0009 also touches `cmd/mandat/main.go` to add the `doctor` dispatch case — land
US-0009 first to avoid a merge collision on the same dispatch switch statement. This one
line is not fully file-disjoint from US-0009; flagged as a sequencing dependency, not a
parallel pair.
