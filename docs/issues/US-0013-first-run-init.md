---
id: US-0013
title: First-run experience — `mandat init` guided setup
status: in-progress
owner: baodq97
date: 2026-07-16
priority: P2
---

# US-0013: First-run experience — `mandat init` guided setup

As an operator standing up a new mandat installation, I want `mandat init` to interview me
and generate a reviewable `config.yaml`, embedded playbook templates, and an optional
systemd unit, then run the existing doctor checks, so that first-run setup collapses from a
sequence of manual steps into one guided command instead of hand-issued REST calls and
hand-written YAML.

## Source

`docs/research/cli-first-run-survey.md` (peer survey, 26 tools, 2026-07-16). Design spec
§4.10 ("install time is one download... setup time is one command"); RFC-0001.
`internal/config/config.go` (the loader `init` must satisfy: `Load`, `applyDefaults`,
`validate`, `FieldError`). `cmd/mandat/doctor.go` (the checks `init` runs at the end).
GETTING-STARTED.md (the current manual runbook this story partially replaces). US-0009
(config, role resolution, doctor: the loader and doctor this story builds on). US-0011,
US-0012 (neighbor story structure).
`docs/research/entra-agent-id-provisioning-surface.md` (Spike rounds 1 and 2, verified
2026-07-16: pins the AC-13.1 ADO discovery chain and confirms the Agent ID registry read
surface AC-13.3(b) uses for the identity picker).

## Design boundary

`config.yaml` is a reviewable artifact: autonomy ceilings and every other operational
setting live in a file the customer reviews like code (CLAUDE.md design invariants,
README design invariants). `init` generates and explains the file: every optional field
ships with a comment stating its default and effect, and `init` writes no configuration
state anywhere it does not also print or leave in a diffable file. It never hides setup
behind an opaque store, a hidden cache, or a write path the operator cannot inspect with
`git diff` or a text editor. `init` is expected to run under `sudo`, for the root-owned
`/etc/mandat/config.yaml` write; it resolves the invoking operator through `SUDO_USER` for
every per-user artifact it writes, including the systemd unit (AC-13.6).

## Problem

GETTING-STARTED.md's steps 2 through 7 are manual today: roughly ten hand-issued Graph and
Azure DevOps REST calls across two principals (steps 2–3), one hand-written ~50-line
`config.yaml` (step 5), two hand-authored playbook files (step 5), and one hand-written
systemd unit (step 7). Of the full walkthrough, only step 6 (`mandat doctor`) is automated.
An operator who already has working `az` credentials for the target ADO org still
transcribes org, project, and repo values into YAML by hand, and copies playbook prose from
the walkthrough verbatim because no shipped template exists.

## Scope — Phase 1 (this story)

Deterministic, no new cloud write surface: `init` reads through the operator's existing
`az`-derived token to discover values and prompts for what it cannot discover; it issues no
Graph or ADO write calls of its own.

- `mandat init` interviews the operator and writes `/etc/mandat/config.yaml`: discovers
  ADO org, project, and repo urls via the operator's existing `az`-derived token where
  reachable, and prompts for each value it cannot discover.
- Every optional field in the written file carries a comment naming its default, derive
  rule, or explicit no-default plus omission behavior (AC-13.2): `applyDefaults` covers only
  `tracker.states.in_progress` and `runner.pool_size`; `budget.max_usd_in_flight` derives as
  `runner.pool_size * budget.max_usd_per_run` when omitted; `roles.<name>.model_tier` has no
  default (omitted means no `--model` flag is passed); and every other `omitempty` field in
  `config.go` states its own default or derive rule the same way.
- `init` ships embedded playbook templates for the `dev` and `reviewer` roles and writes
  them to the configured `playbook` paths on request, instead of requiring the operator to
  hand-author the two stub files GETTING-STARTED §5 shows today.
- `init` optionally writes the systemd user unit (the GETTING-STARTED §7 shape) when the
  operator asks for it; skipped by default for operators managing their own process
  supervision.
- `init` ends by invoking the same checks `mandat doctor` runs and prints the same
  PASS/FAIL/WARN table, so a completed `init` run doubles as its own preflight.

## Out of scope

