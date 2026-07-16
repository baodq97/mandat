---
id: RFC-0001
title: MVP pipeline — one ADO work item to a draft PR under a Dev RoleAgent mandate
status: accepted
owner: baodq97
date: 2026-07-16
---

# RFC-0001: MVP pipeline — one ADO work item to a draft PR under a Dev RoleAgent mandate

## Summary

This RFC pins the design for the MVP walking skeleton the PRD scopes (PRD-0001 §Scope,
spec §10): one Azure DevOps work item drives one Developer RoleAgent through the full
pipeline and stops at a draft PR opened under the Dev agent user. The path is ingest a
work item assigned to the Dev agent user, map it to a `TaskContract`, provision an
isolated worktree, spawn Claude Code headless (ADR-0006), let the agent edit inside its
remit and write a schema-valid `ResultContract`, re-run the repo's gates, confirm the
diff stays inside the remit, open a draft PR under the Dev agent user, and journal every
transition. Happy path, one role, no dependency waves.

The load-bearing output of this RFC is a set of interfaces the rest of the product keys
off: the `TaskContract` and `ResultContract` schemas, the orchestrator state machine as
a pure function, the append-only journal schema, the runner isolation seam, and the
identity-injection invariant (the delegated token never reaches the child). Those are the
one-way doors. The plane package boundaries, the WIQL query, the poll interval, and the
gate command list are two-way doors that US stories iterate; the concrete git
credential-delivery mechanism is neither, it is an open question gated by spike
S-credential-delivery (§Identity injection).

This RFC carries a correction to a stale PRD phrase. PRD-0001 §Scope and spec §10 both
name `client-credential` identity mode for the MVP. ADR-0005 (accepted) supersedes that:
the Dev role runs `identity_mode: agent-user-pair` on Azure DevOps, because the S1
round-3 and S3 spikes proved the paired agent user carries read, comment, clone, push,
and PR creation with a sponsor-linked, revocable token. `service-principal` remains the
portable fallback for surfaces where the chain is unproven, not the ADO write path. This
RFC designs to `agent-user-pair` and treats the older phrase as superseded.

## Scope

### In scope — the walking skeleton

One ADO work item, one Dev RoleAgent, no dependency waves. The slice exercises every
plane once on the happy path plus the deterministic `needs-human` edges. Ingestion is a
30s WIQL poll (webhook deferred). Consent is the work item being assigned to the Dev
agent user (no custom ADO field). Remit comes from the repo registry's remit defaults
for the named repo (config, spec §4.10). Gate re-run is a per-repo configured command
list; the dogfood proof targets a repo whose list is `make check` then `npx govkit
check`. The Dev role's model tier defaults to `sonnet` with a per-role override to
`opus`; the per-run budget ceiling is config via `--max-budget-usd`.

### Out of scope

Merge, state sync, and the `done` state (merge is the human ratification act, decision
2). Auto-retry and repair loops (decision 3, deferred). The PO/SA/QA/Reviewer role
playbooks (the Reviewer identity is provisioned for the ground-truth probe, but its LLM
playbook is deferred, per PRD §Scope). Dependency waves, conflict groups, and the
scheduler beyond a single in-flight task. Webhook ingestion, Teams notification, the
Jira adapter, Arc identity mode, and the multi-VM runner pool. The dashboard beyond a
read-only status view.

### Post-acceptance amendment (2026-07-16): single-VM concurrent dispatch

