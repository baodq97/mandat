# Board-driven SDLC operating model — design synthesis for PRD-0002

Design note, 2026-07-16. Captures the operating-model brainstorm (this session) plus the
process/permission facts verified against Azure DevOps this session, so PRD-0002 charters
from evidence rather than memory. Not a governed doc; PRD-0002 cites it as a Source.

## Motivation

The pilot ran the board as flat Basic Issues, one card per implementation slice, with
in-review/needs-human surfaced only as free-text comments. Two gaps the owner named:

1. **No work stations.** A reader cannot tell from the board which item is brainstorm, which
   is a PRD/tech doc, which is implementation. The stages of the governed chain
   (PRD → RFC → ADR → US → Code) have no visible home on the board.
2. **Cards are information-poor.** The board is the "surface" a human should understand and
   act on in ~10 seconds, but today it carries a title and a state and little else — the
   design detail lives only in the repo docs.

The design thesis mandat already holds (spec §4.4): a human status flip on the tracker is the
only act that advances a stage gate. The board is therefore both the operational projection
of work and the ratification surface. This note designs what that projection should carry.

## Principle 1 — one canonical home per field, never a mode-switch source of truth

The tempting design ("source of truth changes between board and repo per artifact") is the
most expensive to operate: every consumer must first ask "who is canonical now?", and that
question is the drift generator (LEARNING-LOOP entry 16 is exactly this class). Replace it
with **field-level ownership** — each field has one permanent home, so there is no conflict
to resolve:

| Field | Canonical home | Sync direction |
|---|---|---|
| State, assignment, priority, backlog rank | **Board** (the human act is the gate/grant) | board → repo (accept commit follows) |
| Acceptance criteria of the active slice | **Board** — already true: the adapter lifts AC from the card into the `TaskContract`, not from the doc | card AC edited → doc amendment commit, provenance = card revision |
| Doc body, design detail, research, decision rationale | **Repo** (where the agent works) | repo → board digest |
| Operational evidence (PR link, verify verdict, cost, run journal) | **Journal / mandat** | mandat → board comment |

mandat already operates this way implicitly: the card's AC field is the operative truth for
dispatch. Formalizing it as an ownership matrix plus a **drift detector** (the poll already
reads the AC each cycle — hash it against the doc's AC; on mismatch, surface a sync proposal
or hold) closes the drift class by construction.

## Principle 2 — work stations map 1:1 to the governance chain

| Board object | Governance meaning | Author (agent) | Human gate |
|---|---|---|---|
| Epic | Milestone / theme spanning several US | AI proposes, human seeds | pick which Epic proceeds |
| Feature `doc:PRD-xxxx` | one governed PRD | spec-drafter role + red-team | drag = approve PRD |
| Feature `doc:RFC/ADR-xxxx` | one governed RFC/ADR | sa-architect role + red-team | drag = accept |
| Feature = a US doc; child User Story = a single-concern slice | the US and its breakdown | ba-analyst / planner, created UNASSIGNED | assignment = mandate grant |
| User Story: Active → In Review → (Needs Human) → Closed | implementation | dev + reviewer RoleAgent | drag to Closed = ratify Done |

Assignment-as-grant is structural, not a new mechanism: Poll only picks up work items
assigned to an agent UPN, so the human assigning a card IS the act that grants the mandate
(spec §4.4). The `.claude/agents` roles (spec-drafter, sa-architect, ba-analyst, red-teamer)
are the dev-time mirror of the product RoleAgents these stations would run.

## Principle 3 — rich card projection (all deterministic, no new judgment)

The board becomes a dashboard + control surface. Every enrichment below has an existing data
source, so this is projection, not new analysis:

- **Description** = story statement + a one-paragraph problem digest written for a 10-second
  read, not a doc dump.
- **Link to the governed doc** + parent PRD/RFC ids — one click to full detail.
- **Structured verify-verdict comment**: per-gate green/red with the command, remit check,
  ancestry check, probe result. Today the journal keeps only exit codes — backlog #34
  (bounded gate output on red) is the missing input and now has a home.
- **Cost-per-run comment**: `run.TotalCostUSD` already persists per run; surfacing it on the
  card gives board-level cost visibility (cost-conscious by default).
- Priority / backlog rank / parent link: carried by the Agile hierarchy itself.

## Principle 4 — process and state model

Stock Agile (New / Active / Resolved / Closed) cannot express the stages the operating model
needs: "in red-team", "in-review", "needs-human". Two rungs:

1. **Now (zero customization): Agile + conventions.** Hierarchy + tags as above. Gates are
   poor — in-review / needs-human stay comments, as today. Runnable immediately (the
   Basic→Agile migration already landed this session).
2. **Target: an inherited process on Agile.** Add User Story states `In Review` and
   `Needs Human`. Config gains `tracker.states.in_review` / `needs_human` (config-not-code),
   and mandat writes state instead of only commenting. The board then mirrors mandat's
   internal state machine (`queued → in-progress → in-review → needs-human → done`) 1:1, and
   the board genuinely becomes the single control surface.

## Verified constraint — process/state customization is a PCA act, not automatable per-project

Probed this session on the dogfood org: the REST path
`PATCH /_apis/projects/{id}/properties {System.ProcessTemplateType}` returns **403 TF50309:
"needs Manage system project properties"** for a project owner. That permission is
Project-Collection-Administrator level. Consequence for the design: **setting up the process
and its states is a one-time human/PCA act — like ratification itself — and mandat only
OPERATES inside the resulting process** (create work items, set state, link PRs) with
ordinary project rights. This is the right boundary, not a limitation: the human defines the
gate topology; the agent runs work through it. The inherited process is maintained as code
(exported/version-controlled) to match the config-not-code stance.

## Trade-offs and phasing

- The inherited process is a maintained artifact; keep it version-controlled, treat changes
  like code review. Cost is real but one-time-ish.
- Rung 1 (conventions) is live now and unblocks board-driven work today; rung 2 (custom
  states) is the milestone that makes the board the sole surface.
- Risk to weigh in the PRD: the field-ownership matrix depends on the drift detector actually
  running each poll; without it, board/repo AC divergence is silent — the same failure mode
  the matrix is meant to kill. The detector is a required AC, not a nice-to-have.

## Sources (verified this session)

- `docs/research/cli-first-run-survey.md` (config-as-reviewable-artifact, config/secret split
  patterns carry over to board/repo ownership).
- `docs/research/entra-agent-id-provisioning-surface.md` (assignment-as-grant, identity
  registry the planner station consumes).
- Azure DevOps process migration + permission model, MS Learn (verified 2026-07-16):
  change-process-basic-to-agile; the PCA `Manage system project properties` gate on
  `System.ProcessTemplateType`.
- mandat design spec §4.4 (ratification = the only stage-gate advance); RFC-0001 (the
  pipeline the board projects).
