---
id: US-0013
title: First-run experience — `mandat init` guided setup
status: open
owner: TBD
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

Design spec §4.10 ("install time is one download... setup time is one command"); RFC-0001.
`internal/config/config.go` (the loader `init` must satisfy: `Load`, `applyDefaults`,
`validate`, `FieldError`). `cmd/mandat/doctor.go` (the checks `init` runs at the end).
GETTING-STARTED.md (the current manual runbook this story partially replaces). US-0009
(config, role resolution, doctor: the loader and doctor this story builds on). US-0011,
US-0012 (neighbor story structure).

## Design boundary

`config.yaml` is a reviewable artifact: autonomy ceilings and every other operational
setting live in a file the customer reviews like code (CLAUDE.md design invariants,
README design invariants). `init` generates and explains the file: every optional field
ships with a comment stating its default and effect, and `init` writes no configuration
state anywhere it does not also print or leave in a diffable file. It never hides setup
behind an opaque store, a hidden cache, or a write path the operator cannot inspect with
`git diff` or a text editor.

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
- Every optional field in the written file carries a comment naming its default (mirroring
  `config.go`'s `applyDefaults`: `tracker.states.in_progress`, `runner.pool_size`,
  `budget.max_usd_in_flight`, `roles.<name>.model_tier`, and every other `omitempty` field).
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
GETTING-STARTED §2, the manual runbook this covers) is deferred to a phase 2 story. The
Graph agent-identity surface is beta and the ceremony needs a Global Administrator
delegated token that `init` running as the operator does not hold. Automating it needs its
own spike and RFC to settle the token-acquisition and consent model, not a silent extension
of this story's scope.

## Acceptance criteria

- [ ] AC-13.1 Given an operator with a working `az`-derived token for the target ADO
      organization, running `mandat init` discovers that org, its accessible projects, and
      their repo urls without the operator retyping them, and falls back to a prompt for
      any value discovery cannot resolve (no ADO org reachable, ambiguous project match, or
      an unreachable git remote).
- [ ] AC-13.2 Given a completed `init` interview, observe the written `/etc/mandat/config.yaml`
      parses and passes `config.Load` unmodified, and every optional field present in the
      file (every `omitempty`-tagged key in `internal/config/config.go`) carries an adjacent
      comment naming its default value.
- [ ] AC-13.3 Config-surface audit: enumerate every field `config.go`'s `validate*`
      functions check. Each is either satisfied by `applyDefaults` (a sane default, no
      operator input required) or belongs to the irreducible set this story requires
      `init` to prompt for: tracker org and project, per-repo url + remit paths + gates,
      and per-role identity ids/UPNs (`agent_identity_id`, `agent_user_id`,
      `agent_user_name`). No field outside those two categories exists; a field found in
      neither is a defect in this story, not an accepted gap.
- [ ] AC-13.4 Given `init` writes a config with a missing irreducible field (interview
      aborted early, or a discovery step failed silently), observe `config.Load` on that
      file returns a `ValidationErrors` value whose `FieldError.Path` names the exact
      dotted field (e.g. `roles.dev.agent_user_name`) and whose `Reason` states the fix,
      matching the existing `FieldError` shape rather than a generic parse failure.
- [ ] AC-13.5 Given the operator selects the `dev` and `reviewer` roles during the
      interview, observe `init` writes the embedded playbook template to each role's
      configured `playbook` path, and the written file's content differs from an empty
      stub (it names the role's remit-scoped, self-review, commit/push, ResultContract-write
      sequence GETTING-STARTED §5 describes for the Developer playbook, adapted per role).
- [ ] AC-13.6 Given the operator answers "yes" to installing the systemd unit, observe
      `init` writes a unit file matching the GETTING-STARTED §7 shape (`ExecStart` sourcing
      the env file, `Restart=on-failure`) to the user systemd directory; given "no" or the
      default (unattended) answer, observe no unit file is written and no `systemctl` call
      is made.
- [ ] AC-13.7 Given a completed `init` run, observe it invokes the same check functions
      `mandat doctor` runs (`cmd/mandat/doctor.go`'s `claudeVersionCheck`,
      `gitVersionCheck`, `sqliteCheck`, `trackerCheckFor`, `reviewerIdentityCheck`,
      `diskCheck`) against the config it just wrote, and prints the identical
      CHECK/STATUS/DETAIL table shape, so a green `init` run is evidence, not a claim.
- [ ] AC-13.8 GETTING-STARTED.md shrinks: steps 5 (write the config), the playbook
      sub-step of step 5, and step 7's unit-file sub-step collapse into one step that
      runs `mandat init` and answers its prompts; step 2 (Entra provisioning) is unchanged
      and stays the longest step in the document, matching this story's phase-1 scope.

## Remit

File-disjoint allowed paths:

- `cmd/mandat/init.go`
- `cmd/mandat/init_test.go`
- `cmd/mandat/main.go` (register the `init` subcommand)
- `internal/config/**` (template/default-comment support only; no change to the validated
  schema itself)
- `GETTING-STARTED.md`

## Dependencies

Depends on US-0009 (config loader, role resolution, `doctor`: this story wraps and reuses
both) and US-0011/US-0012 only for shared config keys (`tracker.states.in_progress`,
`runner.pool_size`) whose defaults this story's comments must state accurately. Not
file-disjoint from any open story touching `internal/config` at the same time; sequence
after those land.

## Gaps

- The discovery mechanism for "the operator's existing az-derived token" (which `az`
  subcommands, what scope, how failure is distinguished from "no such credential") is not
  pinned by any source read for this story; the implementer names the exact discovery
  calls and their fallback behavior.
- Whether `init` is idempotent on a second run against an existing `/etc/mandat/config.yaml`
  (spec §4.10 states re-running `init` "updates config in place and never destroys state")
  is not covered by this story's acceptance criteria; a first-run-only implementation
  satisfies AC-13.1 through AC-13.8, and idempotent re-run is flagged here as a follow-up
  the doc owner may fold into this story or split into its own.
- Phase 2 (automating the Entra provisioning ceremony) has no spike or RFC yet; this story
  does not open one, it only records the boundary in "Out of scope".
