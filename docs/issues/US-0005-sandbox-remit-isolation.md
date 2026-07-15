---
id: US-0005
title: Sandbox / remit isolation — mirror cache, worktree, sparse checkout, diff-in-remit
status: open
owner: TBD
date: 2026-07-16
priority: P1
---

# US-0005: Sandbox / remit isolation — mirror cache, worktree, sparse checkout, diff-in-remit

As a mandat contributor, I want a per-task worktree with sparse checkout limited to the
remit and a post-hoc diff-inside-remit check, so that the agent cannot see or touch paths
outside its mandate.

## Source

RFC-0001 (accepted) §Runner harness → Isolation, §Package layout (`internal/workspace`),
AC-10, AC-16, AC-17, AC-18.

## Scope

`internal/workspace`: mirror cache, `git worktree add --reference` provisioning against
the mirror, sparse checkout materializing only `remit.paths`, and the diff-inside-remit
check (`.mandat/` excluded from the comparison).

## Acceptance criteria

- [ ] AC-5.1 Given a queued task and a local bare git origin, observe claiming it
      provisions a worktree via `git worktree add --reference` against that origin and
      sparse-checks-out only the remit paths (RFC-0001 AC-10; §9 double: local bare git
      origin).
- [ ] AC-5.2 Given a worktree whose branch diff touches only remit paths, observe the
      diff-inside-remit check passes (RFC-0001 AC-16; §9 double: local bare git origin,
      combined with a scripted edit inside the remit).
- [ ] AC-5.3 Given a worktree whose branch diff touches a path outside the remit, observe
      the check fails and reports a `remit_violation` reason (RFC-0001 AC-17; §9 double:
      local bare git origin, combined with a scripted edit outside the remit).
- [ ] AC-5.4 Given a worktree or sparse-checkout provisioning failure (the bare origin is
      unreachable, or sparse-checkout setup errors), observe the failure surfaces as a
      `setup_failed` condition rather than silently degrading to a full checkout
      (RFC-0001 AC-18, "no shared-checkout fallback"; §9 double: local bare git origin,
      forced-failure variant). This story proves the workspace half of `setup_failed`
      only; the per-role OS-user half is US-0006's.
- [ ] AC-5.5 Given the `.mandat/` control directory inside a worktree, observe the
      diff-inside-remit check excludes it from the comparison (RFC-0001 §ResultContract:
      "the `.mandat/` control directory is excluded from the diff-inside-remit check";
      §9 double: local bare git origin).

## Remit

File-disjoint allowed paths:

- `internal/workspace/**`

## Dependencies

Depends on US-0001 (the remit shape `{repo, base_branch, paths}` carried on
TaskContract). Composes with US-0006 (runner) at the `setup_failed` / OS-user seam and
with US-0002 (orchestrator) for the resulting state transition; the full composed proof
lands in US-0010.
