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
shows. The work-item-assignment half of S1 is therefore unreachable and unanswered until
the subtype is accepted.

Post-spike inspection pins where the rejection lives. The blueprint is an application
subclass (`@odata.type: #microsoft.graph.agentIdentityBlueprint`; credential APIs behave
like any app registration, with the quirk that its object id equals its appId) and the
blueprint's principal is a plain `servicePrincipalType: Application`. Only the agent
identity carries the new `ServiceIdentity` principal type, and that type alone is what the
entitlement call rejects: everything typed `Application` enters the org, `ServiceIdentity`
does not. The blueprint principal itself would likely pass the Users hub, but as a single
shared principal for every role it would defeat per-role separation, so it is not a usable
path.

## Fallback

Per design §11, now active: standard Entra service principal per role. The architecture is
unchanged; sponsor and agent-native audit trails are deferred until Azure DevOps accepts
the subtype. Recorded as [ADR-0005](../adr/ADR-0005-identity-mode-service-principal.md)
(proposed).

## Assets kept

All objects are prefixed `mandat-spike-`.

- The blueprint and the agent identity created in steps 2 to 5, kept in the tenant as the
  retest asset for step 6.
- The plain service principal from the control (step 7), already entitled in the org.
  It becomes the S3 asset: git over HTTPS authenticated with an Entra token.

## Retest criteria

Retest monthly, or immediately when Azure DevOps release notes mention agent identity or
Agent ID support. Retest procedure: re-run step 6 verbatim against the kept agent identity
asset and record the result the same way.
