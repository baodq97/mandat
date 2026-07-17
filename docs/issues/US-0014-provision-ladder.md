---
id: US-0014
title: Provisioning ladder — `mandat provision` ensure-flows for Entra Agent ID
status: in-progress
owner: baodq97
date: 2026-07-16
priority: P2
---

# US-0014: Provisioning ladder — `mandat provision` ensure-flows for Entra Agent ID

As an operator standing up a new mandat installation, I want `mandat provision` to run the
Entra agent-identity ceremony (auth, blueprint, per-role identity) as idempotent ensure-flows,
so that GETTING-STARTED §2's manual REST-call sequence collapses into one command instead of
ten hand-issued Graph and Azure DevOps calls.

## Source

`docs/research/entra-agent-id-provisioning-surface.md` (verified 2026-07-16 against
graph-rest-1.0, Microsoft Learn; the primary source for this story). Every endpoint, role,
and permission below is taken from it, not invented. `docs/research/cli-first-run-survey.md`
patterns 1 and 11 (config-writing and token-minting stay separate, composable verbs, the same
split this story applies to blueprint/identity writes vs. token minting). `GETTING-STARTED.md`
§2 (the manual runbook this story automates). US-0013 (neighbor story: the first-run interview
whose AC-13.1 discovery and AC-13.3 irreducible per-role identity fields consume the registry
this story produces, via pickers). `docs/research/init-provision-pilot.md` (piloted
2026-07-17 on the dogfood Entra tenant and ADO org): finding F1 resolves the least-privilege
Gap below positive for the owner agent-identity create/delete path; F2 and F4 ground the
`internal/entra` / `cmd/mandat/provision.go` create ensure-flows that already ship on `main`.

## Background: the deferral this story resolves

US-0013's Out of scope deferred Entra provisioning to a phase 2 story on the grounds that the
Graph agent-identity surface was beta and needed a Global Administrator token. The research
doc verifies both premises are now stale: every write endpoint in the ladder below is
documented at graph-rest-1.0, and the only role requirement is Agent ID Developer or
Administrator for one-time blueprint creation. Identity creation under an owned blueprint
needs no Agent ID role at all, so this story opens the phase 2 work US-0013 deferred.

## Scope

Three ensure-semantics flows. Each is idempotent: running it a second time creates nothing
new and reports the state it found instead.

1. **ensure-auth.** Detect `az`. If absent or logged out, drive `az login --tenant <tenant>`
   (device code). Mint Graph tokens through `az account get-access-token` for the flows below.
   Never persist a token to disk, `config.yaml`, or any file; a token lives only for the
   in-process call that needs it.
2. **ensure-blueprint.** List existing blueprints via the registry read surface
   (`GET /applications/microsoft.graph.agentIdentityBlueprint`). If one tagged for this
   installation already exists, create nothing and report its `appId`. If none exists, create
   the blueprint and its blueprint principal (`POST .../agentIdentityBlueprint`,
   `POST .../agentIdentityBlueprintPrincipal`) exactly once; the creator becomes owner; record
   the resulting `appId` for `config.yaml`. This is a one-time step per installation and
   requires the operator to hold the Agent ID Developer or Administrator role. Check for the
   role as a precondition and fail with a named-role error before attempting any write, never
   a raw Graph 403.
3. **ensure-role-identity** (run once per configured role). Create the agent identity under
   the owned blueprint (`POST .../agentIdentity`), the owner path, no Agent ID role required.
   Create the paired agent user (`POST .../agentUser`) using the blueprint's own
   client-credential token, the client the research doc recommends for this call, reusing
   machinery mandat's broker already implements. Then run the two steps that stay
   privileged regardless of role: the ADO `oauth2PermissionGrant` (admin consent) and the ADO
   entitlement plus group add (ADO org admin). When the operator's session lacks the privilege
   for either of these two steps, the flow does not fail opaquely. It prints the exact call
   (method, endpoint, body) for an admin to run, and continues the rest of the ladder.

`mandat provision` writes non-secret identifiers only (tenant id, blueprint `appId`, agent
identity/user ids and UPNs) to the registry US-0013's interview reads; it never writes a
token or secret to `config.yaml` or any other file, matching the config-write/token-mint
separation the CLI survey documents (patterns 1, 11).

## Out of scope

