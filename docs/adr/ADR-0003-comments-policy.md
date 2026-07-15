---
id: ADR-0003
title: Comments policy — self-documenting code, comments only for what code cannot say
status: accepted
owner: baodq97
date: 2026-07-15
---

# ADR-0003: Comments policy — self-documenting code, comments only for what code cannot say

## Context

This codebase will be read and edited primarily by AI agents — that is the product's own operating model, and the development process mirrors it. For an agent, comments are context it treats as ground truth: a stale or narrating comment does not just clutter, it actively biases the next agent's reading of the code. No gate can verify a comment (the compiler checks code, never prose), so every comment is unverified content with drift risk. The owner set the direction: the code itself is the documentation; keep comments scarce and load-bearing.

## Decision

- Meaning lives in names, types, and small functions. If a comment is needed to explain *what* code does, rename or restructure instead.
- A comment is justified only when it states something the code cannot show:
  - an **external contract** (a linker `-X` target, a schema another process reads, a wire format);
  - an **invariant or constraint** (append-only, must not block, ordering requirements);
  - a **non-obvious why** (an alternative that looks better but was rejected for a concrete reason).
- Forbidden: comments narrating the next line, restating the symbol name, justifying a change to a reviewer, or recording history (git owns history).
- Godoc comments on exported symbols follow the standard style where purpose is not self-evident — but the linter deliberately does **not** enforce blanket doc-comments (revive's `exported` rule and the ST1000-class staticcheck checks stay off). Forced boilerplate is precisely the noise this policy bans.
- Enforcement is a review lens, not a CI gate: no deterministic zero-false-positive check for "useless comment" exists, and a comment-count threshold would be a badge, not a gate (ADR-0001 philosophy).

## Consequences

- A smaller bias surface for every agent that reads the code; what comments remain are trustworthy signal.
- The naming bar rises — the cost of a clarifying rename is paid instead of the cost of a clarifying comment.
- Reviewers (human and agent) own enforcement; the reviewer checklist gains one lens.
- Risk of under-documentation is bounded by the contract/invariant rule: anything the code genuinely cannot express must still be written down.

## Alternatives considered

- **Enforce doc comments on all exported symbols** (revive `exported` on): consistent godoc coverage, but generates the restating-the-name boilerplate this policy exists to kill. Rejected.
- **Ban comments outright**: external contracts and invariants have nowhere to live; the ldflags version-stamp contract alone disproves this. Rejected.
- **A comment-density linter in CI**: not deterministic-zero-FP; a dense-but-correct constraint block would fail it. Kept as review judgment.
