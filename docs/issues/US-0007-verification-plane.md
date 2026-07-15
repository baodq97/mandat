---
id: US-0007
title: Verification plane — gate re-run, diff-inside-remit consumption, PR-existence probe
status: open
owner: TBD
date: 2026-07-16
priority: P2
---

# US-0007: Verification plane — gate re-run, diff-inside-remit consumption, PR-existence probe

As a mandat contributor, I want the verifier to re-run the repo's own gates and confirm
the PR exists under the Reviewer identity, never trusting the agent's self-report, so
that `in-review` is reached only on ground-truth-confirmed state.

## Source

RFC-0001 (accepted) §Definition of done, AC-23..AC-25, AC-27, §Package layout
(`internal/verify`).

## Scope

`internal/verify`: per-repo gate command-list re-run executed in the verifier's own
context, consumption of the diff-inside-remit result (US-0005), and the PR-existence
probe run under the Reviewer identity.

## Acceptance criteria

- [ ] AC-7.1 Given a repo configured with the gate list `make check` then `npx govkit
      check`, observe the verifier executes both commands in its own process context,
      never reading or trusting any agent-produced summary (RFC-0001 AC-23). No §9 double
      is named in RFC-0001 for this seam — flagged in the gaps list; a stubbed fixture
      repo with a trivial replacement command list is proposed pending owner
      confirmation.
- [ ] AC-7.2 Given both gate commands exit 0, observe the verifier reports a green result
      the orchestrator can key `result_ok` on; given either command exits non-zero,
      observe the verifier reports a failure naming the failing command and its exit code
      (RFC-0001 AC-24). Same gap as AC-7.1.
- [ ] AC-7.3 Given a completed gate re-run, observe the verifier returns a result shape
      (command list, per-command exit codes) suitable for `runs.gate_result` (RFC-0001
      AC-25; consumed by US-0003).
- [ ] AC-7.4 Given a stub PR-existence probe configured to act under the Reviewer
      identity's credentials, distinct from the Dev identity's, observe the probe's
      request is issued as the Reviewer identity, never the Dev identity (RFC-0001
      AC-27, architecture proof only). RFC-0001 states the full AC-27 claim — the probe
      confirms the PR and fires `probe_failed` on no-PR or a mismatched `createdBy` — is
      verified by a live integration check against the kept S1/S3 spike assets, not a §9
      double; this story proves the identity-routing wiring, not live confirmation.

## Remit

File-disjoint allowed paths:

- `internal/verify/**`

## Dependencies

Depends on US-0005 (diff-inside-remit result), US-0003 (the `gate_result` storage shape),
US-0008 (the Reviewer-identity credential the probe acts under).