Automating the Entra provisioning ceremony (blueprint creation, agent identity creation,
paired agent user creation, `oauth2PermissionGrants`, and ADO user entitlements, per
GETTING-STARTED §2, the manual runbook this covers) is chartered as US-0014
(`mandat provision`), not this story. `docs/research/entra-agent-id-provisioning-surface.md`
(verified 2026-07-16) corrects the prior assumption here: the write surface is documented at
graph-rest-1.0, not beta, and the ceremony does not need a Global Administrator token. What
stays privileged is the one-time Agent ID Developer or Administrator role for blueprint
creation, the two admin-consent actions (granting the blueprint's own app permission, and the
ADO `oauth2PermissionGrant`), and ADO org admin for entitlements. Phase-2 design target:
config-writing and token-minting stay separate commands, mirroring the `aws configure sso`
/ `aws sso login` split (survey patterns 1, 11).

## Acceptance criteria

- [ ] AC-13.1 Given an operator with a working `az`-derived token for the target ADO
      organization, running `mandat init` discovers that org, its accessible projects, and
      their repo urls without the operator retyping them, through the pinned chain
      (`docs/research/entra-agent-id-provisioning-surface.md`, Spike round 2): an
      ADO-resource token (`az account get-access-token --resource
      499b84ac-1321-427f-aa17-267ca6975798`) feeds `profile/profiles/me` for the member id,
      `accounts?memberId={id}` for the org list, `{org}/_apis/projects` for projects, and
      `{org}/{project}/_apis/git/repositories` for repo names and remote urls. It falls back
      to a prompt for any value discovery cannot resolve (no ADO org reachable, ambiguous
      project match, or an unreachable git remote). Before writing, `init` validates the
      discovered token/tenant against a real ADO endpoint and refuses to write
      `config.yaml` when validation fails.
- [ ] AC-13.2 Given a completed `init` interview, observe the written `/etc/mandat/config.yaml`
      parses and passes `config.Load` unmodified, and every optional field present in the
      file (every `omitempty`-tagged key in `internal/config/config.go`) carries an adjacent
      comment that names its default value (`tracker.states.in_progress: Doing`,
      `runner.pool_size: 1`), states its derive rule (`budget.max_usd_in_flight`: derived as
      `runner.pool_size * budget.max_usd_per_run` when omitted), or states "no default" with
      the omission behavior (`roles.<name>.model_tier`: no default; omitted means no
      `--model` flag is passed).