With the walking skeleton delivered and run live end to end (green `in-review` under the
Reviewer identity, 2026-07-16), the single-in-flight scope line above is relaxed for one
slice: concurrent dispatch of independent queued tasks on a single VM, bounded by a
configured runner pool size that defaults to 1 and is bit-compatible with the original
single-in-flight scope when unset. US-0012 scopes the slice; its acceptance criteria carry
the red-team-hardened constraints: dispatch integrity (no double-dispatch across poll
cycles, AC-12.6), a whole-mirror cross-task lock (AC-12.2), per-task runner config isolation
(`CLAUDE_CONFIG_DIR`/`HOME`, AC-12.7), an aggregate in-flight budget ceiling (AC-12.8), and a
pool-vs-sequential wall-clock benchmark as the kill criterion (no measured speedup yields to
multi-VM scale-out, not more single-VM locks). Still out of scope, unchanged: dependency
waves and conflict groups (spec §4.3's full `max_concurrent` scheduler) and the multi-VM
runner pool (spec §10, D4's scale-out answer). Provenance: the US-0012 red-team brief finding
F4 raised whether this relaxation needs an RFC amendment; the owner ruled amend, authorized
in-session 2026-07-16. Status, owner, and the accepted decisions are unchanged.

## Definition of done

The skeleton is done when one dispatched ADO work item reaches the `in-review` state
with a draft PR opened under the Dev agent user, the per-repo gate re-run is green, the
post-hoc diff stays inside the remit, and every transition is recorded in the append-only
journal (decision 2). `done`, merge, and state sync are explicitly outside this bar.

## Load-bearing contracts (one-way doors)

These four seams are persisted, cross-plane, and expensive to change after data exists.
This RFC fixes their shape now; §Impact records the migration stance.

### TaskContract

The canonical work-item model every tracker adapter maps to (spec §2). Persisted as JSON
in `tasks.contract`; the adapter is the only producer, every downstream plane a consumer.
Minimal per the glossary (id, type, acceptance, refs, state, remit) plus the fields the
skeleton dispatch needs.

| Field | Type | Notes |
|---|---|---|
| `id` | string | Canonical mandat task id, derived from the tracker reference, stable across polls |
| `tracker_ref` | object | `{ system: "azure-devops", org, project, work_item_id, url }`, sanitized placeholders in tests |
| `type` | enum | `dev-task` in the skeleton; `story`, `prd` reserved for later roles |
| `title` | string | Work item title |
| `acceptance` | string | Acceptance-criteria text lifted from the work item, unparsed |
| `refs` | []string | Related/dependency links; always empty in the skeleton (no waves) |
| `state` | enum | The orchestrator pipeline state (see the state machine); `queued` at creation |
| `role` | string | The RoleAgent key to dispatch; `dev` in the skeleton |
| `remit` | object | `{ repo, base_branch, paths: []string }` from the repo registry defaults (decision 4) |
| `assigned_to` | string | The agent-user principal the item is assigned to; consent is `assigned_to == <dev-agent-user>` (decision 4) |
| `schema_version` | int | Pinned at `1`; additive evolution only |

The adapter fails the mapping (no TaskContract, journaled skip) when the item's repo is
not in the registry or a required field is absent. Validation runs before dispatch.

### ResultContract

The schema-validated JSON the runner subprocess must write (spec §2, §4.6, ADR-0006).
The supervisor reads the file, validates it against this schema, and never parses
stream-json prose as the outcome. The subprocess writes to a fixed worktree path,
`.mandat/result.json`, passed to the child as `MANDAT_RESULT_PATH`; the `.mandat/`
control directory is excluded from the diff-inside-remit check.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "ResultContract",
  "type": "object",
  "additionalProperties": false,
  "required": ["schema_version", "task_id", "status"],
  "properties": {
    "schema_version": { "type": "integer", "const": 1 },
    "task_id":        { "type": "string", "minLength": 1 },
    "status":         { "enum": ["completed", "needs_human", "failed"] },
    "reason":         { "type": "string" },
    "artifacts": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["repo", "branch"],
        "properties": {
          "repo":   { "type": "string", "minLength": 1 },
          "branch": { "type": "string", "minLength": 1 },
          "pr_url": { "type": "string" }
        }
      }
    }
  },
  "allOf": [
    { "if":   { "properties": { "status": { "const": "completed" } } },
      "then": { "required": ["artifacts"],
                "properties": { "artifacts": { "minItems": 1 } } } },
    { "if":   { "properties": { "status": { "enum": ["needs_human", "failed"] } } },
      "then": { "required": ["reason"] } }
  ]
}
```

A file that is missing or fails this schema routes the task to `needs-human` (decision 3),
never to a retry. `status: completed` requires at least one artifact carrying `repo` and
`branch`; `pr_url` is present once the PR is opened. `status: needs_human` and
`status: failed` require a `reason`. The state machine keys solely on the `status` enum:
the `needs_human` state is derived from `status == "needs_human"`, so no separate boolean
field exists to disagree with the enum.

### Orchestrator state machine

A pure function `Next(state, event) -> (state, error)`, total over the enumerated inputs,
zero tokens, zero I/O (spec §4.3). Every plane keys off these states and the journal
records every transition. Six states: `queued`, `in-progress`, `in-review`, `needs-human`,
`done`, `failed`, joined by twelve transitions. The skeleton drives the subset reaching
`in-review` and `needs-human`; `done` (decision 2) and `failed` (reachable only by the
deferred `human_abandon`) are enumerated but out of slice.

Transition table (an unlisted `(state, event)` pair returns an error and is a no-op, never
a silent transition):

| Event | From | To | In skeleton |
|---|---|---|---|
| `dispatch` (consent: item assigned to `<dev-agent-user>`) | (start) | `queued` | yes |
| `claim_ok` (worktree + sparse checkout + OS user ready) | `queued` | `in-progress` | yes |
| `setup_failed` (isolation cannot be established) | `queued` | `needs-human` | yes (happy path skips) |
| `result_ok` (valid ResultContract `status == "completed"` + gates green + diff-in-remit + PR probe confirms) | `in-progress` | `in-review` | yes |
| `result_needs_human` (ResultContract `status == "needs_human"`) | `in-progress` | `needs-human` | yes |
| `result_invalid` (missing or schema-invalid ResultContract, or runner crash) | `in-progress` | `needs-human` | yes |
| `gate_red` (verification gate re-run fails) | `in-progress` | `needs-human` | yes |
| `remit_violation` (post-hoc diff touches a path outside the remit) | `in-progress` | `needs-human` | yes |
| `probe_failed` (Reviewer PR probe finds no PR, or PR `createdBy` ≠ the Dev agent user) | `in-progress` | `needs-human` | yes |
| `human_requeue` | `needs-human` | `queued` | no (retry deferred, decision 3) |
| `human_abandon` | `needs-human` | `failed` | no (deferred) |
| `human_ratify` (merge / status flip) | `in-review` | `done` | no (out of slice, decision 2) |

`needs-human` and `failed` separate a recoverable hold from a terminal human decision.
`setup_failed` is a system fault before the agent runs (worktree, sparse checkout, or
OS-user provisioning failed); it routes `queued -> needs-human` for a human to adjudicate,
with no shared-checkout fallback (spec §4.5). `failed` is reserved for explicit human
abandonment (`human_abandon`, deferred); no automatic edge reaches it. PRD-0001 §Success
metrics enumerates three deterministic failures that fire the `needs-human` edge and so
count against the clean-run proxy — a red gate re-run (`gate_red`), an isolation failure
(`setup_failed`), and an invalid or missing ResultContract (`result_invalid`); all three
route to `needs-human` here, which is the reconciliation F5 requires. Two further
deterministic ground-truth failures join them, `remit_violation` (post-hoc diff outside
the allowed paths) and `probe_failed`; the sixth `needs-human` event, `result_needs_human`,
is the agent's own escalation request, not a deterministic failure. `probe_failed` closes
the don't-trust-the-agent's-self-reported-PR seam: a schema-valid `completed` ResultContract
with green gates and an in-remit diff still holds for a human when the Reviewer-identity
ground-truth probe finds no PR, or a PR whose `createdBy` is not the Dev agent user.

```d2
direction: right

