---
id: PRD-0002
title: "Board-driven SDLC operating model: the board as operational and ratification surface"
status: draft
owner: TBD
date: 2026-07-16
---

# PRD-0002: Board-driven SDLC operating model

Extends PRD-0001 (the mandate pipeline) with the operating model that runs it. Source of record: `docs/research/board-driven-sdlc-operating-model.md` (design synthesis, 2026-07-16), which carries the work-station map, the field-ownership matrix, the rich-card projection, the process/state model, and the permission facts verified against Azure DevOps this session. This PRD charters that synthesis; it invents nothing the research doc does not state.

## Problem

The pilot ran the board as flat Basic Issues, one card per implementation slice, and migrated Basic to Agile this session (research doc, Motivation). Two gaps remain. First, no work stations: a reader cannot tell from the board which card is a brainstorm, which is a PRD or tech doc, and which is implementation, so the governed chain PRD → RFC → ADR → US → Code has no visible home on the board. Second, cards are information-poor: the board should be the surface an operator understands and acts on in roughly 10 seconds, but a card today carries a title and a state and little else, while the design detail lives only in the repo docs. A consequence of both: in-review and needs-human exist only as free-text comments, so the two states that most need operator attention are the least legible on the board.

The board is not a report of the work. Under the design thesis mandat already holds (spec §4.4), a human status flip on the tracker is the only act that advances a work-item stage gate, which makes the board both the operational projection of work and the ratification surface. Governed-doc status is the one advance that needs a second act beyond the drag: the accept commit the field-ownership matrix names below. This PRD defines what that projection must carry to be worth deciding from.

## Field-ownership principle

One canonical home per field, never a mode-switch source of truth. The tempting alternative, where the source of truth moves between board and repo per artifact, is the most expensive to operate: every consumer must first ask "who is canonical now?", and that question is the drift generator (research doc, Principle 1; LEARNING-LOOP entry 16 is this class). The fix is field-level ownership, so no field has two homes and there is no conflict to resolve.

| Field | Canonical home | Sync direction |
|---|---|---|
| Work-item state (`queued → in-progress → in-review → needs-human → done`), assignment, priority, backlog rank | Board (the human act is the gate or the grant) | board → mandat (work-item lifecycle; spec §4.4) |
| Governed-doc status (PRD/RFC/ADR/US: `draft → approved/accepted/done`) | Repo accept commit (govkit-gated) | board drag is the ratification signal; the accept commit the human authorizes is the authoritative advance. mandat never self-flips governed-doc status (CLAUDE.md) |
| Acceptance specification | Repo doc (authored, red-teamed) | doc → read-only card digest at breakdown. The planner authors a slice-scoped digest onto the card that cites the doc AC-ids it derives from (write-time provenance); the adapter lifts that card digest into the `TaskContract` as the runtime dispatch contract, a projection, not a competing source. Drift is prevented at authoring, not detected after (probe, `docs/research/ac-ownership-probe.md`) |
| Doc body, design detail, decision rationale | Repo (where the agent works) | repo → board digest |
| Operational evidence (PR link, verify verdict, cost, journal) | Journal / mandat | mandat → board comment |

The State row splits two state machines the PRD must not conflate. The board work-item lifecycle (`queued … done`) is board-canonical, and spec §4.4's "only a human tracker flip advances a stage gate" governs it. A governed doc's STATUS is separate: the board drag is the human's ratification signal, but the authoritative advance is the human-authorized accept commit in the repo that govkit gates. mandat never self-flips governed-doc status (CLAUDE.md); the drag authorizes the commit, the commit advances the status.

The card digest the adapter lifts into the `TaskContract` is the runtime dispatch contract for one slice, a projection of the doc's authored spec, not the spec itself. AC is repo-canonical: the authored `- [ ] AC-x` in `docs/issues/US-xxxx.md` is the source of truth, and the planner authors the card digest from it at breakdown, citing the doc AC-ids it derives from (the #36 card already cites "US-0013 AC-13.3(c) and AC-13.9"). There is no automated card-vs-doc comparison at any time. A probe (`docs/research/ac-ownership-probe.md`) killed the AC-containment check an earlier draft proposed: no deterministic normalizer separates a contained clause from an uncontained one, because the residual is sign-inverted (a contained clause scored 50%, below an uncontained one at 55%), and a scoped card legitimately adds slice material (a Definition-of-Done) that no AC states, so "contained in" is the wrong relation. Correctness is an authoring discipline: a faithful digest that cites its source AC-ids at write time.