- [ ] AC-13.3 Config-surface audit: enumerate every field `config.go`'s `validate*`
      functions check, plus the two fields `applyDefaults` resolves, and place each in
      exactly one of three categories. (a) `applyDefaults`-defaulted, no operator input:
      `tracker.states.in_progress` (defaults to `Doing`), `runner.pool_size` (defaults to
      `1`). (b) `init`-supplied without prompting: constants fixed by this story's scope
      (`tracker.kind`: hardcoded to `azure-devops`, the only tracker phase 1 ships;
      `entra.identity_mode`: hardcoded to `agent-user-pair`, ADR-0005's recommended mode
      for Azure DevOps); values discovered from the operator's existing `az` session
      (`tracker.org`, `tracker.project`, `repos.<>.url`, `repos.<>.base_branch`: the AC-13.1
      ADO chain; `entra.tenant`: the az session's tenant claim; `roles.<>.agent_identity_id`,
      `roles.<>.agent_user_id`, `roles.<>.agent_user_name`: a registry picker over the
      blueprint's children, the same az-session pattern AC-13.1 uses for ADO, per the
      research doc's "registry (read surface)" table); and template-derived paths
      (`roles.<>.playbook`: the path AC-13.5's embedded template is written to). (c)
      Irreducible prompts, where no discovery signal or default exists: confirming the
      discovered tracker org/project (accept or override), which discovered repo(s) to
      register plus each one's remit `paths` and `gates`, `auth.mode`, `entra.blueprint`
      (only when US-0014's registry finds none to pick from), `roles.<>.autonomy_ceiling`,
      and `budget.max_usd_per_run`. A field found in none of the three categories is a
      defect in this story, not an accepted gap.
- [x] AC-13.4 Given `init` writes a config with a missing irreducible field (interview
      aborted early, or a discovery step failed silently), observe `config.Load` on that
      file returns a `ValidationErrors` value whose `FieldError.Path` names the exact
      dotted field (e.g. `roles.dev.agent_user_name`) and whose `Reason` states the fix,
      matching the existing `FieldError` shape rather than a generic parse failure.
- [x] AC-13.5 Given the operator selects the `dev` and `reviewer` roles during the
      interview, observe `init` writes the embedded playbook template to each role's
      configured `playbook` path, and the written content differs per role: the `dev`
      template names the remit-scoped implement / self-review / commit-push /
      ResultContract-write sequence GETTING-STARTED §5 describes; the `reviewer` template
      (`report` autonomy ceiling, Readers group per GETTING-STARTED §3) names a read /
      probe / report sequence and contains no commit or push step.
- [x] AC-13.6 Given the operator answers "yes" to installing the systemd unit, observe
      `init` writes a unit file matching the GETTING-STARTED §7 shape (`ExecStart` sourcing
      the env file, `Restart=on-failure`) to the invoking operator's user systemd directory
      (resolved via `SUDO_USER` per the Design boundary, never root's own
      `~/.config/systemd/user/`); given "no" or the default (unattended) answer, observe no
      unit file is written and no `systemctl` call is made.
- [x] AC-13.7 Given a completed `init` run, observe it invokes the same check functions
      `mandat doctor` runs (`cmd/mandat/doctor.go`'s `claudeVersionCheck`,
      `gitVersionCheck`, `sqliteCheck`, `trackerCheckFor`, `reviewerIdentityCheck`,
      `diskCheck`) against the config it just wrote — no second validator set — and prints
      the identical CHECK/STATUS/DETAIL table shape, so a green `init` run is evidence, not
      a claim. `init` exits non-zero when any check reports FAIL: a sharp tri-state
      (`flutter doctor`'s model), never `brew doctor`'s advisory shrug, because this gates
      Entra identity and worktree isolation.
- [ ] AC-13.8 GETTING-STARTED.md shrinks: steps 5 (write the config), the playbook
      sub-step of step 5, and step 7's unit-file sub-step collapse into one step that
      runs `mandat init` and answers its prompts; step 2 (Entra provisioning) is unchanged
      and stays the longest step in the document, matching this story's phase-1 scope.
- [ ] AC-13.9 Given `mandat init --non-interactive`, observe it requires every irreducible
      field (tracker org/project, repo url + remit paths + gates, per-role identity
      ids/UPNs) as a flag and errors naming the specific missing flag instead of prompting.
      Given stdin is not a TTY, observe `init` behaves as if `--non-interactive` was passed,
      so it never hangs in CI. `--non-interactive`, and the non-TTY autodetect path,
      additionally imply `--yes` for the AC-13.12 write confirmation: no prompt of any kind,
      including the pre-write diff confirmation, fires on either path.
- [ ] AC-13.10 Given `MANDAT_*` environment variables set for any init input, observe
      `init` accepts them with precedence flags > env > existing config values, and observe
      env inputs carry non-secret values only. Env vars feed the interview only: the
      written `config.yaml` remains the sole runtime source, and `mandat serve` never reads
      governed settings (remits, ceilings) from environment variables.
- [ ] AC-13.11 Given a second `init` run over an existing `/etc/mandat/config.yaml`,
      observe each existing value is presented as the bracketed prompt default (Enter keeps
      it), and observe fields the operator does not change come out byte-identical to the
      file before the rerun.
- [ ] AC-13.12 Given `init` is about to write, observe it prints a diff of what will change
      in `config.yaml` and asks for confirmation before writing; given `--yes`, observe the
      confirmation is skipped for automation; given a fresh install with no existing file,
      observe the diff shown is the whole file.
- [x] AC-13.13 Given a completed `init` run finishes printing the doctor table, observe it
      prints the next command to run and a security note naming the Entra identities and
      remit paths this VM now operates under.
- [x] AC-13.14 Given `install.sh` completes, observe it prints the next step
      (`mandat init`) after running `mandat version`, and observe it never auto-launches
      the init wizard or mutates `config.yaml` itself.

## Remit

File-disjoint allowed paths:

- `cmd/mandat/init.go`
- `cmd/mandat/init_test.go`
- `cmd/mandat/main.go` (register the `init` subcommand)
- `internal/config/**` (template/default-comment support only; no change to the validated
  schema itself)
- `GETTING-STARTED.md`
- `install.sh` (AC-13.14: prints the next-step message only; no config writes)

## Dependencies

Depends on US-0009 (config loader, role resolution, `doctor`: this story wraps and reuses
both) and US-0011/US-0012 only for shared config keys (`tracker.states.in_progress`,
`runner.pool_size`) whose defaults this story's comments must state accurately. Not
file-disjoint from any open story touching `internal/config` at the same time; sequence
after those land.

## Gaps

- The `az`-derived ADO discovery chain gap is closed: the research doc's Spike round 2
  (2026-07-16) pins the exact chain and confirms it end to end on the dogfood org
  (AC-13.1); no discovery-chain gap remains for this story's scope.
- Phase 2 (automating the Entra provisioning ceremony) is now chartered as US-0014, which
  itself needs a spike to pin the least-privilege `az` scope check before implementation;
  this story only records the boundary in "Out of scope" and does not open the spike itself.
