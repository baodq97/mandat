# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Mandat turns tracker work items (Azure DevOps first, Jira later) into reviewed pull requests through AI role agents. Every agent acts under an explicit, scoped, revocable mandate: a Microsoft Entra agent identity sponsored by a named human. The product ships as one Go static binary on one customer Linux VM with embedded SQLite.

**Current stage: harness only. Product logic does not exist yet** — the Go tree holds a harness-proving slice (`cmd/mandat`, `internal/buildinfo`) and every runtime capability is design-gated by the governed docs. The canonical design is `docs/superpowers/specs/2026-07-15-mandat-system-design.md` (decisions D1–D7, glossary, nine planes, MVP slice, spikes S1–S5). Read it before any architecture-affecting work and use its glossary terms (Mandate, RoleAgent, TaskContract, Remit, ResultContract, Ratification, Journal) in everything you write here. The design spec is the pre-governance draft that seeds the first governed PRD; harness and policy decisions live in ADR-0001..0003.

## Commands

Two gate families, both keyless. CI runs exactly these commands — one gate definition, two consumers (ADR-0001).

Code gate:

- `make check`: the aggregate — format diff (`golangci-lint fmt --diff`), lint, `go test -race -shuffle=on -count=1`, go.mod tidy check, `go tool govulncheck`, and a `CGO_ENABLED=0 go build`. The static-build step is a design gate: D3/D4 break the moment a dependency needs cgo.
- Individual targets: `make fmt` (writes formatting), `make lint`, `make test`, `make vuln`, `make build` (binary at `bin/mandat`), `make deps-check` (advisory: direct deps with newer versions).
- Run a single test: `go test -race -run 'TestRun/version' ./cmd/mandat`.
- golangci-lint is a pinned binary that make auto-installs into repo-local `bin/` (version-stamped path, so bumping `GOLANGCI_LINT_VERSION` forces a fresh install); govulncheck is pinned via the go.mod `tool` directive. Dependabot automates go.mod and GitHub Actions bumps only — the golangci-lint pin in the Makefile and the govkit pin in `ci.yml` are manual bumps.

Docs gate (govkit via npx, no install step):

- `npx govkit check`: the no-key CI gate. Runs `verify` then `eval`; exits non-zero if either fails.
- `npx govkit verify`: structural gate. Front-matter completeness, status enum, id↔filename convention, INDEX sync, globally unique ids, no unresolved placeholders.
- `npx govkit eval`: quality signal. A small required floor that blocks (minimum word count, no template filler) plus an advisory 0–100 score against the rubrics in `govkit.yml` (threshold 70; a lower score warns, never blocks).
- `d2 docs/diagrams/system-context.d2 docs/diagrams/system-context.svg`: re-render the context diagram after editing the `.d2` source. Diagrams are D2.

A `PreToolUse` hook in `.claude/settings.json` runs `npx govkit audit-write` on every Write/Edit. A rejected write means the governance audit blocked it on purpose: fix the doc, don't retry the write.

## Governed docs

`govkit.yml` is the single source of truth for doc types, dirs, required front-matter, lifecycles, and eval rubrics. Read it rather than trusting this summary:

| Type | Dir | ID prefix | Start status | Statuses |
|---|---|---|---|---|
| PRD | `docs/product/` | `PRD` | `draft` | draft, review, approved, rejected, superseded |
| RFC | `docs/rfc/` | `RFC` | `draft` | draft, proposed, accepted, rejected, superseded |
| ADR | `docs/adr/` | `ADR` | `proposed` | proposed, accepted, rejected, superseded |
| US | `docs/issues/` | `US` | `open` | open, in-progress, blocked, done, wontfix |

