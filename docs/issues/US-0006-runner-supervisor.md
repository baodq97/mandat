---
id: US-0006
title: Runner supervisor — spawn Claude Code headless, OS-user isolation, stream-json, session, ResultContract read
status: open
owner: TBD
date: 2026-07-16
priority: P1
---

# US-0006: Runner supervisor — spawn Claude Code headless, OS-user isolation, stream-json, session, ResultContract read

As a mandat contributor, I want to spawn Claude Code headless under the per-role OS user
with the ADR-0006 flag set, parse stream-json telemetry, and read the ResultContract file,
so that the subprocess seam stays a validated file contract and never parses prose.

## Source

RFC-0001 (accepted) §Runner harness (Invocation, Session and resume, Isolation),
ADR-0006, §Package layout (`internal/runner`), AC-11..AC-15, AC-19..AC-22.

## Scope

`internal/runner`: the subprocess supervisor (ADR-0006 invocation), per-role OS-user
isolation (`systemd-run --uid=<role-user>` / `setpriv`, per-child `HOME` and
`CLAUDE_CONFIG_DIR`), stream-json event parsing (`system/init`, terminal `result`), the
deterministic `--session-id`, and reading `.mandat/result.json`.

## Acceptance criteria

- [ ] AC-6.1 Given a queued-and-claimed task, observe the runner spawns the fake `claude`
      (§9) with the ADR-0006 flag set, including `--bare`, `--permission-mode dontAsk`,
      `--add-dir <worktree>`, `--session-id <uuid>`, and `--max-budget-usd <ceiling>`
      (RFC-0001 AC-11; §9 double: fake `claude` binary).
- [ ] AC-6.2 Given a role configured with the default tier, observe `--model` carries
      `sonnet`; given a role with a per-role override to `opus`, observe `--model`
      carries `opus` (RFC-0001 AC-12; §9 double: fake `claude` binary; consumes
      role-tier resolution owned by US-0009 — see dependency note).
- [ ] AC-6.3 Given a spawn, observe a deterministic `--session-id` is written to
      `runs.session_id` before the subprocess starts and matches the `system/init`
      event's `session_id` the fake `claude` emits (RFC-0001 AC-13; §9 double: fake
      `claude` binary).
- [ ] AC-6.4 Given a spawn, observe the child process runs as the per-role OS user with
      its own `HOME` and `CLAUDE_CONFIG_DIR`, distinct from the parent's (RFC-0001
      AC-14; §9 double: fake `claude` binary combined with a stub OS-user provisioner).
- [ ] AC-6.5 Given a spawn using a stub identity broker, observe no Entra token appears in
      the child's environment or in any file the child's OS user can read (RFC-0001
      AC-15; §9 double: fake `claude` binary plus a stub broker, "seam-testable with the
      fake claude and a stub broker" per RFC-0001's own text).
- [ ] AC-6.6 Given the fake `claude` writes a ResultContract to `.mandat/result.json`,
      observe the supervisor reads that file directly and never treats any stream-json
      line as the outcome (RFC-0001 AC-19; §9 double: fake `claude` binary).
- [ ] AC-6.7 Given a schema-valid ResultContract with `status=completed` and one
      artifact, observe the runner hands the orchestrator a `result_ok`-eligible outcome,
      subject to gates/diff/probe still passing, which US-0007 covers (RFC-0001 AC-20;
      §9 double: fake `claude` binary).
- [ ] AC-6.8 Given a missing or schema-invalid ResultContract file, observe the runner
      reports an outcome routing to `needs-human` with no auto-retry, and hands the raw
      bytes plus `valid=0` to the journal store (RFC-0001 AC-21; §9 double: fake `claude`
      binary, missing-file and malformed-JSON variants).
- [ ] AC-6.9 Given a ResultContract with `status=needs_human` and `reason` set, observe
      the runner surfaces that reason for the journal (RFC-0001 AC-22; §9 double: fake
      `claude` binary).

## Remit

File-disjoint allowed paths:

- `internal/runner/**`

## Dependencies

Depends on US-0001 (ResultContract validation), US-0002 (orchestrator events the
runner's outcome maps to), US-0005 (the worktree the child runs inside). Cross-dependency:
AC-6.2's role-tier resolution and AC-6.5's broker are owned by US-0009 and US-0008
respectively, both scheduled P2 while this story is P1 — flagged in the gaps list.
