# AC ownership probe — can a deterministic normalizer settle card↔doc AC "containment"?

Answers the open kill-criterion the PRD-0002 red-team left: is a board card's scoped
acceptance criteria "contained in" its source doc's authored AC decidable by deterministic
means, or is that comparison irreducibly semantic — meaning AC must be repo-canonical with
the card as a read-only digest and no containment check at all?

**Verdict: NOT VIABLE.** No deterministic rule separates "contained" from "not contained"
on the real pair. Recommendation: AC is repo-canonical; the card carries a read-only,
planner-authored digest that cites the doc AC-ids it derives from (the #36 card already
does); no automated containment check. Drift is prevented at authoring time, not detected
after.

## Method

One real card↔doc pair, fetched live, normalized and compared deterministically. No LLM.

- **Card**: ADO work item #36 (`mandat-pilot`, "US-0013 slice 1: mandat init skeleton"),
  `Microsoft.VSTS.Common.AcceptanceCriteria` field via the ADO REST API (api-version 7.1).
  Rich-text HTML, an `<ol>` of 6 items, slice-1 scoped to the init flag surface.
- **Doc**: `docs/issues/US-0013-first-run-init.md`, `## Acceptance criteria` — 14 authored
  ACs (`- [ ] AC-13.1 … AC-13.14`).
- **Normalizer**: strip HTML tags / Markdown checkbox+list markup, HTML-unescape, lowercase,
  map all non-alphanumerics (hyphens, dots, underscores) to spaces, collapse whitespace,
  tokenize on whitespace, drop a small stopword set. Then three deterministic rules per card
  clause: token-set containment (fraction of card tokens present in a doc AC / in the union of
  all 14), longest verbatim shared substring, and a literal atomic-token probe on the flag
  clause. Script: throwaway, `scratchpad/normalize.py`.

**Is #36 the only real pair?** Yes. Scanned all 24 pilot work items (#13–#36): only #36's AC
references a `US-00xx`/`AC-x` source. Every other card carries self-authored slice AC, not
doc-derived. #36 (a 6-item scoped restatement against a 14-AC spec) is the canonical hard
case and the only one that exists.

## The real pair

**Doc, AC-13.9 (authored):**
> Given `mandat init --non-interactive`, observe it requires every irreducible field
> (tracker org/project, repo url + remit paths + gates, per-role identity ids/UPNs) as a flag
> and errors naming the specific missing flag instead of prompting…

**Card, item 1 (the scoped restatement of AC-13.3(c)+AC-13.9):**
> `mandat init --non-interactive` accepts one flag per irreducible field per US-0013 AC-13.3(c)
> and AC-13.9: `--tracker-org`, `--tracker-project`, `--auth-mode`, `--entra-tenant`,
> `--entra-blueprint`, `--repo` (key=url), `--base-branch`, `--remit-path` (repeatable),
> `--gate` (repeatable), and per-role `--dev-identity-id/--dev-user-id/--dev-user-upn/…`,
> `--autonomy-ceiling`, `--max-usd-per-run`. A missing required flag errors naming that exact
> flag; exit non-zero; nothing is written.

Same requirement; the card materializes into concrete flag names what the doc states as prose
("tracker org/project", "per-role identity ids/UPNs"). A human reads item 1 as trivially
contained in AC-13.3(c)+AC-13.9. The normalizer cannot confirm it — see below.

## Normalizer results (real numbers)

Token-set containment per card item, best single AC and against the union of all 14 ACs:

| Card item | Content | Best single-AC | vs union of 14 | Human verdict |
|---|---|---|---|---|
| 1 flag surface | init flags → irreducible fields | 57% (AC-13.3) | **85%** | contained |
| 2 non-TTY | stdin-not-TTY implies non-interactive | 57% (AC-13.9) | **70%** | contained |
| 3 round-trip + constants | emit round-trips config.Load | 58% (AC-13.3) | **91%** | contained |
| 4 comment-per-field | every omitempty field commented | 81% (AC-13.2) | **95%** | contained |
| 5 truncated → FieldError | aborted file yields FieldError.Path | 36% (AC-13.4) | **50%** | contained |
| 6 make check + tests | gate green + test list | 35% (AC-13.9) | **55%** | **NOT in doc** |

**No threshold classifies the real pair correctly — the residual is sign-inverted.** The one
genuinely-contained clause below any reasonable bar (item 5, a faithful restatement of AC-13.4,
50%) scores *lower* than the one clause that is genuinely absent from the doc (item 6, a slice
Definition-of-Done — `make check` green plus a test list — that no authored AC states, 55%).
Because 55% > 50%, any monotone containment rule that passes item 5 also passes item 6, and any
rule that fails item 6 also fails item 5. The metric is not merely incomplete in the decision
region; it is anti-correlated with truth there.

