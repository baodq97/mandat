# Learning loop — escape log

One entry per escape or friction: something a gate, rule, or agent should have
caught and did not. The distill pass (swe-flow:distill-learnings) clusters these
with the `.govkit/journal.jsonl` sensor data and proposes fixes at the cheapest
surface — a CLAUDE.md rule, a corpus fixture, a govkit.yml tweak, a ledger entry.
Entries are append-only; corrections rewrite the entry honestly, never fork it.

Format: `date | what escaped | where it should have been caught | lesson | encoded at`

## Entries

- 2026-07-15 | PRD-0001 reached the owner for ratification with only the
  deterministic floor run; no independent agent had reviewed substance, and the
  doc's co-author was its only reader | the status-advance path | every status
  advance needs an independent red-team pass attached | CLAUDE.md governed-docs
  rule (commit b977f77)
