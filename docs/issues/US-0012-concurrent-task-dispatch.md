---
id: US-0012
title: Concurrent task dispatch (runner pool slice)
status: done
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
      concurrently, observe a cross-task lock covers the entire per-repo mirror touch:
      clone/fetch, the idempotent config heal (the git config writes `ensureMirror` runs
      on every call, including on an already-warm cache), and worktree add, not
      clone/fetch alone (the `ensureMirror` comment's "no cross-task lock yet" becomes
      real locking code around the whole touch, not just the fetch). Rationale: `git
      config` takes an exclusive `config.lock` with no retry, so two concurrent
      same-repo provisions racing that heal turn the loser into a spurious
      needs-human. Test: two concurrent `Provision` calls against one warm mirror never
      produce a `SetupError`.
- [ ] AC-12.3 Given concurrent runs, observe each task's existing per-task isolation
      (worktree, branch, journal rows, and `CLAUDE_CONFIG_DIR`/`HOME`, new to this story
      per AC-12.7) is unchanged or newly isolated per task, and observe journal writes
      stay serialized through the existing store with no interleaved or lost rows.
- [ ] AC-12.4 Given a concurrent dispatch cycle, observe scheduling which contract
      runs next stays zero-token: pure orchestrator logic decides pool admission, with
      no LLM call in the decision path.
- [ ] AC-12.5 Given one concurrent run's task fails, observe no sibling run's outcome,
      state, or journal rows are affected by that failure.
- [ ] AC-12.6 Given a poll interval shorter than one run's duration and more queued
      contracts than the configured pool size, observe `dispatchCycle` never
      re-dispatches a contract a prior cycle already polled but has not yet started to
      run. Name the guard mechanism used: either the cycle blocks until every task it
      launched that pass finishes before returning, or the store row is written at
      admission (before provision starts), not at run start, so the next poll's query
      excludes it. Test: back up more contracts than `pool_size` with a poll interval
      shorter than run duration; assert exactly one dispatch per work item.
- [ ] AC-12.7 Given two concurrent runs of the same role, observe they do not share a
      writable `CLAUDE_CONFIG_DIR`/`HOME`. Today both are per-role
      (`roles/<role>/home`, `roles/<role>/claude-config`, set in
      `cmd/mandat/serve.go` and passed to `internal/runner`'s spawned process as the
      `HOME`/`CLAUDE_CONFIG_DIR` env vars), and the spawned `claude` process rewrites
      its config file wholesale under that directory, so two same-role runs sharing it
      clobber each other's session state.
      The story either gives each task its own config dir seeded from the role's, or
      documents a serialization that keeps same-role concurrency from overlapping;
      whichever the implementer picks, it is a named, tested mechanism, not silent
      sharing.
- [ ] AC-12.8 Given the configured pool size N and the existing per-run cost ceiling
      `budget.max_usd_per_run`, observe a config-level aggregate ceiling bounds total
      concurrent burn: a new key (e.g. `budget.max_usd_in_flight`) enforced before
      admitting another task into the pool, or, at minimum, the config comment next to
      `pool_size` documents the N × `max_usd_per_run` worst case a reviewer must
      approve. Pool size must not multiply spend with no circuit breaker; autonomy and
      cost ceilings live in reviewable config, per the design (spec §4.8).

## Definition of done

- [ ] Benchmark gate: on the pilot VM, `pool_size = N` (N > 1) beats `pool_size = 1`
      sequential wall-clock on the same backlog of independent, file-disjoint tasks. No
      measured speedup means single-VM contention (CPU, disk I/O, or the mirror lock
      itself) is structural, and this story yields to the multi-VM scale-out (D4,
      design spec §10) instead of accumulating more locks on one VM.

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
  here. The accepted red-team pass on this story raised the same question as finding
  F4 (amend RFC-0001 in place, write a successor RFC, or proceed on the US alone) and
  it awaits the doc owner's ruling; decision pending.
