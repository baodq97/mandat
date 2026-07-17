---
id: US-0016
title: Auto-derive first-run config from the az session and Agent-ID registry
status: open
owner: baodq97
date: 2026-07-17
priority: P2
---

# US-0016: Auto-derive first-run config from the az session and Agent-ID registry

As an operator standing up a new mandat installation, I want `mandat init` to derive
`entra.tenant`, the repo `base_branch`, and the six role-identity fields from my existing
`az` session and the Entra Agent-ID registry, prompting only for what it cannot derive, so
that first-run setup follows the az-cli convention the peer survey found: auto-discover,
offer the result as a confirmable default, and keep every field overridable by flag,
environment variable, or typed answer.

## Sources

- `docs/research/init-provision-pilot.md` (piloted 2026-07-17 on the dogfood Entra tenant
  and ADO org; the primary source for this story). Findings F3 and F4 record the auto-derive
  behavior this story charters, live and end to end.
- `docs/research/entra-agent-id-provisioning-surface.md` (verified 2026-07-16): the registry
  read surface (`GET /applications/microsoft.graph.agentIdentityBlueprint`, the agent
  identity and agent user lists, and the `agentUser.identityParentId` pairing link) this
  story's picker reads from.
- `docs/research/cli-first-run-survey.md`, pattern 10 (auto-discovery, still surfaced through
  a picker rather than silently committed) and the closest-peer comparison table: none of the
  26 surveyed tools auto-discover identity/URL fields from an existing operator session, so
  this derivation is the improvement the survey identified as open.
- `docs/issues/US-0013-first-run-init.md`: the interview this story enhances
  (`runInteractiveInterview`, `attemptDiscovery`, AC-13.3(c)'s irreducible-prompt set) and
  AC-13.3(b)'s note that a registry picker would move the six role-identity fields from
  category (c) (irreducible prompt) toward category (b) (`init`-supplied) once one ships.
- `docs/issues/US-0014-provision-ladder.md`, AC-14.9: names exactly this picker as the
  consumer of the registry `mandat provision` populates.
- `docs/issues/US-0015-discovery-token-tenant-pin.md`: the tenant pin this story's derived
  `entra.tenant` value feeds; `azCLITokenSource` mints the discovery and validation tokens
  against whichever tenant is resolved by the time those calls run.
- `cmd/mandat/init.go`: `resolvePrefill`, `roleIdentitiesFromRegistry`,
  `defaultBranchForRepoURL`, `deriveTenant`/`productionDeriveTenant`, `discoverEntra`/
  `productionDiscoverEntra`, and the merge helpers (`mergeEnvIntoUnset`,
  `mergeEnvOverPrior`) this story's fields flow through.
- `internal/entra/entra.go`: `Client.DiscoverRegistry`, `Registry.PairedUser`, the read-only
  half of the package this story consumes.
- `internal/discovery/discovery.go`: the `defaultBranch` field on a discovered repository,
  including the null-defaultBranch (empty repository) case.

## Note on status

This story documents behavior already implemented and pilot-verified, not a design proposal.
`cmd/mandat/init.go`'s derivation logic and `internal/entra`'s registry read already ship on
`main`. Status starts at `open`, the type's start status per `govkit.yml`: a governed doc
never self-flips its own status. A human owner ratifies the flip to `done` in a separate
accept commit, citing this story and the pilot evidence above.

## Scope

Three fields derive automatically on a fresh install, when the machine has the session or
registry access to know them, and fall back to a prompt otherwise:

- `entra.tenant`, from the active `az` session's tenant id claim (`az account show`).
- The registered repo's `base_branch`, from the ADO repository's `defaultBranch`, stripped of
  its `refs/heads/` prefix.
- The six role-identity fields, `roles.dev.agent_identity_id`/`agent_user_id`/
  `agent_user_name` and the same three for `roles.reviewer`, from the Entra Agent-ID registry:
  each role matched to an agent identity by display name, then paired to its agent user
  through `identityParentId`.

Every derived value renders as a bracketed prompt default in the interactive interview, never
a silent write: the operator confirms with Enter or types an override, matching the
confirm-or-override pattern US-0013 AC-13.1 already established for tracker org, project, and
repo url.

## Non-goal

Creating or provisioning identities. `internal/entra`'s write side (`CreateAgentIdentity`,
`EnsureAgentIdentity`, `DeleteAgentIdentity`) belongs to US-0014's `mandat provision` ladder.
This story only reads the registry (`DiscoverRegistry`, `ListBlueprints`,
`ListAgentIdentities`, `ListAgentUsers`) to prefill `init`'s interview; it issues no Graph or
ADO write call.