classes: {
  st:   { style: { stroke: "#2A2A33"; fill: "#9FA7D0" } }
  ok:   { style: { stroke: "#2A2A33"; fill: "#66CAD8" } }
  hold: { style: { stroke: "#2A2A33"; fill: "#FAA61A" } }
  bad:  { style: { stroke: "#2A2A33"; fill: "#F22000" } }
  done: { style: { stroke: "#2A2A33"; fill: "#00CC40" } }
}

queued:       { class: st }
in_progress:  { class: st; label: "in-progress" }
in_review:    { class: ok; label: "in-review" }
needs_human:  { class: hold; label: "needs-human" }
failed:       { class: bad; label: "failed (out of slice)" }
done:         { class: done; label: "done (out of slice)" }

start: { shape: circle; label: "" }
start -> queued: dispatch

queued -> in_progress: claim_ok
queued -> needs_human: setup_failed

in_progress -> in_review: "result_ok (gates green, diff in remit, PR probe confirms)"
in_progress -> needs_human: "result_needs_human"
in_progress -> needs_human: "result_invalid"
in_progress -> needs_human: "gate_red"
in_progress -> needs_human: "remit_violation"
in_progress -> needs_human: "probe_failed (no PR / createdBy ≠ dev agent)"

needs_human -> queued: "human_requeue (deferred)"
needs_human -> failed: "human_abandon (deferred)"
in_review -> done: "human_ratify (out of slice)"
```

### Journal and results schema

Four SQLite tables (WAL, one file under `/var/lib/mandat/`, pure-Go driver, additive
migrations only, spec §6, D4). The journal is append-only: the code exposes no update or
delete path and a trigger rejects both.

`tasks` (contract plus current pipeline state):

| Column | Type | Notes |
|---|---|---|
| `task_id` | TEXT PK | Canonical id |
| `tracker_ref` | TEXT | JSON |
| `role` | TEXT | RoleAgent key |
| `remit` | TEXT | JSON `{repo, base_branch, paths}` |
| `state` | TEXT | Current orchestrator state |
| `contract` | TEXT | The full TaskContract JSON |
| `created_at`, `updated_at` | TEXT | RFC3339 UTC |

`runs` (one execution record per spawn):

| Column | Type | Notes |
|---|---|---|
| `run_id` | TEXT PK | UUID |
| `task_id` | TEXT FK | |
| `session_id` | TEXT | The deterministic Claude session id (ADR-0006) |
| `acting_identity` | TEXT | The Dev agent-user principal the run acts as |
| `model` | TEXT | Resolved tier (`sonnet`/`opus`) |
| `started_at`, `ended_at` | TEXT | RFC3339 UTC |
| `total_cost_usd` | REAL | From the terminal `result` event |
| `usage` | TEXT | JSON `usage` from the `result` event |
| `num_turns` | INTEGER | From the `result` event |
| `is_error` | INTEGER | From the `result` event |
| `exit_code` | INTEGER | Subprocess exit status |
| `gate_result` | TEXT | JSON: command list, per-command exit codes |
| `harness_version`, `config_version` | TEXT | Bundle + config versions (spec §4.5, §4.9) |

`results` (raw ResultContract per run, never parsed as prose):

| Column | Type | Notes |
|---|---|---|
| `run_id` | TEXT FK | |
| `task_id` | TEXT | |
| `raw` | TEXT | The exact bytes the subprocess wrote |
| `valid` | INTEGER | Schema-validation outcome |
| `recorded_at` | TEXT | RFC3339 UTC |

`journal` (append-only, one row per event, spec §4.9):

| Column | Type | Notes |
|---|---|---|
| `seq` | INTEGER PK AUTOINCREMENT | Monotonic order |
| `ts` | TEXT | RFC3339 UTC |
| `task_id` | TEXT | |
| `run_id` | TEXT | Nullable |
| `acting_identity` | TEXT | Who acted: `system:orchestrator`, the Dev agent user, the Reviewer identity, or a human |
| `act` | TEXT | The event or probe name (`dispatch`, `claim_ok`, `gate_rerun`, `pr_opened`, `probe_pr_exists`) |
| `from_state`, `to_state` | TEXT | Nullable, set on transition events |
| `detail` | TEXT | JSON payload (pr_url, gate exit codes, diff-in-remit result, session_id, cost) |
| `config_version`, `harness_version` | TEXT | |

Every transition and every ground-truth probe writes one journal row naming the acting
identity. The orchestrator's own transitions act as `system:orchestrator`; the runner and
PR-open act as the Dev agent user; the PR-existence probe acts as the Reviewer identity,
which is how writer ≠ scorer shows up in the audit trail (spec §4.1, §4.7).

## Runner harness

### Invocation

The supervisor spawns Claude Code headless per task with the ADR-0006 flag set, pinned
there as an external contract and referenced here, not restated:
`claude -p --output-format stream-json --verbose --model <tier> --permission-mode dontAsk
--add-dir <worktree> --bare --session-id <uuid> --max-budget-usd <ceiling>`, plus
`--append-system-prompt-file <playbook+remit>`, `--settings <inline PreToolUse deny hook>`,
and `--mcp-config <tracker MCP> --strict-mcp-config`. `stream-json` is telemetry (the
`system/init` event carries `session_id`; the terminal `result` event carries
`total_cost_usd`, `usage`, `num_turns`, `is_error`); the ResultContract file is the
outcome. Require CLI ≥ 2.1.208 (ADR-0006 version floor); `mandat doctor` asserts it before
first dispatch. Default `--max-budget-usd` is `5.00` per run (config, aligned to the
PRD's ≤ $5 cost-per-size-S-PR target); the productionized per-role value is an open
question.

### Session and resume

Generate a deterministic `--session-id` (UUID) at spawn and write it to the journal before
the subprocess starts, so a crashed run is still addressable. Continuation uses
`claude -p --resume <uuid>` from the same worktree (ADR-0006), the seam a future
`needs-human` clear would drive. Resume is out of the skeleton (decision 3) but the id and
worktree binding are pinned now because they are recorded in `runs.session_id`.

### Isolation

Three mechanical layers, never prompts (spec §4.5–§4.6, ADR-0006). A per-task worktree via
`git worktree add --reference` against the mirror cache, with sparse checkout materializing
only the remit `paths` so the agent cannot see the rest of the repo. The child runs as the
per-role OS user (`systemd-run --uid=<role-user>` or `setpriv`), bounded by file
permissions. Each child gets its own `HOME` and `CLAUDE_CONFIG_DIR` so per-role config and
session stores do not collide. The `--bare` flag disables ambient discovery of hooks,
skills, MCP, and CLAUDE.md, so nothing loads except what this invocation names. Failure to
establish any layer fires `setup_failed`, which routes the task `queued -> needs-human` for
a human to adjudicate (spec §4.5); there is no shared-checkout fallback and no automatic
retry.

### Identity injection (decision 8)

The invariant is pinned as a one-way door: the broker mints a delegated agent-user token on
demand through the ADR-0005 three-leg chain, and that token is never reachable by the
per-role child process — never in the child's environment, never in a file or argv the
child can read, alive only for the single operation that needs it. The per-role OS user is
the runtime binding to the role's Entra identity: the broker mints for the role that owns
the calling process, and the token stays inside the broker's own process boundary. Tracker
access (read, comment) goes through a mandat-provided MCP server backed by the same broker,
wired via `--mcp-config`, so no tracker token reaches the child either. Anthropic model auth
is a separate credential plane, the owner's Claude Code OAuth token (`claude setup-token`)
surfaced to the `--bare` child via `apiKeyHelper` (PRD-0001 §Prerequisites), never mixed
with the git or tracker token.

The concrete git delivery mechanism is not pinned. S3 proved a delegated Entra token
authenticates against ADO repos over HTTPS; it did not prove an invariant-preserving
delivery. S3 exercised the write path with `http.extraheader` Bearer, which writes the token
into `.git/config` and process argv where a child running as the role OS user can read it —
a direct breach of the invariant above. Two further facts block a naive pin: the git
credential-helper stdio protocol returns username/password, so git emits `Authorization:
Basic`, not Bearer; and a helper that emits an `authtype`/Bearer credential needs git ≥ 2.46,
while the dev box and the S3 sandbox run git 2.43. The delivery mechanism is therefore an
OPEN QUESTION resolved by a named spike, **S-credential-delivery**, whose matrix is
{ credential-helper returning a Basic-password (git ≥ 2.46), credential-helper returning
`authtype`/Bearer (git ≥ 2.46), ephemeral per-operation `http.extraheader` scoped to one
push } × ( ADO accepts the delegated token in that shape ) × ( the token is invisible to a
process running as the role OS user ). A cell qualifies only if it clears all three columns;
the spike gates the runner's credential code, which is not written until a cell passes.
`mandat doctor` gains a git version floor parallel to the CLI ≥ 2.1.208 floor (ADR-0006,
§4.10): it asserts the installed git meets the minimum the chosen S-credential-delivery
mechanism requires and fails before first dispatch otherwise.

### Role config and playbook load (decision 9)

A RoleAgent is thin config plus a playbook, never per-role code (spec §4.4). The config
carries the mandate reference, remit defaults, autonomy ceiling (`draft-pr` is the MVP
ceiling), and model tier. The playbook is the skill set the runner loads into the child by
context: `--append-system-prompt-file` supplies the role playbook and the remit statement,
and skills plus MCP config come from the versioned harness bundle pulled at provision time
(spec §4.5), loaded from the per-role `CLAUDE_CONFIG_DIR` because `--bare` suppresses
ambient discovery. A new role is a new config entry plus a playbook, never a code change.
This is the product mirror of this repo's own `.claude/agents` (a thin role definition plus
skill hints), and it is a core part of this design, not a separate ADR.

## Package layout

Grounded in the current tree: `cmd/mandat/main.go` dispatches on `os.Args`, and
`internal/buildinfo` is the only plane package today. ADR-0001 fixes the convention:
`cmd/mandat` for the single command, `internal/` one package per plane as they materialize,
no `pkg/`. The skeleton adds one package per plane it touches.

| Package | Responsibility |
|---|---|
| `cmd/mandat` | CLI entrypoint; adds `serve` (the poll/dispatch daemon) and the credential-injection entrypoint (subcommand shape decided by spike S-credential-delivery) beside `version` |
| `internal/config` | `/etc/mandat/config.yaml`: repo registry, `identity_mode`, role table |
| `internal/task` | `TaskContract` type and validation |
| `internal/result` | `ResultContract` type, JSON-schema validation, the `.mandat/result.json` path constant |
| `internal/role` | RoleAgent config resolution and playbook reference |
| `internal/orchestrator` | The pure-function state machine (states, transition table, `Next`) |
| `internal/tracker` | `Tracker` (poll, comment, apply) and `Forge` (create PR) interfaces |
| `internal/adapter/azuredevops` | ADO implementation: WIQL poll, work-item read/comment, draft-PR create via REST |
| `internal/identity` | Token broker (ADR-0005 three-leg chain) and the git credential-delivery backing (mechanism gated by spike S-credential-delivery) |
| `internal/workspace` | Mirror cache, worktree, sparse checkout, remit-diff |
| `internal/runner` | Subprocess supervisor (ADR-0006), stream-json parse, session, OS-user isolation |
| `internal/verify` | Gate re-run, diff-inside-remit, PR-existence probe under the Reviewer identity |
| `internal/journal` | SQLite store (`tasks`, `runs`, `results`, `journal`), additive migrations |

The pure cores (`orchestrator`, `task`, `result`) get exhaustive unit tests; every I/O
seam gets a contract test with the §9 doubles (recorded ADO fixtures, a local bare git
origin, a fake `claude` binary emitting scripted stream-json and a ResultContract).

## Alternatives and trade-offs

**Identity mode: `agent-user-pair` versus `service-principal` for the Dev role.** Chosen:
`agent-user-pair` on ADO. It restores sponsor-linked, revocable, Entra-native attribution
across read, comment, clone, push, and PR create, proven by S1 round 3 and S3 (ADR-0005).
Trade-off: one extra Basic license per assignable role and a heavier provisioning path
(Global Administrator plus admin consent in the spikes). The rejected alternative,
`service-principal` per role, is simpler to provision but attributes writes to a bare
principal with display-name-only audit; it stays the portable fallback for non-ADO
surfaces, not the ADO write path (ADR-0005 supersedes the PRD's `client-credential`
phrasing).

**Runner seam: CLI subprocess versus embedded Agent SDK.** Chosen: the headless CLI
subprocess with a file contract (ADR-0006, D6). Trade-off: parsing stream-json and a
result file rather than an in-process object model, and a per-runner invocation adapter to
swap runners later. Rejected: the Agent SDK, which ships only for Python and TypeScript
and would force a second runtime onto the VM against D3, is a thin wrapper over the same
CLI JSON, and adds in-process callbacks that mandat deliberately replaces with OS-level
isolation.

**Remit source: config default versus a custom ADO field.** Chosen for the skeleton: the
repo registry's remit defaults for the named repo (decision 4), because it needs no ADO
schema change and keeps the slice thin. Trade-off: every task on a repo shares one remit,
so path-scoped stories are not yet expressible per work item. The richer mapping (a custom
ADO field versus a naming convention) is an open question below, deferred rather than
guessed.

**Invalid ResultContract: `needs-human` versus auto-retry.** Chosen: `needs-human` with no
retry (decision 3). Trade-off: a transient runner glitch parks a task for a human instead
of self-healing. Rejected for the skeleton: an auto-retry loop, which adds backoff state,
a repair budget, and a loop-detection guard the walking skeleton does not need to prove the
pipeline. Retry is deferred, listed as future work.

**Ingestion: 30s WIQL poll versus webhook.** Chosen: the poll (decision 5, spec §4.2),
because it needs no inbound endpoint, no shared-secret handling, and no public reachability
on the VM for the skeleton. Trade-off: up to 30s dispatch latency and steady WIQL calls.
Rejected for now: the Service Hook webhook, which lowers latency but adds an authenticated
listener; it is roadmap, and the poll and webhook already share the one dispatcher seam.

## Impact and rollout

The four contracts (`TaskContract`, `ResultContract`, the state set and transition table,
and the journal schema) are the one-way doors: once tasks and journal rows persist, a
breaking change to any of them is a migration. They evolve additively only (a new field, a
new enum value, a new nullable column, spec §6); `schema_version` is pinned at `1` on both
contracts so a future break is explicit. Migration on the deployed VM is additive SQLite
DDL run at boot (spec §4.10), never a rewrite.

The build-time gate is the same one CI already runs: `make check` (which includes the
`CGO_ENABLED=0` build that keeps D3/D4 honest) plus `npx govkit check`. New packages ride
that gate with no new gate definition (ADR-0001). Every new direct dependency (a JSON
Schema validator, a pure-Go SQLite driver) is vetted and recorded in the PR that
introduces it, first rung of the ladder that fits (ADR-0002); the SQLite driver must be
`CGO_ENABLED=0`-compatible or it fails the static-build gate on arrival.

Rollout order inside the skeleton follows the pipeline, each stage a contract-tested seam:
ingestion and the adapter first (recorded ADO fixtures), then the orchestrator and journal
(pure plus store tests), then the workspace and runner (local bare git origin, fake
`claude`), then verification and PR create. No stage is trusted from a downstream summary;
each is proven at its own seam (spec §9). Nothing here moves the §10 MVP boundary; it fills
it in.

## Open questions

These stay open by instruction; this RFC does not invent answers.

- **Productionized remit-on-work-item mapping.** Beyond the config default, whether a
  per-work-item remit rides a custom ADO field or a naming convention. Affects the adapter
  and the TaskContract's remit source; the config default is the skeleton's stand-in.
- **Retry and repair policy beyond `needs-human`.** Backoff, repair budget, and
  loop-detection for a task that failed once. Deferred; `needs-human` is the skeleton's
  only terminal for a failed run.
- **The concrete customer gate-config format.** The shape of the per-repo gate command list
  in `config.yaml` (a plain command array, a named-gate map, or a richer descriptor). The
  dogfood target uses `make check` then `npx govkit check`; the general format is unsettled.
- **The productionized per-role budget ceiling.** The default `--max-budget-usd 5.00` is a
  skeleton placeholder aligned to the PRD cost target; the per-role production value is
  unsettled.
- **Conditional access against the agent user.** Carried from ADR-0005: a tenant baseline
  requiring MFA or a compliant device on all users could refuse the non-interactive
  delegated token and drop ADO writes to the service-principal fallback. Untestable in the
  dogfood tenant (no Entra ID P1); must be validated in a P1 tenant before a CA-enforcing
  deployment.
- **Invariant-preserving git credential delivery (spike S-credential-delivery).** Which of
  { credential-helper Basic-password (git ≥ 2.46), credential-helper `authtype`/Bearer
  (git ≥ 2.46), ephemeral per-operation `http.extraheader` } delivers the delegated
  agent-user token so ADO accepts it and no process running as the role OS user can read it.
  S3 proved the token authenticates, not that any delivery preserves the on-disk/on-env
  invariant; the spike gates the runner's credential code and sets the `mandat doctor` git
  version floor. Until it resolves, the runner's push path stays unbuilt.
  **Resolved 2026-07-16 by S-credential-delivery:** the `mandat git-credential` helper
  delivers the delegated token as a Basic-auth password on git 2.43, invariant preserved
  (token never in `.git/config`, on disk, the child env, or argv); the F4 kill criterion is
  falsified and the runner's push path is unblocked.

## Success criteria (acceptance)

The 28 criteria below are the slice's success bar, and they fold the BA's acceptance set
into this RFC. Most are provable at a contract-test seam (§9) or against a cited decision,
but the §9 doubles have a ceiling: recorded ADO fixtures and a fake `claude` can simulate a
`createdBy` value or a probe's acting identity, they cannot establish live ADO/Entra
attribution. AC-26 (PR `createdBy` = the Dev agent user) and AC-27 (the probe runs under the
Reviewer identity) therefore fall to a live integration check against the kept S1/S3 spike
assets — a `mandat doctor`-style live assertion — not to the fixture or fake-`claude`
doubles. Every other criterion, AC-15 included (no token in the child env or on disk), is
genuinely seam-testable.

Ingestion and dispatch:

- [ ] AC-01 The 30s WIQL poll surfaces a work item assigned to `<dev-agent-user>` and the dispatcher enqueues exactly one task (consent = assignment, decision 4).
- [ ] AC-02 A work item not assigned to `<dev-agent-user>` is never enqueued.
- [ ] AC-03 Re-polling an already-enqueued work item creates no duplicate task (idempotent on `tracker_ref`).
- [ ] AC-04 Ingestion runs against a recorded ADO WIQL fixture with no live ADO call in the test (§9).
- [ ] AC-05 Dispatch writes a journal row: acting `system:orchestrator`, `act=dispatch`, `from_state` empty, `to_state=queued`.

Adapter to TaskContract:

- [ ] AC-06 The ADO adapter maps a fixture work item to a TaskContract with id, tracker_ref, `type=dev-task`, title, acceptance, `refs=[]`, `state=queued`, `role=dev`.
- [ ] AC-07 The TaskContract's remit is filled from the repo registry defaults for the named repo (config, decision 4), not from any ADO field.
- [ ] AC-08 A work item whose repo is absent from the registry yields no TaskContract and a journaled skip, no silent default.
- [ ] AC-09 The produced TaskContract validates against the schema; a missing required field fails validation before dispatch.

Runner spawn and session:

- [ ] AC-10 Claiming a queued task provisions a worktree via `git worktree add --reference` against a local bare git origin (§9) and sparse-checks-out only the remit paths.
- [ ] AC-11 The runner spawns the fake `claude` (§9) with the ADR-0006 flag set, including `--bare`, `--permission-mode dontAsk`, `--add-dir <worktree>`, `--session-id <uuid>`, and `--max-budget-usd <ceiling>`.
- [ ] AC-12 `--model` carries the role's tier: default `sonnet`, a per-role override to `opus` honored (decision 7).
- [ ] AC-13 A deterministic `--session-id` is written to `runs.session_id` before spawn and matches the `system/init` event's `session_id`.
- [ ] AC-14 The child runs as the per-role OS user with its own `HOME` and `CLAUDE_CONFIG_DIR` (ADR-0006).
- [ ] AC-15 No Entra token appears in the child environment or on disk; git credentials reach git only through the broker's on-demand mint, never via the child env or a file the child can read (decision 8; the delivery mechanism is gated by spike S-credential-delivery). Seam-testable with the fake `claude` and a stub broker.

Edit inside remit:

- [ ] AC-16 The fake `claude` edits a file inside the remit paths; the post-run branch diff touches only remit paths and the diff-inside-remit check passes.
- [ ] AC-17 A run whose diff touches a path outside the remit fails the diff-inside-remit check and routes to `needs-human` with a journaled `remit_violation` reason.
- [ ] AC-18 Failure to establish isolation routes the task `queued -> needs-human` (`setup_failed`) and journals the setup fault; there is no shared-checkout fallback and no auto-retry (§4.5).

ResultContract write and validate:

- [ ] AC-19 The subprocess writes the ResultContract to `.mandat/result.json` (`MANDAT_RESULT_PATH`); the supervisor reads that file and never parses stream-json as the outcome (ADR-0006, §4.6).
- [ ] AC-20 A schema-valid ResultContract `status=completed` with one artifact `{repo, branch, pr_url}` advances the task toward `in-review` (given green gates and the PR).
- [ ] AC-21 A missing or schema-invalid ResultContract routes `in-progress -> needs-human` with no auto-retry (decision 3), and the raw bytes plus `valid=0` land in `results`.
- [ ] AC-22 A ResultContract `status=needs_human` (`reason` set) routes `in-progress -> needs-human` and journals the reason; the state machine keys on the `status` enum alone.

Gate re-run:

- [ ] AC-23 Verification re-runs the per-repo gate list; the dogfood target's list is `make check` then `npx govkit check` (decision 6), executed in the verifier context, never trusting the agent summary (§4.7).
- [ ] AC-24 A green gate re-run is a precondition for `in-review`; a red gate re-run routes `in-progress -> needs-human` and journals the failing command and its exit code.
- [ ] AC-25 The gate result (command list, per-command exit codes) lands in `runs.gate_result`.

PR under the agent user, and journal:

- [ ] AC-26 The Dev branch is pushed with a delegated agent-user token minted on demand by the broker (ADR-0005 chain; invariant-preserving delivery gated by spike S-credential-delivery), attributed to `<dev-agent-user>`; a draft PR opens under `<dev-agent-user>` (PR `createdBy` = the agent user) and its url matches the ResultContract artifact. Verified by a live integration check against the kept S1/S3 spike assets (a `mandat doctor`-style live assertion), not by the §9 doubles, which can only simulate the `createdBy` value.
- [ ] AC-27 The PR-existence ground-truth probe runs under `<reviewer-agent-user>`, not the Dev identity (writer ≠ scorer as an IAM property, §4.1, §4.7), and confirms the PR before `in-review`; a probe that finds no PR, or a PR whose `createdBy` is not the Dev agent user, fires `probe_failed -> needs-human`. Verified by a live integration check against the kept S1/S3 spike assets (a `mandat doctor`-style live assertion), not by the §9 doubles, which can only simulate the probe's acting identity.
- [ ] AC-28 The end-to-end happy path produces a journal whose ordered rows reconstruct `dispatch -> claim_ok -> gate_rerun -> pr_opened -> probe_pr_exists -> result_ok -> in-review` with no `needs-human` hold, every row carrying acting identity, UTC timestamp, and (on the completed run) `total_cost_usd` and `usage`; no journal row is ever updated or deleted (§4.9).

## Decision

Build the MVP walking skeleton to the contracts and state machine pinned above. The Dev
role runs `identity_mode: agent-user-pair` on Azure DevOps (ADR-0005, superseding the
PRD's `client-credential` phrasing); the runner is the headless Claude Code subprocess with
the `.mandat/result.json` file contract (ADR-0006); the orchestrator is a pure-function
state machine; every transition and probe lands in the append-only journal keyed by acting
identity. The skeleton is done at `in-review` with a draft PR under the Dev agent user,
green gate re-run, and a complete journal (decision 2). The four contracts are one-way
doors and are fixed now; the plane package boundaries, WIQL query, poll interval, gate
command list, and budget default are two-way doors that US stories iterate. The concrete
git credential-delivery mechanism is pinned to neither: it is the open question spike
S-credential-delivery resolves, and the delegated-token invariant it must preserve is the
one-way door. The six open questions above stay open.

Recommendation to the owner: advance RFC-0001 to `proposed` after an independent red-team
pass (the harness rule before any status flip), then to `accepted` on ratification, at
which point US stories decompose per package and per acceptance criterion. Proposed
`owner:` for the human owner to set is the repo owner.
