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
- 2026-07-16 | The lead dispatched a test-writing agent with "the merge has
  landed, trust the working tree" before actually running the merge; the agent
  read main, found neither feature, and correctly halted instead of inventing
  the missing production surface | the dispatch step - the lead's own
  pre-dispatch checklist | state-dependent dispatches name the exact commit or
  branch the agent must see, and the lead verifies that state exists before
  sending; an agent that halts on a false premise is doing its job | this
  entry; the halting behavior itself is the swe-flow implementer contract
  working as designed
- 2026-07-16 | A hand-rolled requeue (delete store row + rm worktree dir)
  missed the mirror's worktree metadata and the stale task branch, so the
  re-dispatch died at setup two seconds in (worktree add -b on an existing
  branch) and the actual fix under test (a raised budget cap) never ran |
  the ops runbook - a multi-step manual recovery encoded only in an agent's
  memory | recovery procedures that touch more than one store belong in the
  binary as a command (mandat requeue), not in a runbook; until then the
  runbook lists ALL four steps: store row, worktree dir, worktree prune,
  branch -D | board item #25 (requeue command) carries the evidence; this
  entry
- 2026-07-16 | A four-concern work item (pool loop + drain + per-task env
  isolation + budget admission, ~540 lines with tests) died mid-run twice at
  the same ~17-18 minute mark with no ResultContract, indifferent to a 2x
  budget raise; two runs of spend produced zero shipped value | the work-item
  authoring step - the PO sized a story for a senior engineer and handed it
  to a bounded headless junior | size work items for the role's real ceiling
  (single-concern, one file area, one test surface); when a run dies with no
  contract, do not re-run bigger - split or escalate the model tier; salvage
  the paid partial diff as reviewable draft input | this entry; WI 28 record
  carries the timeline
- 2026-07-16 | The first live pool=2 run hit the mirror config.lock race the
  red-team predicted (F2) one layer deeper than anyone scoped: the runner's
  post-provision `git config` writes (credential helper, author) run in a
  LINKED worktree, and linked worktrees write the SHARED repo config - so one
  task's credential setup raced the sibling's mirror heal. The concurrency
  tests passed on timing luck: config.lock is held for microseconds and -race
  only sees memory races, not file locks | the batch-1 lock-scope review -
  "whole per-repo touch" missed writes that LOOK worktree-local but land on
  the shared file | enumerate shared-file writers by FILE TARGET, not by call
  site: any `git config` without --worktree in a linked worktree is a shared
  write; scope all per-task config to --worktree (the extension batch 1
  enabled exists for exactly this) | runner gitConfig fix (this arc); this
  entry
- 2026-07-16 | Under live pool=2 one task's worktree lost its branch and the
  agent's commit became an orphan root commit (complete tree, no parent,
  full-tree content incl. out-of-remit paths) - and NOTHING downstream
  noticed: gates green, PR opened, probe confirmed existence+creator,
  in-review. REWRITTEN after investigation: the lead's first hypothesis
  (racy symbolic-HEAD reads outside the lock) was WRONG - the proven
  mechanism is the mirror refresh's `fetch --prune`, which on a mirror
  refspec deletes every in-flight task branch the origin lacks; prune
  timing picks the symptom (before read-tree: setup_failed pre-spawn = the
  CI flake; after: unborn-HEAD orphan commit). Sequential runs escaped only
  because each branch was pushed before the next fetch | the v0.1-era
  ensureMirror review - reflexive `--prune` hygiene on a mirror whose
  local refs are live state | on a mirror that HOSTS local state, fetch
  must not prune; and verification needs an ancestry check (a PR with no
  merge base to the base branch is never result_ok) | fix + red-proven
  regression test in v0.5.0; ancestry check = board backlog item; this
  entry rewritten per the corrections rule, wrong hypothesis owned
- 2026-07-16 | The first in-place upgrade of the always-on era failed with
  ETXTBSY: install.sh cp'd over the binary the systemd unit was executing,
  and the VM silently kept running the old version while the script reported
  the new one | install.sh's copy step - written when serve only ran in
  foreground sessions | daemonizing a binary changes its upgrade contract:
  replace by staged rename (atomic, old inode lives until restart), never
  in-place copy; and restart the unit as part of the upgrade runbook | fix
  in install.sh (stage + mv -f); this entry
- 2026-07-16 | The ten chartering US (US-0001..0010) sat at open|TBD through
  24 PRs and ten releases while every plane they chartered shipped, and one
  AC (US-0008 8.2, the MCP tracker wiring) silently diverged from what was
  actually built; the owner's direct question was the only detector. The
  contrast is damning: US-0011/0012, driven as governance dogfood, got eight
  lifecycle commits (draft, owner, red-team folds, in-progress, done) while
  US-0001..0010 got exactly two - birth and today's bulk accept | the arc's
  definition-of-done: code done-ness had gates, reviewers, and a release
  train; doc done-ness had no trigger surface at all (govkit verify is
  structural, the doc-keeper agent exists but nothing invokes it, the
  promote/release ritual never asks which US it completed) | a release is
  not done until the docs it satisfies are reconciled: every promote ends
  with a doc-keeper pass (which US do these changes complete? propose flips
  or corrections with evidence); an AC nothing implements is drift to
  surface, not silence | this entry; harness fix pending owner's pick
  (release-ritual doc-keeper step vs a govkit staleness report)
- 2026-07-16 | US-0013 (first-run init) was drafted and landed straight from
  the owner's observation with zero research input: the peer survey (how
  gitlab-runner, gcloud, gh, flyctl actually do first-run) only started when
  the owner asked "research first", so the doc's ACs encode first-guess UX
  rather than evidence | the doc pipeline's entry edge - the red-team pass
  gates a status ADVANCE, but nothing gates DRAFTING on evidence, so
  observation went straight to prescription | a governed doc that charters
  new product surface starts from a research artifact (peer survey, repo
  measurement, spike note) landed in docs/research/ and cited as a Source;
  the red-team pass verifies the citation exists | CLAUDE.md governed-docs
  rule (this commit); the running survey feeds AC deltas back into US-0013
