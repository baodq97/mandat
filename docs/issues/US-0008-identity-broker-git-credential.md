---
id: US-0008
title: Identity broker — ADR-0005 token mint, tracker MCP wiring, git credential delivery
status: open
owner: TBD
date: 2026-07-16
priority: P2
---

# US-0008: Identity broker — ADR-0005 token mint, tracker MCP wiring, git credential delivery

As a mandat contributor, I want the broker to mint delegated agent-user tokens on demand
and keep them inside its own process boundary, so that the child process never holds a
credential it could leak, for both tracker access and the git push path.

Spike S-credential-delivery is resolved (2026-07-16): the `mandat git-credential` helper
delivers the delegated token to git as a Basic-auth password on git 2.43, the token never
touching the child env, argv, `.git/config`, or disk. The former spike gate on AC-8.3/AC-8.4
is lifted; RFC-0001's condition that "the runner's push path stays unbuilt" no longer holds.

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

## Acceptance criteria — git credential delivery (unblocked by S-credential-delivery)

- [ ] AC-8.3 Given the clone/fetch path, observe the credential reaches git only through
      the `mandat git-credential` helper answering the git credential-helper `get` protocol
      (the agent-user UPN as `username`, the delegated agent-user token as `password`,
      git sending `Authorization: Basic`); after the operation the token is absent from the
      child environment, from argv, from `.git/config`, and from everywhere under `.git/`
      (RFC-0001 §Identity injection, AC-15; mechanism proven invariant-preserving on git
      2.43 by S-credential-delivery). §9 double: a local bare git origin plus the stub
      broker.
- [ ] AC-8.4 Given the branch push path, observe `git push` authenticates over HTTPS
      through the same `mandat git-credential` Basic-password helper and attributes the push
      to `<dev-agent-user>`, with the delegated token never reachable by the role OS-user
      child, in the child env, in argv, or under `.git/` (RFC-0001 AC-26; mechanism proven
      by S-credential-delivery). RFC-0001 states the AC-26 attribution is verified by a live
      integration check against the kept S1/S3 spike assets, not the §9 doubles.

## Remit

File-disjoint allowed paths:

- `internal/identity/**`

## Dependencies

None for AC-8.1/AC-8.2, buildable once US-0001/US-0002 land, since it needs no other
`internal/` package. AC-8.3/AC-8.4 were gated on S-credential-delivery, now resolved
(2026-07-16): the `mandat git-credential` Basic-password helper delivers the delegated
token invariant-preserving on git 2.43, so the runner's push path is buildable. The owner
may still split this story's done bar at the former spike boundary (broker mint plus MCP
wiring versus git credential delivery) if useful for sequencing.