The runtime dispatch contract is the card, not the doc: the adapter lifts the card's AC field into the `TaskContract` at dispatch, and the doc's authored AC is never read at runtime. No automated check catches an unfaithful or stale card digest; the design relies on two controls, not one. Prevention: the planner authors the digest from the repo-canonical doc AC at breakdown (above). Backstop: the human at the Done-ratification gate reads the card's governed-doc link (rich-card projection, research doc Principle 3) and confirms the delivered work matches the authored AC before dragging to Closed. The Card self-sufficiency metric's "decide without opening the repo" (Success metrics, below) covers routine operational triage: moving a card through in-progress and in-review, not the Done gate. Ratifying Done is the one point in the flow that consults the linked doc AC. Falsifier: if dogfood measurement shows unfaithful or stale digests reaching a ratified Done at a real rate, this provenance-only model is abandoned, not patched, in favor of dispatching from the repo-canonical doc AC at runtime or a keyed semantic fidelity check. Both are architecture changes deferred to implementation (`docs/research/ac-ownership-probe.md`).

## Persona and audience

Primary: the operator or PO who wants to run the SDLC from the board in roughly 10-second decisions, reading a card and dragging it rather than opening the repo. Secondary actors, doing the station work under mandate: the AI RoleAgents, planner, drafter, architect, and reviewer. The `.claude/agents` roles (spec-drafter, sa-architect, ba-analyst, red-teamer) are the dev-time mirror of the product RoleAgents these stations run (research doc, Principle 2).

As AI quality rises the agents do more of the chain and the human gates get rarer, but they do not disappear. Three gates stay human by construction and map to a board act: approve a PRD (drag the `doc:PRD-xxxx` Feature), accept the tech (drag the `doc:RFC/ADR-xxxx` Feature), and ratify Done (drag the User Story to Closed). For the two doc gates the drag is the ratification signal, not the status flip itself: the governed-doc status advances only when the human-authorized accept commit lands and govkit gates it (field-ownership matrix above). Assignment is itself a gate, not a new mechanism: Poll only picks up work items assigned to an agent UPN, so the human assigning a card is the act that grants the mandate (spec §4.4; research doc, Principle 2). Humans stand at gates; agents run inside them.

## Success metrics

Each metric reads from a surface the design already has (the journal, the config, or the board itself); none depends on an agent self-report. Targets are proposals the owner may adjust at ratification, the same stance PRD-0001 takes; the measure and its source are fixed, the numbers are not.

| Metric | Target (proposed, owner-adjustable) | Measured by |
|---|---|---|
| Card self-sufficiency | 100% of active-slice cards carry all four projections: story digest, governed-doc link, per-gate verify verdict, run cost, so the operator decides without opening the repo | card-projection audit against the journal and config sources (research doc, Principle 3) |
| One card per doc | every governed doc (PRD, RFC, ADR, US) maps to exactly one board object; 1:1, 0 orphans | work-station reconciliation of board objects against `docs/*/INDEX.md` (research doc, Principle 2) |
| AC provenance on every slice card | every active-slice card names the doc AC-ids it derives from; the audit confirms the provenance link is present, not that the digest faithfully restates the cited AC; fidelity is an authoring discipline, not a deterministically measurable property (probe killed the containment check that would have measured it) | card-provenance audit: does each active-slice card cite its source AC-ids (research doc, Principle 1; probe `docs/research/ac-ownership-probe.md`) |
| Gate states are board states | in-review and needs-human are User Story states, not comments; the board mirrors the internal state machine `queued → in-progress → in-review → needs-human → done` 1:1; this is a Rung-2 milestone gated on the one-time PCA process-state setup this PRD does not own (Phasing) | `config.tracker.states` populated and mandat writing state instead of commenting (research doc, Principle 4) |
| Runtime runs inside project rights | 0 Project-Collection-Administrator permissions required at runtime; process and state setup is a one-time PCA act done before mandat operates | runtime board writes succeed under ordinary project rights (research doc, Verified constraint) |

