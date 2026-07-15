# S1: Agent identity in Azure DevOps

Spike from design §11. Kill risk if this fails: falls back to a standard Entra service
principal per role. Date: 2026-07-15. Status: answered, blocked (fallback active).

All identifiers below are placeholders. Real tenant ID, org name, project name, and
object IDs are operator-local (see the operator's private notes or memory, never
committed).

## Question

Does the Azure DevOps Users hub accept Entra agent-identity service principals (the new
`ServiceIdentity` subtype), and can an agent user be assigned work items?

## Method

Sequential Microsoft Graph and Azure DevOps REST calls from a WSL shell, authenticated as
a tenant admin user through cached Azure CLI credentials. Every step was recorded with its
HTTP status code. As a control, the operator ran a plain (non-agent) service principal
through the identical org-entitlement call, isolating the agent-identity subtype as the
only variable under test.

## Evidence

1. **Org-to-tenant binding confirmed.** The `x-vss-resourcetenant` response header,
   present on any Azure DevOps org API call, equals the personal tenant ID. Azure DevOps
   documents this as a hard constraint: identities can be added to an organization only
   from the tenant it is connected to ([Use service principals and managed identities in
   Azure DevOps, "Tenant restrictions"](https://learn.microsoft.com/azure/devops/integrate/get-started/authentication/service-principal-managed-identity?view=azure-devops#implementation-guide)).

2. **Agent identity blueprint created.**
   `POST /v1.0/applications/microsoft.graph.agentIdentityBlueprint`
   (`OData-Version: 4.0`, sponsor and owner bound to the operator) returned `201`.
   Notable: the az CLI Graph token carried no `AgentIdentityBlueprint.*` scope; Global
   Administrator plus `Application.ReadWrite.All` was sufficient anyway. Quirk: the
   returned application object ID equals its `appId` (`<blueprint-app-id>` denotes both
   below).

3. **Blueprint principal created.**
   `POST /v1.0/serviceprincipals/microsoft.graph.agentIdentityBlueprintPrincipal`
   returned `201`.

4. **Short-lived client secret added.**
   `POST /v1.0/applications/<blueprint-app-id>/addPassword` returned `200`. 7-day
   expiry, value captured to local scratch only, never logged or committed.

5. **Agent identity created, as the blueprint.** The operator acquired a client-credentials
   token for `<blueprint-app-id>`, then
   `POST /beta/serviceprincipals/Microsoft.Graph.AgentIdentity`
   `{ displayName, agentIdentityBlueprintId, sponsors@odata.bind }` returned `201` with
   `servicePrincipalType: ServiceIdentity`.

6. **The crux: add the agent identity to the Azure DevOps org.**
   `POST https://vsaex.dev.azure.com/<ado-org>/_apis/serviceprincipalentitlements?api-version=7.1-preview.1`
   `{ accessLevel: express, servicePrincipal: { origin: "aad", originId: "<agent-identity-id>" } }`
   returned HTTP `200`, but `operationResult.isSuccess = false`, error key `5000`:
   `"VS403283: Could not add user ... at this time."` Reproduced twice, about 4 minutes
   apart, same error both times.

7. **Control: a plain service principal.** A plain app registration and its service
   principal, created seconds before this entitlement call, went through the identical
   endpoint and body shape and were added on the first attempt (`isSuccess = true`).
   The control rules out permission gaps, license-slot exhaustion, a wrong API shape, and
   propagation delay as explanations, since all four would have hit the control too.

## Verdict

Azure DevOps does not accept the agent-identity (`ServiceIdentity`) subtype in the Users
hub as of 2026-07-15. No Microsoft documentation claims support for it: the GA'd
service-principal path ([Managed identity and service principal support for Azure DevOps,
GA in 2023](https://learn.microsoft.com/azure/devops/release-notes/2023/sprint-228-update#general))
covers only regular service principals and managed identities, and Microsoft's own
agent-identity documentation ([Agent identity blueprints in Microsoft Entra Agent
ID](https://learn.microsoft.com/entra/agent-id/agent-blueprint)) describes creation and
governance inside Entra with no mention of Azure DevOps as a consuming surface. A
Microsoft Q&A answer separately states that Azure DevOps has only limited native support
for Entra workload identities in service-to-service scenarios, consistent with what step 6
shows. The work-item-assignment half of S1 initially looked unreachable behind the same
wall; the round-2 evidence below answered it the same day through the paired agent user.

Post-spike inspection pins where the rejection lives. The blueprint is an application
subclass (`@odata.type: #microsoft.graph.agentIdentityBlueprint`; credential APIs behave
like any app registration, with the quirk that its object id equals its appId) and the
blueprint's principal is a plain `servicePrincipalType: Application`. Only the agent
identity carries the new `ServiceIdentity` principal type, and that type alone is what the
entitlement call rejects: everything typed `Application` enters the org, `ServiceIdentity`
does not. The blueprint principal itself would likely pass the Users hub, but as a single
shared principal for every role it would defeat per-role separation, so it is not a usable
path.

## Round 2 evidence: the paired agent user (same day)

The owner's retest through the web UI surfaced a second failure signature for the agent
identity: `VS860016: Could not find subject ... in the backing domain` from the identity
picker, while the entitlement API still returned VS403283 and Graph confirmed the
principal alive and enabled. Two surfaces, one root cause: the org's identity-resolution
layer does not see `ServiceIdentity` principals at all.

That reopened the assignment half through the object the design already planned for
(§4.1): the paired agent user.

8. Agent user created: `POST /v1.0/users/microsoft.graph.agentUser` (a GA v1.0 endpoint)
   with `identityParentId` bound to the spike agent identity and a UPN on the tenant's
   default verified domain → HTTP 201, `@odata.type #microsoft.graph.agentUser`. The
   operator token again lacked the dedicated scope (`AgentIdUser.ReadWrite.All`); Global
   Administrator plus `User.ReadWrite.All` sufficed.
9. Org entitlement via the UserEntitlements API succeeded → `isSuccess: true`, Basic
   license, AAD descriptor issued. Gotcha: identifying the user by `originId` fails with
   "The Id, OriginId, or User.PrincipalName must be set"; identify by `principalName`.
10. A test work item created with `System.AssignedTo` set to the agent user's UPN →
    HTTP 200, the assignment sticks and renders as a normal assignee.

The verdict therefore splits cleanly. The service-principal half stands: `ServiceIdentity`
principals cannot enter the org, so an agent cannot yet authenticate to Azure DevOps APIs
under its agent identity. The assignment half flips to yes: a paired agent user is a
first-class org member and a valid work-item assignee today. The remaining unknown is the
token flow — tokens minted through the blueprint identify the agent identity, which the
org cannot resolve — so whether any token path exists that lets the agent act as its agent
user is the follow-up question, and S3 will answer the interim path with the plain-SP
asset.

## Fallback

Per design §11, now active for API authentication: standard Entra service principal per
role. The architecture is unchanged; sponsor accountability and agent-native audit for
tracker writes are deferred until Azure DevOps accepts the subtype. Assignment and
attribution, however, need not wait: the paired agent user already works, and whether the
MVP provisions agent-identity/agent-user pairs alongside the per-role service principals
is an owner decision at ADR-0005 ratification. Recorded as
[ADR-0005](../adr/ADR-0005-identity-mode-service-principal.md) (proposed).

## Assets kept

All objects are prefixed `mandat-spike-`.

- The blueprint and the agent identity created in steps 2 to 5, kept in the tenant as the
  retest asset for step 6.
- The plain service principal from the control (step 7), already entitled in the org.
  It becomes the S3 asset: git over HTTPS authenticated with an Entra token.
- The paired agent user (steps 8 to 10), entitled in the org on a Basic license, and the
  test work item assigned to it, kept as living evidence and as the attribution asset for
  later spikes.

## Retest criteria

A July 2026 research sweep found no roadmap item, no preview flag, no community thread,
and no Microsoft-authored mention of Azure DevOps as an Agent ID consuming surface; both
error codes are absent from every troubleshooting table (sibling codes are documented, so
the absence is signal). Quarterly re-check of three deterministic doc surfaces, retest
immediately when any moves:

1. The Azure DevOps roadmap initiative "Minimizing the risks associated with credential
   theft" (the stated home for deeper Entra integration) gaining an agent-identity item.
2. The service-principals-and-managed-identities doc adding a `ServiceIdentity` row to its
   supported types.
3. The Entra Agent ID "What's new" page naming Azure DevOps in its integration list.

Retest procedure: re-run step 6 verbatim against the kept agent identity asset and record
the result the same way. The agent-user path needs no retest; it works today. The open
token-flow question (agent acting as its agent user) rides along with S3.
