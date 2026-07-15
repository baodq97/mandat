---
id: ADR-0004
title: Implementation stance — minimal implement, maximum value
status: accepted
owner: baodq97
date: 2026-07-15
---

# ADR-0004: Implementation stance — minimal implement, maximum value

## Context

Most code in this repo will be written by agents, and agents fail in both directions: they gold-plate (speculative abstraction, features nobody asked for, five files where one suffices) and they under-build (thin gates, skipped tests) — often in the same change. The design doc already commits to a thin runnable MVP (§10) and ADR-0001 established that a gate that cannot fire is a badge, not a gate. What is missing is the governed default stance for every implementation decision in between: how much to build, and when "minimal" is the wrong answer. The owner set the direction in working sessions; this ADR encodes it so the stance survives context resets.

## Decision

- **Default stance: maximize value per effort.** Every change starts from the smallest implementation that delivers the stated value. No speculative generality — build for the governed requirement that exists, not an imagined future one.
- **Scale rigor by door type.** Two-way doors (cheap to reverse: internal refactors, additive config, draft docs) get the minimal treatment and fast iteration. One-way doors (expensive to reverse: public contracts, schema and data migrations, the identity model, anything a customer depends on) get design-first treatment — alternatives weighed, and an ADR when the choice binds the future. When the door type is unclear, treat it as one-way.
- **Exception 1 — gates, tests, and quality floors are exempt from minimization.** Verification is never where effort gets saved; its investment follows risk, not the minimal stance. This is the same lesson the reference system paid for (§13, F1).
- **Exception 2 — asymmetric cost beats minimal.** Where the downside of under-building dwarfs the cost of building more — data loss, security boundaries, journal append-only integrity, unrecoverable states — invest past minimal up front.
- **Precondition: value must have a measure before implementation.** A change qualifies as value only when its measure is stated first — a metric, an acceptance criterion, a user-visible behavior, a gate that flips. No measure, no implementation: go back and define one. "Valuable" without a measure is an opinion, and opinions default to the human, not the agent.

## Consequences

- Smaller diffs and less speculative code to own and review; effort concentrates where reversal is expensive.
- Door classification becomes a real act with a misclassification risk; the unclear-means-one-way rule bounds it at the cost of occasional over-caution.
- The measure-first precondition adds friction before coding — accepted, it is the same paper trail govkit already demands of docs, applied to code.
- Gates and tests will sometimes look over-built next to the code they guard. That is the intent, not an inconsistency (Exception 1).
- Reviewers gain two concrete questions for every PR: "what is the measure?" and "which door is this?"

## Alternatives considered

- **Always-maximal engineering** (build it "right" the first time, everywhere): front-loads effort into two-way doors where iterating is cheaper than predicting. Rejected.
- **Pure minimalism, no exceptions**: exactly how the reference system ended up with a score gate that could not fire, and how asymmetric-cost domains get quietly under-built. Rejected.
- **Leave it as unwritten culture**: agents do not share culture, and unwritten stances do not survive context resets — the same reason this repo governs any decision as a doc. Rejected.