## Scope and non-goals

In scope, the four capabilities the research doc designs:

- Work-station map: Epic, Feature, and User Story objects mapped 1:1 to the governance chain, with author RoleAgent and human gate per station (research doc, Principle 2).
- Field-ownership matrix plus AC provenance: the matrix above made operative. AC is repo-canonical; at slice-card breakdown the planner authors a read-only card digest that cites the doc AC-ids it derives from (write-time provenance). There is no automated card-vs-doc comparison; a card-provenance audit checks that each active-slice card names its source AC-ids (research doc, Principle 1; probe `docs/research/ac-ownership-probe.md`).
- Rich-card projection: description digest for a 10-second read, link to the governed doc plus parent ids, a structured per-gate verify-verdict comment, and a run-cost comment. Every enrichment is deterministic projection from an existing source (`run.TotalCostUSD`, the journal, the config); it adds no new judgment. Backlog #34 (bounded gate output on red) is the one missing input and now has a home (research doc, Principle 3).
- Inherited-process state model: User Story states `In Review` and `Needs Human` added to an inherited process on Agile, with config gaining `tracker.states.in_review` and `tracker.states.needs_human` (config-not-code) so mandat writes state, not only comments (research doc, Principle 4).

Out of scope, deferred not dropped:

- First-run init (US-0013, `mandat init`) and the provisioning ladder (US-0014, `mandat provision`) are separate chartered surfaces; this PRD neither duplicates nor blocks them.
- Building the custom inherited process itself is a human/PCA act. This PRD depends on it existing but does not automate it (see Verified constraint below); mandat operates inside the resulting process, it does not create it.
- Multi-tracker support (Jira) stays future, consistent with PRD-0001's single-adapter MVP scope.

## Verified constraint (design boundary)

Probed this session on the dogfood org, the REST path `PATCH /_apis/projects/{id}/properties {System.ProcessTemplateType}` returns 403 TF50309 ("needs Manage system project properties") for a project owner, a permission that sits at Project-Collection-Administrator level (research doc, Verified constraint). So setting up the process and its states is a one-time human/PCA act, like ratification itself, and mandat only operates inside the resulting process, creating work items, setting state, and linking PRs under ordinary project rights. This is the right boundary, not a limitation: the human defines the gate topology, the agent runs work through it, which is the same "human opens the gate, agent runs inside it" thesis the product enforces everywhere else (spec §4.4). The inherited process is kept version-controlled and reviewed like code, matching the config-not-code stance.

## Phasing

Two rungs (research doc, Principle 4, and Trade-offs). Rung 1, zero customization, is live now: Agile plus hierarchy-and-tag conventions, with in-review and needs-human staying comments as today; it unblocks board-driven work immediately after the Basic-to-Agile migration this session. Rung 2, the inherited process with the two custom states, is the milestone that makes the board the sole control surface. The inherited process is a maintained artifact whose cost is real but largely one-time. The load-bearing discipline is the planner authoring a faithful card digest from the doc at breakdown and the AC staying repo-canonical, so the doc remains the single authored spec; there is no runtime comparison to maintain (probe `docs/research/ac-ownership-probe.md`).

## Sources

- `docs/research/board-driven-sdlc-operating-model.md` (design synthesis and verified permission facts, 2026-07-16), primary source.
- `docs/research/ac-ownership-probe.md` (2026-07-16, verdict NOT VIABLE): killed the AC-containment mechanism an earlier draft proposed; establishes AC as repo-canonical with a read-only card digest that cites its source AC-ids at write time.
- PRD-0001 (the mandate pipeline this operating model runs), spec §4.4 (ratification is the only stage-gate advance), RFC-0001 (the pipeline the board projects).
- US-0013 (first-run init) and US-0014 (provisioning ladder), adjacent chartered surfaces this PRD excludes by reference.
- Prose follows ADR-0003 (spare senior prose, active voice, quantified targets).
