---
id: US-0008
title: Identity broker — ADR-0005 token mint, tracker MCP wiring, git credential delivery (SPIKE GATED)
status: open
owner: TBD
date: 2026-07-16
priority: P2
---

# US-0008: Identity broker — ADR-0005 token mint, tracker MCP wiring, git credential delivery

As a mandat contributor, I want the broker to mint delegated agent-user tokens on demand
and keep them inside its own process boundary, so that the child process never holds a
credential it could leak, for both tracker access and, once unblocked, the git push path.

**Spike gate: the git credential-delivery portion of this story is BLOCKED on spike
S-credential-delivery (design spec §11, F4). RFC-0001 states plainly: "the spike gates the
runner's credential code, which is not written until a cell passes" and "until it
resolves, the runner's push path stays unbuilt" (RFC-0001 §Identity injection). Do not
start AC-8.3/AC-8.4 before the spike clears a matrix cell.**

## Source

RFC-0001 (accepted) §Identity injection (decision 8), §Open questions
(invariant-preserving git credential delivery), ADR-0005, §Package layout
(`internal/identity`), AC-15, AC-26.

## Scope

`internal/identity`: the token broker implementing the ADR-0005 three-leg chain
(blueprint → agent-identity federated-credential exchange → user federated-credential
grant), the mandat-provided MCP server wiring for tracker read/comment (`--mcp-config`),
and the git credential-delivery backing (mechanism TBD by the spike).

## Acceptance criteria — not blocked (broker mint and MCP wiring)

- [ ] AC-8.1 Given a stub broker minting a delegated agent-user token on demand, observe
      the token is never written to the child's environment, argv, or a file the child's
      OS user can read, and it lives only for the single operation that needs it
      (RFC-0001 AC-15, decision 8; §9 double: fake `claude` binary plus a stub broker,
      per RFC-0001's explicit statement).
- [ ] AC-8.2 Given a tracker read/comment call, observe it is routed through the
      mandat-provided MCP server backed by the broker (`--mcp-config`), so no tracker
      token reaches the child (RFC-0001 §Identity injection; §9 double: fake `claude`
      binary, MCP config wiring check — this proves wiring, not the live ADO round-trip,
      which US-0004 covers separately).

## Acceptance criteria — BLOCKED on spike S-credential-delivery

- [ ] AC-8.3 [BLOCKED] Given the spike matrix { credential-helper Basic-password
      (git ≥ 2.46), credential-helper `authtype`/Bearer (git ≥ 2.46), ephemeral
      per-operation `http.extraheader` }, observe at least one cell clears all three
      columns — ADO accepts the token in that shape, and the token is invisible to a
      process running as the role OS user — before any git credential-delivery code is
      written (RFC-0001 §Identity injection, §Open questions: "the spike gates the
      runner's credential code, which is not written until a cell passes").
- [ ] AC-8.4 [BLOCKED, pending S-credential-delivery] Given the branch push path, observe
      the delegated agent-user token authenticates the push over HTTPS and attributes it
      to `<dev-agent-user>`, with no token reachable by the role OS-user child (RFC-0001
      AC-26). RFC-0001 states this is verified by a live integration check against the
      kept S1/S3 spike assets, not the §9 doubles.

## Remit

File-disjoint allowed paths:

- `internal/identity/**`

## Dependencies

None for AC-8.1/AC-8.2 — buildable once US-0001/US-0002 land, since it needs no other
`internal/` package. AC-8.3/AC-8.4 are explicitly gated on S-credential-delivery
resolving; "the runner's push path stays unbuilt" until then (RFC-0001 §Identity
injection). The owner should split this story's done bar at the spike boundary rather
than treat it as a single all-or-nothing gate — flagged in the gaps list.
