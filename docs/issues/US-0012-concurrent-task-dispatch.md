---
id: US-0012
title: Concurrent task dispatch (runner pool slice)
status: open
owner: baodq97
date: 2026-07-16
priority: P2
---

# US-0012: Concurrent task dispatch (runner pool slice)

As a mandat contributor, I want `dispatchCycle` to run independent queued contracts
concurrently up to a configured pool size, so that a story broken into several
file-disjoint tasks executes in parallel instead of one at a time.

## Source

RFC-0001 (accepted) §Scope ("Dependency waves, conflict groups, and the scheduler
beyond a single in-flight task" is out of scope), §Accepted trade-offs ("dependency
waves already serialize; a runner-pool mode is roadmap, not MVP"). Design spec §4.3
(Orchestrator plane: "Zero-token dispatcher: dependency links block hard, ... waves
respect `max_concurrent`") and §10 (MVP slice and roadmap: multi-VM runner pool "only
when a real customer saturates one VM"). US-0006 (runner supervisor, the per-task
spawn this story runs more than one of at a time).

## Problem

RFC-0001 scopes the walking skeleton to a single in-flight task. `cmd/mandat/serve.go`'s
`dispatchCycle` reflects that scope directly: it loops over polled contracts and calls
`runTask` one at a time, in order. `internal/workspace`'s `ensureMirror` carries the same
scope as an explicit code comment: "The skeleton runs one in-flight task (RFC-0001
scope), so the refresh needs no cross-task lock yet."

The consequence: breaking one story into several file-disjoint tasks (the remit-based
parallelism the product's own design assumes) still executes those tasks serially
today. The pilot demonstrated story breakdown but not parallel implementation. The
design spec already names dependency-wave orchestration with a `max_concurrent` ceiling
as the target shape (§4.3); this story is the first slice toward it, scoped to pool
size alone, not waves or conflict groups.

## Scope

`cmd/mandat/serve.go`'s `dispatchCycle` and the shared-mirror provisioning path in
`internal/workspace`. A configured runner pool size (e.g. `runner.pool_size`) bounds
how many queued contracts run at once; the shared bare-mirror clone/fetch gets a
cross-task lock so two concurrent provisions of the same repo cannot race each other.
Dependency waves and conflict groups (design spec §4.3) stay out of scope; this story
governs pool size only, not scheduling by dependency graph.

## Acceptance criteria

- [ ] AC-12.1 Given more queued contracts than the configured pool size, observe
      `dispatchCycle` runs independent contracts concurrently up to that pool size
      (e.g. `runner.pool_size`), and given the default pool size of 1, observe
      dispatch behavior is identical to today's sequential loop.
- [ ] AC-12.2 Given two queued contracts that provision the same repo mirror
      concurrently, observe a cross-task lock serializes the clone/fetch against that
      mirror so no race corrupts the shared object store (the `ensureMirror` comment's
      "no cross-task lock yet" becomes real locking code).
- [ ] AC-12.3 Given concurrent runs, observe each task's existing per-task isolation
      (worktree, branch, journal rows) is unchanged, and observe journal writes stay
      serialized through the existing store with no interleaved or lost rows.
- [ ] AC-12.4 Given a concurrent dispatch cycle, observe scheduling which contract
      runs next stays zero-token: pure orchestrator logic decides pool admission, with
      no LLM call in the decision path.
- [ ] AC-12.5 Given one concurrent run's task fails, observe no sibling run's outcome,
      state, or journal rows are affected by that failure.

## Remit

File-disjoint allowed paths:

- `cmd/mandat/serve.go`
- `cmd/mandat/serve_test.go`
- `internal/workspace/workspace.go`
- `internal/workspace/workspace_test.go`

## Dependencies

Depends on US-0010 (cmd wiring; this story extends `dispatchCycle` after that loop
exists) and US-0006 (runner supervisor; this story runs more than one runner spawn
concurrently). Relaxes the single-in-flight scope RFC-0001 sets — an RFC-level scope
change, not just a US; flagging that the accepted RFC's "no dependency waves" scope
line may need a corresponding amendment or successor RFC before this story lands,
rather than treating the US alone as authorization to relax it.

## Gaps

- Pool size source (`runner.pool_size` vs another config key/shape) is this story's
  proposal, not a value RFC-0001 or the design spec pins; the implementer sets the
  final key name and default.
- Dependency waves and conflict groups (design spec §4.3, §10 roadmap) are explicitly
  deferred past this slice; do not fold them into this story's acceptance criteria.
- Whether relaxing RFC-0001's "single in-flight task" scope line needs a formal RFC
  amendment before code lands is unresolved — flagged for the doc owner, not decided
  here.