- Production Arc/federated-identity-credential (FIC) setup for the blueprint credential
  (research doc step 3: secret/cert needs the Agent ID Administrator role; production mode
  is FIC/Arc per the design's "no secrets on disk" invariant). This story covers create-time
  identity provisioning only, not the production credential story.
- Multi-tenant provisioning. One blueprint, one tenant, per installation.
- Deleting or rotating identities. This story is create-only; lifecycle operations beyond
  create (revoke, rotate, delete a blueprint or identity) are not in scope.

## Acceptance criteria

- [ ] AC-14.1 Given `az` is absent or the operator is logged out, `mandat provision
      ensure-auth` drives `az login --tenant <tenant>` (device code). Given the operator is
      already logged in, observe ensure-auth creates no new session and reports the existing
      account instead. Observe no Graph or Azure CLI token is ever written to disk, logged in
      full, or written to `config.yaml`; tokens are minted in-process for the flow that needs
      them and held only for that call.
- [ ] AC-14.2 Given a blueprint tagged for this installation already exists (discovered via
      `GET /applications/microsoft.graph.agentIdentityBlueprint`), observe `ensure-blueprint`
      creates nothing and reports the existing blueprint's `appId`, never a duplicate
      blueprint. Given none exists, observe it creates the blueprint and blueprint principal
      exactly once and records the operator as owner. Given the operator lacks the Agent ID
      Developer or Administrator role, observe the command fails before issuing any write,
      with an error naming the specific missing role.
- [ ] AC-14.3 Given a role's agent identity and paired agent user already exist under the
      owned blueprint, observe `mandat provision --ensure-role <name>` for that role creates
      nothing and reports the existing identity and user ids. Given they do not exist, observe
      it creates both the agent identity and the paired agent user using the blueprint's own
      client-credential token, not a delegated operator token — the blueprint acts on its own
      consented application permissions (intrinsic `AgentIdentity.CreateAsManager` for the
      identity, `AgentIdUser.ReadWrite.IdentityParentedBy` for the user), so no operator
      standing privilege is required. Shipped and proven live 2026-07-17 against the dogfood
      tenant (`docs/research/init-provision-pilot.md`): the blueprint minted its own token and
      created a `ServiceIdentity` SP + a `#microsoft.graph.agentUser` parented to it, both
      independently verified via a delegated read. Existence is checked through the operator's
      delegated discovery (idempotency); only the writes go through the client-credential
      token. `AgentIdentity.CreateAsManager` cannot be explicitly granted to a blueprint
      principal (it is intrinsic), correcting the earlier premise that it needed consent.
- [ ] AC-14.4 Given the operator's session lacks admin-consent rights for the ADO
      `oauth2PermissionGrant` step, or lacks ADO org-admin rights for the entitlement/group
      step, observe `ensure-role-identity` does not fail opaquely: it prints the exact call
      (method, endpoint, request body) an admin must run to complete that step, and continues
      the remaining steps of the ladder rather than aborting.
- [ ] AC-14.5 Given the ADO entitlement call immediately follows agent-user creation and
      returns a transient failure, observe `ensure-role-identity` retries with backoff before
      surfacing failure: the propagation-lag gap the research doc documents from the dogfood
      run.
- [ ] AC-14.6 Given `mandat provision --dry-run` (or the equivalent flag on any `ensure-*`
      subcommand), observe it prints the full plan of every read and write call the ladder
      would issue against current tenant state, and issues zero writes.
- [ ] AC-14.7 Given any `ensure-*` step is about to issue a Graph or ADO write call, observe
      the exact call (endpoint and body, secrets redacted) is printed before it is issued. This
      extends US-0013 AC-13.12's diff-before-write stance, applied there to `config.yaml`,
      to tenant mutations, which carry higher stakes than a file write.
- [ ] AC-14.8 Given a completed `provision` run, observe no Graph token, Azure CLI token, or
      blueprint client secret is written to `config.yaml`, any file under `/etc/mandat/`, or
      any other disk location; only non-secret identifiers (tenant id, blueprint `appId`,
      agent identity/user ids and UPNs) land in the registry the interview reads.
- [ ] AC-14.9 Given `ensure-blueprint` and `ensure-role-identity` have run for at least one
      role, observe US-0013's `init` interview can populate role identity ids/UPNs via a
      picker over this registry (US-0013 AC-13.1 discovery, AC-13.3 irreducible per-role
      fields) instead of free-text entry, with typing as fallback only when no picker option
      applies.

## Remit

File-disjoint allowed paths:

- `cmd/mandat/provision.go`
- `cmd/mandat/provision_test.go`
- `cmd/mandat/main.go` (register the `provision` subcommand)
- `internal/entra/**` (new package: `ensure-auth`, `ensure-blueprint`, `ensure-role-identity`)
- `GETTING-STARTED.md` (§2 rewritten from the current `/beta` sketch to `mandat provision`,
  per the research doc's note that §2 still shows beta endpoints)

## Dependencies

Consumed by US-0013: its `init` interview reads the registry this story produces (AC-14.9).
Not blocking; US-0013's phase-1 scope ships independently of this story. Expected to reuse
the blueprint client-credential minting machinery the identity broker (US-0008, ADR-0005)
already implements for the agent-user token path. The exact package boundary between
`internal/entra` and `internal/identity` is an implementation decision, not fixed by this
story.

## Gaps

- The least-privilege `az` scope question is half-resolved, not fully open:
  `docs/research/init-provision-pilot.md` finding F1 pilots the owner agent-identity
  create/delete path live — as the blueprint owner, an ordinary az-minted Graph token (no
  `AgentIdentity*`-named scope) authorized `POST
  .../servicePrincipals/microsoft.graph.agentIdentity` (201) and `DELETE
  .../servicePrincipals/{id}` (204). `internal/entra.CreateAgentIdentity`/
  `EnsureAgentIdentity`, wired through `cmd/mandat/provision.go`'s `--ensure-identity`,
  already implement this path and ship on `main`. Still genuinely open, because the pilot
  ran against an already-owned blueprint and never exercised it: the one-time
  `AgentIdentityBlueprint.Create` scope for `EnsureBlueprint`'s blueprint-creation write,
  which the research doc still ties to the privileged Agent ID Developer/Administrator
  role. The remaining spike narrows from "the whole ladder's least-priv scope" to just the
  blueprint-creation step.
- UPN domain selection for the agent user is now resolved by an explicit required
  `--upn-domain` flag on `--ensure-role`: the operator supplies the verified tenant domain
  (e.g. `contoso.onmicrosoft.com`) and the user's userPrincipalName is `<role>@<domain>`.
  Auto-deriving the domain from the tenant's default verified domain remains a follow-up, not
  a blocker — an explicit flag is the honest MVP.
- Still open for a complete ladder (not covered by the `--ensure-role` slice): AC-14.1
  ensure-auth (`az login` drive) and AC-14.4's ADO steps 6–7 (the `oauth2PermissionGrant`
  admin consent and the ADO entitlement/group add). `--ensure-role` covers the Graph identity
  + user creation only; the ADO grant/entitlement steps are not yet wired.