At the most permissive defensible bar (union containment ≥ 70%), **2 of 6 clauses (33%) are
unadjudicated** (items 5 and 6), and their correct verdicts are *opposite* — so the
unadjudicated fraction is not a residual to shrink with tuning; it is exactly the fraction where
the metric gives no usable signal.

### Why the contained clauses don't match textually (the examples the kill-criterion asked for)

- **Flag names vs prose** — literal flag strings present in the doc: **0 of 17**. The doc never
  writes `--tracker-org`; it writes "tracker org/project". Loosen to "are the flag's constituent
  words present anywhere in the doc" and **13 of 17** pass — but the 4 misses are pure
  normalization artifacts, not semantic gaps:
  - `--gate` misses because the doc writes "gate**s**" (plural; no stemmer).
  - `--dev-user-upn`, `--reviewer-user-upn` miss because the doc writes "UPN**s**".
  - `--max-usd-per-run` misses only because "per" is in the stopword list — flip that one
    tuning knob and it matches. Whether these flags "match" is a function of how aggressively
    you normalize, not whether the requirement is contained.
- **Item 5, same requirement, disjoint vocabulary (36% / 50%)** — card: "A generated-then-
  truncated file (simulate an aborted run) fed to config.Load yields FieldError values whose
  Path names the exact dotted field … do not build new error types." Doc AC-13.4: "init writes
  a config with a missing irreducible field (interview aborted early …) … config.Load … returns
  a ValidationErrors value whose FieldError.Path names the exact dotted field." Identical
  requirement; the shared span is the copied identifier "path names the exact dotted field"
  (35 chars) and little else. The card's operational framing (truncated file, simulate, do not
  build new types) and the doc's spec framing (missing field, ValidationErrors) barely overlap.
- **Exact-substring rule fires only on copied identifiers, not on requirement prose.** Longest
  verbatim shared spans per item: 29, 47, 26, 53, 35, 17 chars — and they are config keys and
  quoted values both sides copy ("tracker states in progress doing runner pool size 1", "path
  names the exact dotted field"), never the requirement's own wording. The rule of the requirement
  ("a missing required flag errors naming that exact flag; exit non-zero; nothing is written")
  shares no verbatim span with AC-13.9's phrasing of the same rule.

### The relation itself doesn't hold

Item 6 exposes a second, structural reason "contained in" is the wrong test. A scoped slice card
is not a subset of the doc's ACs: it legitimately *adds* material (a `make check` gate, a
per-slice test list) that no authored AC states, and it *narrows* scope (6 of 14 ACs). A card is
a scoped projection plus slice-authoring detail, not a subset. Even a perfect containment oracle
would flag item 6 — a desirable, correct card addition — as a "violation." Containment fails not
only because it is deterministically undecidable, but because the target relation is false by
design for a scoped card.

## Second, independent fact: a containment check is net-new plumbing regardless

The verdict above does not depend on build cost, but the build cost is real either way. No
doc-side Markdown AC reader exists anywhere in the tree — `grep` over `internal/adapter`,
`internal/task`, `cmd/` finds AC handling only on the card side: `internal/adapter/azuredevops/
poll.go:122` reads `Microsoft.VSTS.Common.AcceptanceCriteria` into `TaskContract.Acceptance`, and
`internal/runner/invocation.go:72` pastes it into the prompt. Nothing parses
`docs/issues/*.md` `- [ ] AC-`. And `cmd/mandat/serve.go` `dispatchCycle` (lines 543–548) discards
the entire re-read contract — including the re-read card AC — for any task already in the store
(`LoadTask` succeeds → `continue`); only not-yet-dispatched contracts reach `runTask`. So any
containment check is net-new on both sides: a Markdown-AC parser for `docs/issues` that does not
exist, plus a change to stop discarding the re-read card and diff it. Build cost lands
independently of whether the comparison is decidable.

## What this means for PRD-0002

**Drop the containment-check mechanism from PRD-0002.** Make AC **repo-canonical**: the authored
`- [ ] AC-x` in `docs/issues/US-xxxx.md` is the source of truth. The board card carries a
**read-only, planner-authored digest** that **cites the doc AC-ids it derives from** — exactly
what the #36 card already does ("per US-0013 AC-13.3(c) and AC-13.9"). That citation is a
write-time provenance link, not a content-containment proof; it is what a normalizer cannot
reconstruct after the fact. Drift is prevented by the planner authoring the card *from* the doc
at breakdown, not detected after by a comparison engine.

**No RFC is needed to design a containment mechanism** — there is nothing viable to design; this
probe is the kill. If PRD-0002 wants to formalize the alternative, the artifact is a short
authoring-convention note (card digest cites doc AC-ids; generation is one-directional
doc→card), not a comparison-engine RFC.
