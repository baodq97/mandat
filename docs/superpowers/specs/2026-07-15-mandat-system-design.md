# Mandat — System Design

**Status:** draft, awaiting owner review
**Date:** 2026-07-15
**Owner:** Bao Do

Mandat turns tracker work items into reviewed pull requests through AI role agents, where
every agent acts under an explicit, scoped, revocable mandate: a Microsoft Entra agent
identity sponsored by a named human. The product ships as one Go binary that runs on one
customer Linux VM.

The name is the thesis. A mandat (French and Vietnamese legal usage; "mandate" in English)
is a written grant of authority to act on someone's behalf. Entra agent identity is the
instrument, the human sponsor signs it, status ratification renews it, revoking the
identity withdraws it, and the journal is the enforcement record.

## 1. Decisions

| # | Decision | Rationale | Rejected alternative |
|---|---|---|---|
| D1 | Product name **Mandat** | Meaning = product mechanism; clean in software space (8-candidate collision census, 2026-07-15); bilingual FR/VI resonance; 6-letter CLI | Crew/Roster/Cabinet/Cadre/Mandatum/Legate/Prokura/Reeve (all collide with existing agent products) |
| D2 | Own control plane driving Claude Code | Best available coding agent; tracker-independent; identity moat stays ours | Microsoft-native runtime (Foundry/Copilot Studio): weak code execution, deep lock-in |
| D3 | **Go, single static binary, VM-first** | Fastest path holding the whole product stack at lowest cost for highest value; 10-minute pilot on any Linux VM; Gitea/Caddy distribution pattern; enterprise ops teams trust single-bin + systemd | Cloud-first microservices + managed services: higher cost, slower iteration, and the reference system (AI-Autopilot) proved its cloud packaging was aspirational while the real deployment was one box |
| D4 | SQLite embedded (pure-Go driver, WAL), **the only database, permanently** | Zero-dependency install; runtime product legitimately owns durable state; one file to back up; if a deployment ever outgrows one VM, scale the runner pool, never the database | Postgres or any server database (rejected outright: heavy operational dependency that breaks the single-binary promise) |
| D5 | Identity backend = Azure Arc managed identity, client-credential fallback | Arc Connected Machine agent exposes IMDS at localhost:40342 (himds group) on any Linux VM; blueprint FIC chains from it; no secret on disk | Azure-hosted managed identity (forces cloud deployment); PAT (the anti-pattern this product exists to kill) |
| D6 | Runner = Claude Code CLI subprocess, headless stream-json | No Go agent SDK exists; the file-based result contract is the proven integration seam; stream-json feeds the activity view | Embedding an agent loop in-process; driving a TS/Python sidecar (second runtime on the VM) |
| D7 | This repo is govkit-governed from day one | Mandat is govkit consumer n+1: its PRD/RFC/ADR/US lifecycle runs through `govkit check`, dogfooding both products forward together; this design doc is the pre-governance draft that seeds the first governed PRD | Ungoverned docs folder (repeats the reference system's harness-by-willpower failure) |

## 2. Glossary

| Term | Meaning |
|---|---|
| **Mandate** | The grant of authority for one RoleAgent: Entra agent identity + sponsor + scoped tracker/repo permissions + autonomy ceiling. Revocable at will |
| **RoleAgent** | A role (PO, SA, Dev, QA, Reviewer) as config: mandate + playbook + allowed paths + tools. New role = new config entry, never new code |
| **TaskContract** | Canonical work-item model every tracker adapter maps to: id, type, acceptance criteria, refs, state, remit |
| **Remit** | The scope a task grants a RoleAgent: repos + path patterns. Enforced mechanically (sparse checkout, OS user, server policy), never by prompt |
| **ResultContract** | Schema-validated JSON a runner must write: status, artifacts[{repo, branch, pr_url}], needs_human, reason |
| **Ratification** | A human flipping a status on the tracker. The only act that advances a stage gate |
| **Journal** | Append-only record of every dispatch decision, gate outcome, score, and config version, keyed by acting identity |

## 3. System context

Rendered from `docs/diagrams/system-context.d2` (D2, `d2 docs/diagrams/system-context.d2 docs/diagrams/system-context.svg`).

External systems: the tracker (Azure DevOps first, Jira later through the same adapter
seam), Microsoft Entra ID (agent identity blueprint + per-role agent identities), the
Anthropic API (consumed by Claude Code subprocesses), MS Teams (notification only). Humans
appear in two places: the tracker (consent, comments, ratification) and the dashboard
(Entra SSO, read + break-glass controls).

Everything else lives on one customer Linux VM: the `mandat` binary (all planes as
internal Go packages), the Azure Arc agent, a git mirror cache, per-task worktrees, and
Claude Code CLI subprocesses.

## 4. Planes

### 4.1 Identity plane
- One Entra **agent identity blueprint** per installation. Blueprint credential: federated
  identity credential rooted in the Arc managed identity (production) or client
  secret/certificate (pilot fallback). Per MS Learn, the blueprint holds credentials and
  acquires tokens on behalf of agent identities; agent identities hold none.
- One **agent identity** per RoleAgent, sponsor = the customer owner. Roles that trackers
  must treat as people (assignee pickers) get the paired **agent user**.
- Authorization by groups: Entra security group per role maps to tracker permission
  groups. PO writes work items and reads code. Dev pushes branches and cannot merge. QA
  and Reviewer read and comment. ADO branch policy "creator cannot approve own PR" plus
  distinct identities makes writer ≠ scorer an IAM property.
- Tokens expire hourly; the token itself is the git credential (Bearer over HTTPS), so no
  secret ever reaches disk. `mandat git-credential` (the binary re-invoked as a git
  credential helper) serves tokens to git at fetch/push time.

### 4.2 Tracker plane
- `Adapter` interface: `Poll(query) []TaskContract`, `Webhook(payload) TaskContract`,
  `Apply(outcome)`, `Comment(id, body)`, mapped per tracker. ADO adapter first (WIQL poll
  every 30s + Service Hook webhook). Jira later without touching core.
- Consent stays in the tracker: a human-applied trigger tag or assignment to an agent
  user. Webhooks feed the dispatcher, never bypass it.
- Every write uses the acting RoleAgent's token, so tracker audit history reads like a
  team's history, not a bot account's.

### 4.3 Orchestrator plane
- Pure-function state machine: `queued → in-progress → in-review → needs-human → done |
  failed`. `needs-human` is first class: the item holds until a human clears it.
- Zero-token dispatcher: dependency links block hard, related links form conflict groups
  that never run concurrently, waves respect `max_concurrent`. LLM conflict verdicts (see
  4.6) persist as scored edges the dispatcher reads back.
- One outcome-policy table maps run outcomes to tracker mutations. Dry-run suppresses all.
- Budget circuit breaker: token/cost ceilings per day and per task; crossing one halts
  dispatch and escalates. Retry state persists in SQLite and survives restart.

### 4.4 Role plane
- Roles are YAML config: mandate reference, playbook (skill set the runner loads), remit
  defaults, autonomy ceiling (`report`, `draft-pr`, `unattended`), model tier.
- Shipped roles: PO (idea → PRD proposal via comment interview), SA (ratified PRD →
  architecture + user stories), Dev (story → branch → draft PR), QA (acceptance criteria →
  test plan → verdict), Reviewer (read-only, verification plane only).
- A proposal never self-ratifies. PO and SA output lands as `proposed`; a human flips the
  status; only then does the next role's trigger match.

### 4.5 Workspace plane
- Mirror cache: `git clone --mirror` per repo under `/var/lib/mandat/mirrors`, refreshed
  behind a per-repo lock, `--filter=blob:none` for large repos.
- Per-task checkout: `git worktree add --reference` into `/var/lib/mandat/tasks/<id>`,
  base ref resolved per repo (configured, then `origin/HEAD`, then main/master).
- **Remit enforcement in three layers**: sparse checkout materializes only allowed paths
  (the agent cannot see the rest), per-role OS user + file permissions bound the process,
  and the verification plane diffs the branch against the remit after the fact.
- Isolation failure fails the task. There is no fallback to a shared checkout.
- Harness bundle (playbooks, skills, MCP config) is a versioned artifact pulled at
  provision time; its version lands in the journal with every run.

### 4.6 Execution plane
- Runner supervisor spawns `claude` headless (`-p`, `--output-format stream-json`) inside
  the worktree as the role's OS user (`systemd-run --uid=mandat-dev` or setpriv), wires
  the agent-identity token through the credential helper, streams activity to the
  dashboard, and enforces wall-clock plus token limits.
- The subprocess must write the ResultContract file; the supervisor validates it against
  the schema and never parses prose.
- Bounded LLM judges (conflict analysis, self-review) run with plan-mode permissions and
  single-turn caps; their verdicts persist as data for deterministic consumers.

### 4.7 Verification plane
- Ground truth probes: PR existence via the Reviewer identity's API call, files changed
  from the real diff, diff-inside-remit check.
- Gate re-run: build, tests, linters, and configured spec gates execute in the verifier's
  context; agent summaries are never trusted.
- Run scoring: wired signals only, unknown scores neutral-low, thresholds route to
  auto-proceed / human review / escalate. A labeled corpus of good and weak runs ships
  with the product, and CI proves the escalate band actually fires (a gate that cannot
  fire is a badge, not a gate).

### 4.8 Human plane
- The tracker is the primary surface: mentions, comments, ratification.
- Dashboard: Go html/template + htmx, behind Entra SSO. Read-mostly; every mutating action
  requires an authenticated human and lands in the journal. No runtime control can raise
  an autonomy ceiling; ceilings live in config, and config changes are file edits the
  customer reviews like code.
- Teams adaptive cards notify; approval always happens on the tracker where it is durable.

### 4.9 Journal plane
- Append-only SQLite table (plus JSONL export): every dispatch, gate outcome, score,
  ResultContract, config/harness version, acting identity, and human ratification.
- Entra sign-in logs give the identity-level audit trail for free; the journal links to it
  by identity + timestamp.

## 4.10 Install and first-run setup

Install time is one download: a static binary (linux/amd64, linux/arm64) dropped into
`/usr/local/bin/mandat`. Setup time is one command:

- `mandat init` runs an idempotent first-run wizard that collects: tracker kind + org +
  project, auth mode (Arc managed identity or client certificate), the identity mode
  (`service-principal`, `agent-user-pair`, or `agent-identity` per ADR-0005), Entra tenant
  + blueprint + per-role agent identity ids (plus the paired agent user ids in
  `agent-user-pair`), the repo registry (url, base branch, remit defaults per repo),
  enabled roles + autonomy ceilings, and notification targets. It
  writes `/etc/mandat/config.yaml` (root-owned, reviewed like code), bootstraps the
  SQLite file at `/var/lib/mandat/mandat.db`, creates the per-role OS users, installs the
  hardened systemd unit, and registers the git credential helper.
- `mandat doctor` verifies the installation end to end before first dispatch: tracker
  reachability per identity, token acquisition through the configured auth mode and
  identity mode (per-role service-principal client-credential, or the three-leg agent-user
  delegated chain per ADR-0005), git fetch against every registered repo, `claude` binary
  presence, and disk headroom. Every check maps to one spike (S1-S4), so a green doctor run
  is the deployed proof of the spike results.
- Re-running `mandat init` updates config in place and never destroys state. Upgrade =
  replace the binary and restart the unit; additive SQLite migrations run at boot.

## 5. End-to-end flow

1. Owner drops an idea as a work item and tags it for the PO role.
2. PO-agent interviews the owner in the comment thread, then attaches a PRD proposal
   (`proposed`).
3. Owner ratifies (status flip). SA-agent decomposes into stories, each carrying a remit.
   Owner ratifies the plan.
4. Dispatcher schedules stories by dependency waves. For each: worktree + sparse checkout,
   Dev-agent runs headless, pushes a branch with its own identity, opens a draft PR.
5. Verification plane probes ground truth, re-runs gates, scores the run. Weak runs hold
   for a human; clean runs request review.
6. Humans review the draft PR; comments trigger bounded revision loops; a human merges.
7. State sync moves the item on merge, rolls parents up, notifies, journals everything.

## 6. Data model (SQLite, WAL)

`tasks` (contract + pipeline state), `runs` (execution records, token cost, score),
`results` (ResultContracts raw), `conflicts` (scored edges), `merged_prs` (state-sync
dedup), `journal` (append-only), `retry_state`, `planned_runs`. Additive migrations only;
one file under `/var/lib/mandat/mandat.db`; nightly copy is the backup story.

## 7. Security model

- No PAT anywhere. All tracker/git auth is hourly Entra tokens per agent identity.
- Secrets on disk: none in production mode (Arc + FIC); pilot fallback stores one
  blueprint client certificate under `/etc/mandat/` root-only.
- Dashboard and webhook endpoints authenticate (SSO; webhook by shared-secret header at
  minimum). Nothing listens unauthenticated.
- Prompt-injection blast radius is bounded by the remit layers: a hijacked agent can at
  worst edit inside its sparse checkout and open a PR it cannot merge under an identity
  that cannot approve.
- systemd hardening on the unit (ProtectSystem, PrivateTmp, per-role users).

## 8. Failure handling

Restart-safe by construction: pipeline state, retry backoff, revision budgets, and
seen-comment cursors all persist; startup requeues in-progress work, fails orphan runs,
prunes orphan worktrees past a TTL while keeping live interactive sessions addressable by
deterministic names. Every external write is best-effort with logged degradation, except
gates, which block by design.

## 9. Testing and calibration

- Pure cores (state machine, dispatcher, scorer, outcome policy) get exhaustive unit
  tests.
- Every I/O seam gets a contract test: recorded ADO fixtures for the adapter, a local bare
  git origin for the workspace plane, a fake `claude` binary emitting scripted stream-json
  and ResultContracts for the runner. The reference system's mistake (pure cores tested,
  every seam bare) is the explicit anti-goal.
