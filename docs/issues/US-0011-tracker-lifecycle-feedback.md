---
id: US-0011
title: Tracker lifecycle feedback — in-progress state, PR link, and outcome comments
status: open
owner: baodq97
date: 2026-07-16
priority: P1
---

# US-0011: Tracker lifecycle feedback — in-progress state, PR link, and outcome comments

As a human watching the board, I want the pipeline to reflect its own progress on the
work item through a state change, a PR link, and outcome comments, so that I see a task
move without opening the journal.

## Source

RFC-0001 (accepted) §4.2 Tracker plane (`Adapter.Apply(outcome)`, `Adapter.Comment(id,
body)`, "every write uses the acting RoleAgent's token"); US-0004 (the ADO adapter
already ships `Comment` and `ApplyStatus`); design spec §4.2 (Tracker plane), §4.4 (Role
plane, "a proposal never self-ratifies"), §8 (failure handling, "every external write is
best-effort with logged degradation, except gates").

## Problem

The pipeline runs silently on the tracker. `internal/adapter/azuredevops` already
implements `Comment` and `ApplyStatus` (US-0004), but `cmd/mandat/serve.go`'s `runTask`
never calls either, and the draft PR it opens carries no link back to the work item. A
human watching the board sees no state change, no comments, and no PR under
Development/links. The only visible evidence of a run is the journal, which the board
does not surface.

## Design boundary

Setting the board's in-progress state is operational reflection: it restates a fact the
pipeline already owns (a run is under way) and mandat may write it. Setting a done or
completed state is ratification: the human act that advances a stage gate (design spec
glossary, RFC-0001 §4.4, "a proposal never self-ratifies"). Mandat never writes a
done/completed state on any work item; RFC-0001 §Out of scope already places the `done`
state outside the walking skeleton (decision 2, human ratification act). This story adds
no exception to that boundary; it only makes the in-progress half of the lifecycle
visible.

## Scope

- `cmd/mandat/serve.go`: call the tracker adapter's `ApplyStatus` and `Comment` at the
  dispatch, PR-open, and terminal-hold points in `runTask`.
- `internal/adapter/azuredevops`: extend `CreatePRInput`/`CreatePR` to carry a work-item
  link so the created PR is associated with its source work item (ADO PR-to-work-item
  link, surfaced on the board under Development/links).
- `internal/config`: add the tracker in-progress state name as config
  (`tracker.states.in_progress`), not a code constant.

## Acceptance criteria

- [ ] AC-11.1 On dispatch of a TaskContract, before the runner spawns, observe `serve`
      calls `ApplyStatus` with the configured in-progress state and `Comment` with a
      message naming the task id and the acting RoleAgent (RFC-0001 §4.2). (Corrected
      from "run id" at the done-flip red-team pass: the run id is minted by the runner,
      which starts after this comment posts, so it cannot be named at that seam; the
      task id is the board-correlating key.)
- [ ] AC-11.2 When `serve` opens the draft PR, observe the PR carries a work-item link
      (the ADO PR is created with a work-item ref, so the board shows it under
      Development/links) and observe `serve` posts a comment carrying the PR URL.
- [ ] AC-11.3 When a run ends `needs-human` or `failed`, observe `serve` posts a comment
      carrying the reason: gate detail, remit violation, or probe finding, matching
      whichever hold the orchestrator recorded.
- [ ] AC-11.4 Given two fixture configs, one with `tracker.states.in_progress: Doing`
      (ADO Basic process default) and one with `tracker.states.in_progress: Active`
      (ADO Agile process), observe `ApplyStatus` is called with each configured value and
      no code path branches on the process name.
- [ ] AC-11.5 Across every fixture and code path in this story, observe no call ever
      writes a done or completed state to any work item; `ApplyStatus` is invoked only
      with the configured in-progress value.
- [ ] AC-11.6 Given a fixture where `Comment` or `ApplyStatus` returns an error, observe
      `runTask` logs it as a warning and continues to the next pipeline step with no
      task failure and no retry of the tracker write (design spec §8, "every external
      write is best-effort with logged degradation, except gates"). (Corrected from
      "journals" at the done-flip red-team pass: the shipped degradation signal is the
      process log, not a journal row; recording tracker-write degradations in the
      append-only journal is tracked as its own backlog item, not silently implied
      here.)
- [ ] AC-11.7 Given a fixture ADO server recording the bearer token on every tracker
      write in this story (state, comment, PR link), observe every one carries the
      acting RoleAgent's delegated token, not a shared or service token (RFC-0001 §4.2,
      spec §4.2).

## Remit

File-disjoint allowed paths:

- `cmd/mandat/serve.go`
- `cmd/mandat/serve_test.go`
- `internal/adapter/azuredevops/**`
- `internal/config/**`

## Dependencies

Depends on US-0004 (`Comment`/`ApplyStatus` already implemented on the adapter) and
US-0010 (the `runTask` composition this story instruments). Not file-disjoint from
US-0010 or US-0009: `cmd/mandat/serve.go` and `internal/config` are both touched
elsewhere in the build spine, so this story lands after both to avoid a merge collision.

## Gaps

- The exact in-progress state values for non-Basic ADO processes (Agile "Active", Scrum
  "In Progress") are named here as the config values this story's fixtures exercise;
  no further process-specific mapping is invented beyond what config carries.
- This story does not touch the `done`/`failed` terminal states or the human-ratification
  write path; those stay out of scope per RFC-0001 decision 2.
