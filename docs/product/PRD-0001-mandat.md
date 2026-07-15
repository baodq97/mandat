---
id: PRD-0001
title: Mandat — tracker work items to reviewed pull requests under revocable mandates
status: draft
owner: TBD
date: 2026-07-15
---

# PRD-0001: Mandat — tracker work items to reviewed pull requests under revocable mandates

## Problem

Teams that want tracker-to-PR automation today hand an agent a personal access token or a shared bot account: a standing, broadly scoped, unattributable credential that no one can revoke without breaking the bot. The reference-system audit (§13) records where that path ends. A score gate that could never fire, agent self-reports trusted as ground truth, an unauthenticated control surface, and RBAC left vestigial because one token did everything. Mandat's thesis is the inverse and the name states it: authority to act is granted per role, scoped to a remit, and revocable at will, with the journal as the enforcement record. The instrument is a Microsoft Entra agent identity sponsored by a named human (D2, D5), so a hijacked agent can at worst edit inside its own sparse checkout and open a PR it cannot merge.

## Persona and audience

The MVP ships against real stacks, not mocks. The primary deployment is dogfood: the owner, a tech lead, runs Mandat against his personal Azure DevOps organization and his personal Entra tenant on a real Linux VM. That operator stands in for the ideal customer profile this product targets, the Azure-shop engineering team lead who wants tracker-to-PR automation and refuses to give agents PATs or unbounded access. Both share the same ground: an ADO tracker, an Entra tenant, one Linux VM per the single-binary distribution decision (D3), and the demand that every agent act under a scoped, revocable mandate rather than a broad standing credential. Designing for the dogfood operator and the ICP together keeps the product honest, because the owner feels every rough edge the customer would.

## Success metrics

Four metrics govern the MVP, and each reads from a ground-truth surface that already exists in the design: the journal (§4.9), the run-scoring records (§4.7), or the runs table (§6). None depends on an agent's self-report, which is the failure the design corrects (§13, F2). Every target below is a proposal the owner may adjust when he ratifies this PRD; the metric and its measurement source are fixed, the numbers are not. Naming each measure before any code satisfies the measure-first precondition ADR-0004 sets.

| Metric | Target (proposed, owner-adjustable at ratification) | Measured by |
|---|---|---|
| Install to green doctor | ≤ 10 minutes on a clean VM | wall clock during pilot; makes the 10-minute pilot promise of D3 testable |
| Time to first draft PR | p50 ≤ 30 minutes from consent tag to draft PR | the journal |
| Clean-run rate | ≥ 60% of dispatched runs reach in-review without needs-human, after the first month of calibration | journal plus scoring records |
| Cost per merged PR | ≤ $5 for a size-S story | the token-cost column of the runs table |

## Scope and non-goals

Scope is the thin runnable slice that §10 and ADR-0004 both commit to, drawn at exactly the §10 boundary: the ADO adapter, the PO and Dev roles, client-credential identity mode, draft-PR autonomy only, the verification probes with gate re-run, the journal, and the status page, running on one customer VM against one team project. That slice proves the whole pipeline end to end, from a consent tag to a reviewed draft PR, while touching the fewest planes needed to make the mandate mechanism real.

Everything on the design's ordered roadmap is an explicit non-goal for the MVP, deferred not dropped: Arc identity mode, the QA and Reviewer roles with scoring calibrate, the planning view with scheduled waves, Teams notification, the Jira adapter, agent-user assignment UX, and the multi-VM runner pool. The runner pool waits until a real customer saturates one VM, per the accepted single-VM trade-off (§12); the others follow in the §10 order once the MVP holds.

## Operating model: the loop this repo already ran by hand

Mandat serves other repos the same way this repo built itself. The product's end-to-end flow (§5) is not a paper design: every step already ran manually in this repo's own git history, with a human orchestrating agents the way the orchestrator plane will.

| Product mechanism | Already exercised here |
|---|---|
| PO interviews the owner, output lands as a proposal (§4.4) | this PRD came from a four-question owner interview; the answers are encoded above and the doc waits at `draft` |
| An agent proposes and never self-ratifies (§4.4) | commit de9de0f landed the harness and four ADRs at `proposed`, owner TBD |
| A human status flip is the only act that advances a gate | commit 1feed31 is a separate accept commit citing the owner's in-session ratification |
| Verification re-runs gates and never trusts summaries, writer ≠ scorer (§4.7) | a 16-agent adversarial review re-ran every gate, proved the gate can fail by injecting a defect, and its confirmed findings were fixed and independently re-verified |
| Roles carry model tiers (§4.4) | the lead model decides and integrates; senior and middle tiers draft, research, and verify |
| The journal records actor, act, and authorization (§4.9) | git history carries all three per commit |

The adoption path climbs the same autonomy ladder the role plane defines (§4.4): manual, where a human drives every step as this repo's history did; then `report`; then `draft-pr`, the MVP ceiling; then `unattended`. A rung is earned, not granted: the clean-run metric must hold at the current rung before a role's ceiling rises, and ceilings only change by config edits the customer reviews like code (§4.8). The reference system climbed on optimism and its §13 audit is the record of the fall; Mandat climbs on gates.

## Prerequisites

Verification needs ground truth, so this section pins what must exist before the spikes and the MVP verification can run. Per §11 the spikes S1 through S5 gate MVP implementation: each carries a kill risk that would reshape the architecture, so none of the MVP code is written until they clear. On the deployed VM, `mandat doctor` is the standing re-verification of this same set, because §4.10 maps every doctor check to a spike, which makes a green `mandat doctor` run the deployed proof that the prerequisites still hold.

| Prerequisite | Status | Verified by |
|---|---|---|
| Entra tenant with agent-identity preview enabled (feeds S1, S2) | available | owner administers the tenant; agent-identity subtype visible in it |
| Personal ADO org and project with full admin (feeds S1, S3) | available | owner holds full org admin on the collection |
| Arc-capable Linux VM (feeds S2, S4) | available | Arc agent enrolled and reporting; IMDS reachable at localhost:40342 |
| Dedicated Anthropic API budget, separate from any company account (needed before S4 and pilot) | assumed, to arrange | dedicated billing account provisioned before S4 runs |
| Claude Code CLI present on the VM | derived | `mandat doctor` |
