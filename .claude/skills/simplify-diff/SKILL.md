---
name: simplify-diff
description: Use BEFORE opening or proposing any PR — a pre-PR self-review of the diff for reuse, simplification, comment policy, and scope, so review rounds catch design issues instead of mechanical noise. Tier 1, dependency-free.
---

# Simplify the diff before the PR

One pass over `git diff <base>...HEAD` (plus staged/unstaged changes) by the
author, before anyone else sees it. This is a self-check, not the full
four-agent `/simplify` review — it exists so the independent review spends its
attention on design, not on lint-shaped findings. Five checks, all mechanical:

1. **Reuse.** For every new helper or repeated block: grep the package and its
   neighbours first. If an existing helper does the job, call it. Three
   near-identical literals are one extraction late.

2. **Simplification.** Delete dead code, commented-out scaffolding, and state
   derivable from other state. Collapse copy-paste-with-variation into the one
   form that varies. Prefer the stdlib call over the hand-rolled loop
   (dependency ladder rung 1).

3. **Comment policy.** Every comment must state an external contract, an
   invariant, or a non-obvious why — never narrate a line. **Verify every
   cited doc id (US-/RFC-/ADR-/AC-) actually exists** — a wrong citation
   propagates by imitation (live escape, 2026-07-16: five sites cited a
   nonexistent US-0018).

4. **Format + gate.** `gofmt -l` on every changed .go file must list nothing;
   run the repo gate (`make check` here) before pushing, not after the PR
   opens.

5. **Scope.** `git diff --name-only` lists ONLY files the task names. Anything
   else is either a revert or a separate task.

Fix what the pass finds, re-run the gate, then open the PR. If a finding is a
real design question (wrong altitude, missing seam), do NOT silently fix it in
the same diff — surface it to the lead/owner as a note on the PR.