- Scoring ships with a labeled run corpus; `mandat calibrate` proves zero false-escalates
  and a non-empty escalate band before any threshold change lands.

## 10. MVP slice and roadmap

**MVP (thin, runnable):** ADO adapter, PO + Dev roles, client-credential identity mode,
draft-PR autonomy only, verification probes + gate re-run, journal, status page. One
customer VM, one team project.

Then, in order: Arc identity mode, QA + Reviewer roles + scoring calibrate, planning view
+ scheduled waves, Teams notify, Jira adapter, agent-user assignment UX, multi-VM runner
pool only when a real customer saturates one VM.

## 11. Spikes (before MVP code)

| # | Question | Kill risk |
|---|---|---|
| S1 | Does ADO Users hub accept agent-identity service principals (new subtype), and can an agent user be assigned work items? | Falls back to standard SP per role: keeps the architecture, loses sponsor/agent-native audit |
| S2 | Arc IMDS (himds) → blueprint FIC → agent-identity token chain end to end on a plain Linux VM | Falls back to client certificate on the blueprint |
| S3 | Entra access token as git HTTPS credential against ADO repos from a headless CLI | Falls back to short-lived ADO-scoped tokens fetched per push |
| S4 | Claude Code headless under a separate OS user in a sparse worktree, ResultContract + stream-json capture | Falls back to same-user execution with remit enforced by verify-only (weaker) |
| S5 | Run-scoring corpus format + calibrate harness | Scoring ships advisory-only until the corpus exists |

## 12. Accepted trade-offs

- One VM is a single point of failure. Accepted: systemd restart + WAL SQLite + restart-
  safe state machine cover team-scale; HA is a paid-tier problem for later.
- Vertical concurrency ceiling (VM cores). Accepted: dependency waves already serialize;
  a runner-pool mode is roadmap, not MVP.
- Go has no Anthropic agent SDK. Accepted: the CLI subprocess + file contract is the
  proven seam, and it keeps the runner replaceable (Gemini CLI, Codex) behind one
  interface.
- Per-identity tracker licenses cost real money per role. Accepted and surfaced in
  pricing; also caps role sprawl, which the design wants anyway.

## 13. Reference lessons (AI-Autopilot audit, 2026-07-15)

Borrowed with evidence: tag-as-consent, result contract, unknown-scores-neutral-low,
outcome-policy table, restart-safety layering, worktree-per-task. Corrected with
evidence: score gate that cannot fire (F1), agent self-report as ground truth (F2),
unauthenticated control surface (F3), vestigial RBAC (F4), prose rails (F5), harness
outside version control, coverage inverse to risk. Sources: MS Learn on Entra Agent ID
(blueprints, agent identities, agent users), ADO service principals, Arc managed
identity IMDS.