- Every governed doc requires front-matter `id, title, status, owner, date` (US adds `priority`); the filename must match the id. Every add or status change must update the matching `INDEX.md` row. The gate ignores `INDEX.md` and `_TEMPLATE.md`.
- New docs start at the type's start status. **Never self-flip a status, self-assign an owner, or approve your own doc.** AI proposes; a human ratifies in-session; the flip lands in a separate accept commit citing that authorization. The repo dogfoods the product's own thesis (spec §4.4: a proposal never self-ratifies).
- **Before proposing any status advance, red-team the doc**: an independent agent — never the doc's author, senior model tier — runs the spec-red-team pass (steelman → falsifiable "Fails if" findings → self-refutation → one kill criterion) and its brief goes to the owner together with the ratification request. Advisory by construction: keyed passes never enter CI, hooks, or exit codes, and the owner may advance with findings open.
- Structure new docs to the eval rubric sections in `govkit.yml` (PRD: problem, persona, success metrics, scope; RFC: summary, alternatives with trade-offs, open questions, impact, decision; ADR: context, decision, consequences; US: acceptance criteria).

## Code policy (ADR-0002..0004)

- **Dependency ladder** — take the first rung that fits: Go stdlib → official SDK for the service → popular maintained third-party library → hand-written code as the last resort, kept minimal. Every new direct dependency gets vetted (alive, adopted, no vulnerabilities, latest stable, `CGO_ENABLED=0`-compatible, license-safe) and the vetting recorded in the PR that introduces it. Dependabot owns version bumps.
- **Comments** — the code is the documentation. A comment must state what code cannot: an external contract, an invariant, a non-obvious why. Never narrate what a line does or restate a symbol name — agents read comments as ground truth, so noise and drift actively bias them. The linter deliberately does not require doc-comment boilerplate; do not add it out of habit.
- **Implementation stance** — the smallest change that delivers the stated value; rigor scales with reversibility (two-way doors iterate, one-way doors get design-first; unclear means one-way). Two exceptions: gates/tests/quality floors are exempt from minimization, and asymmetric downside cost beats minimal. Precondition: the value's measure (metric, acceptance criterion, visible behavior, gate flip) is stated before implementation — no measure, no code.

## Agent orchestration

The lead session model (Fable) guides, decides, and integrates — it never does detail work itself. Fan hands-on work out with an explicit `model` on every Agent/Workflow `agent()` call: `opus` for senior-level work (code review, adversarial verification, complex implementation), `sonnet` for mid-level work (research sweeps, scaffolding, mechanical edits). Never spawn a subagent on the lead model.

## Architecture (design-level)

One binary, planes as internal Go packages. The pipeline: a tracker adapter maps work items into `TaskContract`s; the orchestrator (a pure-function state machine, `queued → in-progress → in-review → needs-human → done | failed`) dispatches dependency waves with zero tokens; the runner supervisor spawns Claude Code CLI headless (`-p --output-format stream-json`) inside a per-task git worktree; the subprocess writes a schema-validated `ResultContract` file; the verification plane probes ground truth, re-runs gates, and scores the run; every decision lands in the append-only journal (SQLite).

Design invariants that constrain any code written here:

- **SQLite is the only database, permanently** (pure-Go driver, WAL, one file under `/var/lib/mandat/`). D4 rejects server databases outright; when one VM saturates, scale the runner pool, never the database.
- **The runner sits behind a file contract.** The supervisor validates the `ResultContract` against schema and never parses prose. The subprocess seam keeps the runner replaceable (D6).
- **Mechanical layers enforce remit, never prompts**: sparse checkout (the agent cannot see outside its paths), per-role OS user with file permissions, and a post-hoc diff-inside-remit check. Isolation failure fails the task; there is no shared-checkout fallback.
- **Writer ≠ scorer is an IAM property**: distinct Entra agent identities per role plus a creator-cannot-approve branch policy. Verification re-runs gates and never trusts agent summaries.
- **Ratification, a human status flip on the tracker, is the only act that advances a stage gate.** Autonomy ceilings live in config files the customer reviews like code; no runtime control can raise one.
- **No PAT anywhere.** Hourly Entra tokens are the git credential (the binary re-invokes itself as a git credential helper); production mode keeps zero secrets on disk (Arc + FIC).
- **New role = new YAML config entry, never new code.** A role carries mandate reference, playbook, remit defaults, autonomy ceiling, and model tier.
- **Every I/O seam gets a contract test**: recorded ADO fixtures, a local bare git origin, a fake `claude` binary emitting scripted stream-json. Testing only the pure cores is the explicit anti-goal (spec §9).
