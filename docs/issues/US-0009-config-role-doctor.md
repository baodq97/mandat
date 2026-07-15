---
id: US-0009
title: Config, role resolution, and mandat doctor
status: open
owner: TBD
date: 2026-07-16
priority: P2
---

# US-0009: Config, role resolution, and mandat doctor

As a mandat contributor, I want the repo registry, identity mode, and role table loaded
from `/etc/mandat/config.yaml`, role tiers resolved from it, and `mandat doctor` asserting
the CLI and git version floors, so that a new role is a config entry, never new code, and
bad preconditions fail before first dispatch.

## Source

RFC-0001 (accepted) §Role config and playbook load (decision 9), §Runner harness
(CLI version floor), §Identity injection (git version floor), §Package layout
(`internal/config`, `internal/role`), AC-07 (config side), AC-12 (config side).

## Scope

- `internal/config`: `/etc/mandat/config.yaml` — repo registry, `identity_mode`, role
  table.
- `internal/role`: RoleAgent config resolution — mandate reference, remit defaults,
  autonomy ceiling, model tier, playbook reference.
- `cmd/mandat`'s `doctor` subcommand.

## Acceptance criteria

- [ ] AC-9.1 Given a `config.yaml` with a repo registry entry for a named repo, observe
      `internal/config` exposes that repo's remit defaults (`{repo, base_branch,
      paths}`) for the adapter to consume (RFC-0001 AC-07, config side; pure-core unit
      test — file-parsing has no I/O double named in RFC-0001, it is not one of the
      three §9 doubles).
- [ ] AC-9.2 Given a role table entry with no per-role override, observe role resolution
      returns the default model tier `sonnet`; given an entry with a per-role override to
      `opus`, observe resolution returns `opus` (RFC-0001 AC-12, config side; pure-core
      unit test).
- [ ] AC-9.3 Given a role's autonomy ceiling `draft-pr` (the MVP ceiling, RFC-0001
      §Role config and playbook load) is configured, observe role resolution surfaces it
      unchanged — no code path raises a ceiling (RFC-0001: "A new role is a new config
      entry plus a playbook, never a code change"; pure-core unit test).
- [ ] AC-9.4 Given an installed `claude` CLI below version 2.1.208, observe `mandat
      doctor` fails with a message naming the version floor, before first dispatch
      (RFC-0001 §Runner harness: "Require CLI ≥ 2.1.208... `mandat doctor` asserts it
      before first dispatch"). No RFC-0001 AC-NN names this explicitly — flagged in the
      gaps list, cited to RFC prose instead of an AC number.
- [ ] AC-9.5 Given the git version floor required by whichever S-credential-delivery
      mechanism is eventually chosen, observe `mandat doctor` asserts the installed git
      meets that minimum and fails otherwise (RFC-0001 §Identity injection: "`mandat
      doctor` gains a git version floor... asserts the installed git meets the minimum
      the chosen S-credential-delivery mechanism requires"). This criterion cannot be
      fully implemented until S-credential-delivery names the mechanism — partially
      blocked on the same spike as US-0008.

## Remit

File-disjoint allowed paths:

- `internal/config/**`
- `internal/role/**`
- `cmd/mandat/doctor.go`
- `cmd/mandat/doctor_test.go`

## Dependencies

None for AC-9.1..AC-9.3 (pure core, file parsing). AC-9.5 depends on US-0008's spike
resolution. Feeds US-0004 (repo registry), US-0006 (role tier), US-0008
(`identity_mode`).

## Notes

`cmd/mandat/main.go` already exists and dispatches on `os.Args` (`version` today); adding
the `doctor` case touches that shared switch statement. US-0010 also touches
`cmd/mandat/main.go` to add `serve` — land this story first to avoid a merge collision on
that one file (see US-0010's dependency note).
