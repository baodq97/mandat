---
id: ADR-0005
title: Identity mode — standard service principal per role until ADO accepts agent identities
status: proposed
owner: TBD
date: 2026-07-15
---

# ADR-0005: Identity mode — standard service principal per role until ADO accepts agent identities

## Context

Spike S1 (§11) asked whether the Azure DevOps Users hub accepts the Entra agent-identity service-principal subtype, the instrument D5 and the identity plane (§4.1) build the whole mandate model on. Its kill risk was stated in advance: fall back to a standard service principal per role, keep the architecture, lose sponsor and agent-native audit. The S1 run on 2026-07-15 forced exactly that fallback. This ADR records it before MVP code commits to a path.

The run proved the Entra half works end to end in the dogfood tenant: blueprint created (HTTP 201), blueprint service principal created (201), blueprint client-secret credential issued (200), and one agent identity provisioned (201) reporting `servicePrincipalType: ServiceIdentity`. The break is on the tracker side. Adding that agent identity to the ADO organization's Users hub fails deterministically with error VS403283, reproduced twice about four minutes apart, while a plain service-principal control added to the same hub instantly. No Microsoft documentation claims the agent-identity subtype is entitled to ADO today. So the blueprint chain is sound and the tracker does not yet accept the subtype. The second S1 question, whether an agent user can be assigned work items, is unanswerable until the subtype lands, because there is no agent user in ADO to assign to.

## Decision

- **MVP identity mode is one standard Entra service principal per role**, client-credential, in the tenant the ADO organization is connected to. Certificate credential is preferred over client secret for anything past the spikes, matching §7, which already root-owns a blueprint certificate under `/etc/mandat/`. This is the §11 S1 fallback activating, not a redesign. Everything the mandate model rests on holds unchanged: remit enforcement stays mechanical (sparse checkout, per-role OS user, server policy, never by prompt); writer ≠ scorer stays an IAM property because distinct principals open and probe a PR, backed by the ADO "creator cannot approve own PR" branch policy (§4.1); tokens expire hourly and the token itself is the git credential over HTTPS, so no secret reaches disk (§4.1, §7); the no-PAT rule (§7) stands.
- **The agent-identity blueprint architecture (D5, §4.1) stays the target state.** The spike blueprint and one agent identity remain provisioned in the tenant as the retest asset, not torn down. Migration criterion is one falsifiable event: the S1 entitlement call, adding the agent identity to the ADO Users hub, returns success instead of VS403283. Retest cadence is monthly, or on any ADO release note mentioning agent identities.
- **Config-not-code.** Identity mode is a per-installation config value, `identity_mode: agent-identity | service-principal`, written by `mandat init` into `/etc/mandat/config.yaml` (§4.10). The eventual migration is therefore a config flip plus provisioning of the per-role agent identities, never a code change to any plane. The planes consume the configured principals; they do not encode the subtype.

### What this defers, stated plainly

- **Sponsor-based accountability and agent-native audit in Entra**, the agent identities and their paired agent users (§4.1). This is the exact loss S1's kill risk named: a standard service principal carries no sponsor field and no agent-user projection.
- **Work-item assignment to agent users**, the second half of S1. Unanswerable until the subtype is entitled to ADO, because the agent user cannot be created in the tracker.
- **Interim tracker-side attribution** is per-role service-principal display names in ADO history, plus the journal (§4.9) keyed by acting identity. Attribution stays real and per-role; only the Entra-native sponsor and agent-user audit surface waits.

## Consequences

- MVP is unblocked without waiting on Microsoft. The pipeline runs end to end under per-role principals from the first dispatched run, and the §10 MVP was already scoped to client-credential identity mode, so this fixes the credential subtype without moving the slice boundary.
- The security posture is materially unchanged: hourly tokens, no PAT, no on-disk secret in certificate mode, remit and writer ≠ scorer intact. The one honest downgrade is the audit surface: sponsor linkage and agent-user attribution drop from an IAM property to display-name convention plus the journal until migration.
- A live retest obligation now exists, monthly or on ADO release notes, with a one-line pass condition. The blueprint and agent identity keep a small standing footprint in the tenant as the retest asset. Accepted: it is the cost of a config-flip migration later instead of a re-provisioning project.
- `mandat doctor`'s S1 check (§4.10) asserts the service-principal path in this mode and flips to asserting the entitlement call only after migration. Doctor stays the deployed proof of whichever mode is configured.
- PRD-0001's Reviewer-identity provisioning and the agent-identity-preview prerequisite still hold: the Reviewer principal is provisioned as a standard service principal in this mode, and writer ≠ scorer survives the substitution.

## Alternatives considered

- **Wait for ADO to accept agent identities before shipping MVP.** Blocks the product indefinitely on a Microsoft roadmap with no committed date; VS403283 is a present, reproduced fact, not a transient. Rejected; the §11 kill risk exists precisely to remove this dependency.
- **Personal access tokens.** The product's anti-thesis (§7, PRD-0001 Problem): a standing, broadly scoped, unattributable credential is the exact failure Mandat exists to kill. Rejected outright.
- **Impersonate regular user accounts for the agents.** Restores an assignee picker and a "person" in ADO, but breaks the mandate model (no sponsor, no revocable scoped grant, a human license standing in for an agent) and is hygiene the product will not adopt near the ADO terms-of-service line. Rejected.
- **Client secret over certificate for the per-role principals.** Simpler to issue, but leaves a long-lived secret on disk where a certificate need not, against §7's on-disk-secret stance. Rejected for anything beyond spikes; certificate preferred.