## Acceptance criteria

- [ ] AC-16.1 Given a fresh install (no existing `/etc/mandat/config.yaml`, and no env-seeded
      prior either), `resolvePrefill` calls `deriveTenant` (`az account show --query
      tenantId`) and the interview offers the result as the bracketed default for
      `entra.tenant` (`iv.requiredWithDefault("entra.tenant", p.entraTenant, nil)`). The
      resolved tenant is the same value `attemptDiscovery` and `validateADOBeforeWrite` pin
      the ADO discovery and pre-write validation tokens to (US-0015 AC-15.1, AC-15.2), never
      `az`'s ambient default.
- [ ] AC-16.2 Given a successful discovery run, `defaultBranchForRepoURL` reads the matched
      repository's `defaultBranch`, already stripped of its `refs/heads/` prefix by
      `internal/discovery`, and the interview offers it as the bracketed default for `repo
      base_branch`. Given an empty repository (ADO reports `defaultBranch: null`), observe
      `defaultBranchForRepoURL` returns `""` and the prompt falls back to a plain required
      entry, never a wrong or blank default silently accepted.
- [ ] AC-16.3 Given the Entra Agent-ID registry is reachable, `discoverEntra` reads the
      blueprint's agent identities and agent users, and `roleIdentitiesFromRegistry` matches
      each of the `dev` and `reviewer` roles to the first agent identity whose `DisplayName`
      contains the role name (case-insensitive), pairing it to its agent user through
      `Registry.PairedUser`'s `identityParentId` link. The interview prefills all six fields,
      `agent_identity_id`, `agent_user_id`, and `agent_user_name` for both roles, as bracketed
      defaults. A matched identity with no paired user yet prefills the identity id only and
      still prompts for the two user fields.
- [ ] AC-16.4 Every derived value is overridable through the same three input paths US-0013
      established: a non-interactive flag (`--entra-tenant`, `--base-branch`,
      `--dev-identity-id`, `--dev-user-id`, `--dev-user-upn`, `--reviewer-identity-id`,
      `--reviewer-user-id`, `--reviewer-user-upn`), a `MANDAT_*` environment variable, or a
      typed answer at the interactive prompt. Given a re-run over an existing config.yaml (or
      any env-seeded prior), observe `resolvePrefill` returns an empty prefill and issues no
      `az` or Graph call at all: a re-run opts out of derivation entirely and offers the prior
      config value as the default instead.
- [ ] AC-16.5 Given `deriveTenant` or `discoverEntra` returns an error (az absent, logged out,
      or the Graph registry unreachable), observe `resolvePrefill` leaves the corresponding
      field or fields empty rather than propagating the error, and the interview prompts for
      `entra.tenant` from scratch (printing a note and skipping ADO discovery, per US-0015
      AC-15.5) or for whichever role's identity fields found no registry match. `init` never
      exits non-zero, hangs, or hard-fails because the registry or `az` is unreachable; the
      affected field alone degrades to a plain prompt.
- [ ] AC-16.6 Every derivation path in AC-16.1 through AC-16.5 is exercised by a contract test
      substituting an injectable seam, never a live `az` or Graph call: `deriveTenant` and
      `discoverEntra` are package-level `var` seams a test reassigns to a fake; the tenant pin
      on the underlying `az` invocation is proven through the `tokenSource` func-field seam;
      and the registry reads themselves are proven against an `httptest` server through
      `entra.Config.GraphBaseURL`. Reverting any one derivation reproduces a failing test, not
      a silently-passing one.

## Remit

File-disjoint allowed paths (the existing footprint this story documents; no new files):

- `cmd/mandat/init.go`
- `cmd/mandat/init_test.go`
- `internal/discovery/**` (read-only reference: the `defaultBranch` field this story consumes)
- `internal/entra/**` (read-only reference: the registry read surface this story consumes)

## Dependencies

Builds on US-0013 (the interview this story enhances: `runInteractiveInterview`,
`attemptDiscovery`, and the AC-13.3(c) irreducible-prompt set) and US-0014 (the Agent-ID
registry `internal/entra` reads; AC-14.9 anticipated exactly this picker). Consistent with
US-0015: the derived `entra.tenant` value is the input US-0015's tenant pin consumes at both
call sites (`attemptDiscovery`, `validateADOBeforeWrite`).
