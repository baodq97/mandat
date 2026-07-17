---
name: mandat-slice-close
description: >-
  Close a mandat implementation slice into one owner-decision packet before any governed-doc
  status flip. Use this whenever a code slice has landed on main and one or more governed docs
  (US/RFC/ADR/PRD) are candidates to advance status (open->in-progress, ->done, proposed->accepted),
  or when the user asks to verify + reconcile + red-team before ratifying, or says "close this
  slice", "prep the flips", "run the close workflow", "ready to flip". It fans out a gate-verify,
  a doc-keeper drift reconcile, and one red-team per flip candidate — so the owner ratifies ONCE
  from a single packet instead of per-doc. Reach for it any time you are about to propose a mandat
  status flip; skipping it risks flipping a doc to done whose ACs the code no longer matches.
---

# mandat-slice-close

Dev-time mirror of the product's own thesis: a status flip is a ratification, and a ratification
needs evidence. This skill collapses the repetitive pre-flip tail — re-run the gates, reconcile
doc drift, red-team each flip — into one packet the human owner decides from in one pass.

It orchestrates the `mandat-slice-close` workflow (`.claude/workflows/mandat-slice-close.js`),
which fans out three registered agents (`gate-verifier`, `swe-flow:doc-keeper`, `red-teamer`).
The lead runs the workflow, presents the packet, and — on the owner's single authorization —
applies the reconcile edits and lands the accept commits. The lead never self-flips a status.

## When to run it

Run it after a code slice lands (committed on main) and before proposing any governed-doc flip.
Do NOT run it while agents are still editing the tree — the gate-verifier reads the working tree,
so a mid-edit tree yields a false BLOCK. Commit the slice first, then close it.

## How to run it

Invoke the workflow by name with two args:

```
Workflow({
  name: 'mandat-slice-close',
  args: {
    changeSummary: 'One paragraph on what the landed code actually does — the mechanism, the seams, the live-proof. This is what the red-teamers reason from, so be concrete (symbols, file paths, what was verified live).',
    flips: [
      { us: 'US-0015', target: 'done',        doc: 'docs/issues/US-0015-...md' },
      { us: 'US-0014', target: 'in-progress', doc: 'docs/issues/US-0014-...md' },
    ],
  },
})
```

`changeSummary` matters most: a vague summary gives vague red-teams. Name the actual functions,
the live-verified behavior, and the known limitations. The workflow runs in the background and
notifies on completion; its full result lands in the task output file — read it, don't rely on
the truncated notification.

## The packet it returns

```
{ gate:      { verdict: SAFE-TO-COMMIT | BLOCK, gates, findings[] },
  reconcile: { edits: [{ doc, reason, proposed }] },        // exact replacement text, not applied
  redTeam:   [{ us, target, verdict, acSummary, reconciledText, sourcesExist, killCriterion }],
  humanGates: ["US-xxxx -> target", ...] }
```

Red-team `verdict` is one of:
- `flip-as-is` — the status is honest; flip needs no doc change (still apply any `reconcile` drift edits).
- `flip-after-reconcile` — the status is honest ONLY after the ACs are reworded to what shipped;
  apply `reconciledText` first, then flip.
- `blocked` — a real gap; do not flip. Fix the code (or scope down the claim) first.

## Acting on the packet

1. Present the packet to the owner as ONE decision: the flips + any `flip-after-reconcile` edits
   + any live-surfaced limitation. Recommend, with trade-offs; the owner authorizes.
2. On authorization, and ONLY then: apply the `reconcile.edits` and every `reconciledText` to the
   docs (a governed doc must certify exactly what shipped — never round a partial AC up to done).
3. Land each flip as a **separate accept commit** that both edits the front-matter status and
   updates the matching `INDEX.md` row, with a message citing the owner's in-session
   authorization (govkit propose-then-ratify: AI proposes, human ratifies, the flip lands in its
   own accept commit).
4. Re-run `npx govkit check` after the flips; verify remote == local after push.

## Why one packet, not per-doc

The owner is the bottleneck when every flip is surfaced separately. Batching the whole verify +
reconcile + red-team tail into one packet turns N interruptions into one ratification, while
keeping the two gates that actually protect the record: an independent gate-verify and an
independent red-team per doc, neither authored by whoever wrote the diff.
