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
- 2026-07-15 | Doc-writing work was dispatched to general-purpose (all
  tools) instead of the scoped spec-drafter agent, so a subagent wandered
  into dependency internals it did not need; the owner caught the waste |
  the dispatch step | dispatch the narrowest registered agent that fits and
  name its allowed files; treat govkit as a black-box gate | CLAUDE.md
  agent-orchestration rules (least-privilege dispatch, scope discipline)
- 2026-07-15 | Spike S1 shipped with its second question marked "unreachable"
  while a GA Graph API (agentUser) could answer it; the owner's UI retest and
  prodding reopened it the same day and flipped that half of the verdict | the
  spike's own completeness check before closing | before closing any spike
  question as unanswerable, enumerate documented alternative paths for each
  sub-question separately — a blocked path is not a blocked question | spike
  report round-2 section; this entry
- 2026-07-16 | An agent-produced diff passed the pilot's configured gate
  (go test) yet would have failed the repo's real CI at the fmt step — the
  per-repo gate list was a weaker approximation of `make check` | the repo
  registry's gate config | a pilot gate must include every blocking CI step
  or state which it omits; deeper fix is decoupling checkout-scope from
  edit-scope so the gate can be `make check` itself | pilot config gains a
  gofmt gate; decoupling tracked for an RFC
- 2026-07-16 | The v0.1.1 git-2.34 workaround forced core.bare=false on the
  shared mirror and every second provision's fetch then failed; first live
  runs of two tasks halted at setup | the workaround's own review — fixing a
  worktree symptom at mirror scope | scope a workaround to the layer that has
  the symptom (extensions.worktreeConfig + per-worktree override), and heal
  already-poisoned state idempotently before the op that trips on it |
  workspace.go invariant-pair comment + regression tests (v0.1.2)
- 2026-07-16 | A work-item AC written by the lead specified
  searchCriteria.status=all for the PR probe; the agent implemented it
  literally and the senior review caught that an abandoned PR on the reused
  branch name could false-certify a re-run | the AC author's own review — the
  spec was the bug, the implementation was faithful | red-team acceptance
  criteria the way docs are red-teamed: ask what states the query can return
  that the happy path never sees | FindPR queries active-only with a
  deterministic tie-break (v0.3.0 integration commit)
- 2026-07-16 | Agent-written test comments cited a nonexistent governed doc id
  (US-0018, conflating the work-item number with the US id) and a second agent
  copied it as established style; five sites landed before integration caught
  it | the review pass of the first branch that introduced it | comments citing
  doc ids are load-bearing for future agents — verify every cited id exists
  before merge; one wrong citation propagates by imitation | fixed in the
  v0.2.0 integration commit; this entry
- 2026-07-16 | reviewerIdentity compared an Entra object id against a UPN
  field, which would have made the writer!=scorer guard vacuously pass forever;
  only the live WIQL-by-OID failure a day earlier made the class visible and
  the fix was locked by a regression test | the contract between config
  identity fields and their comparison sites | when one identifier class bug
  appears (OID vs UPN), audit every comparison the two field kinds feed before
  the next one fails silently | regression tests lock reviewerIdentity to
  AgentUserName (v0.3.0)
- 2026-07-16 | The lead committed a doc change onto whatever branch a
  concurrently-running verification agent had checked out in the SHARED
  working tree; the stray commit rode an agent branch and the verifier's
  first gate run raced a mutating ref | the dispatch step - verification
  agents that checkout branches and the lead's own git ops cannot share one
  checkout | any agent that switches branches gets its own git worktree
  (mirroring the product's own per-task worktree invariant), and the lead
  serializes own-tree git ops while such agents run | this entry; verifier
  briefs now recommend worktree isolation
