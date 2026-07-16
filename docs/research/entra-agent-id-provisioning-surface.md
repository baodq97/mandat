# Entra Agent ID provisioning surface — verified against Graph v1.0, read for a `mandat provision` ladder

Verified 2026-07-16 on Microsoft Learn (MCP, official docs only). Motivation: the owner ruled
that init-adjacent flows should cover MS-CLI auth, blueprint registration when absent, and
agent-identity registration — the ceremony GETTING-STARTED §2 does by hand today.

## Headline: the deferral rationale in US-0013 is stale

US-0013's out-of-scope section says the Graph agent-identity surface "is beta" and "needs a
Global Administrator delegated token." Both claims are now wrong:

- Every resource and write endpoint below is documented at **graph-rest-1.0**, not beta.
- Blueprint creation needs the **Agent ID Developer** or **Agent ID Administrator** role —
  not Global Administrator. Identity creation under an owned blueprint needs **no Agent ID
  role at all**.

## The registry (read surface)

| Surface | Endpoint | Least-priv permission |
|---|---|---|
| List blueprints | `GET /applications/microsoft.graph.agentIdentityBlueprint` | `AgentIdentityBlueprint.Read.All` (or blueprint owner) |
| List agent identities under a blueprint | children of the blueprint (Entra PowerShell `Get-EntraAgentIdentity -AgentIdentityBlueprintId`) | `Application.Read.All` (or blueprint owner) |
| Agent user ↔ identity link | `agentUser.identityParentId` (1:1, enforced — duplicate link returns 400) | user read |

Implication for the US-0013 interview: role identity ids/UPNs are discoverable — a picker
over the blueprint's children replaces free-text GUID prompts, with typing only as fallback.

## The write surface (the ceremony, now GA)

| Step | Endpoint (v1.0) | Least-priv permission | Who can run it |
|---|---|---|---|
| 1. Create blueprint | `POST /applications/microsoft.graph.agentIdentityBlueprint` (requires `displayName` + `sponsors`) | delegated `AgentIdentityBlueprint.Create` | operator with Agent ID Developer/Administrator role; creator auto-becomes owner |
| 2. Create blueprint principal | `POST /servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal` (`{appId}`) | `AgentIdentityBlueprintPrincipal.Create` | same; creator auto-owner |
| 3. Blueprint credential | secret/cert on the blueprint | Agent ID **Administrator** for secret/cert; Developer may configure FIC | admin-adjacent; production mode is FIC/Arc anyway |
| 4. Create agent identity | `POST /servicePrincipals/microsoft.graph.agentIdentity` | delegated `AgentIdentity.Create.All` | **blueprint owner needs no Agent ID role** (documented) |
| 5. Create agent user | `POST /users/microsoft.graph.agentUser` (`accountEnabled, displayName, mailNickname, userPrincipalName, identityParentId`) | `AgentIdUser.ReadWrite.IdentityParentedBy` | **recommended client is the blueprint itself** (app permission on the blueprint SP, client-credential flow); delegated path needs Agent ID Administrator |
| 6. ADO delegated grant | `POST /oauth2PermissionGrants` (`user_impersonation` on the ADO SP) | admin consent (AllPrincipals) | tenant admin — stays privileged |
| 7. ADO entitlement + group | ADO REST userentitlements + Readers/Contributors | ADO PCA | ADO org admin |

## The ladder this supports (ensure-semantics, idempotent)

1. **ensure-auth** — the "MS CLI" flow: detect `az`; if absent or logged out, drive
   `az login --tenant <tenant>` (device code comes free); mint Graph tokens via
   `az account get-access-token`. Empirically proven in the dogfood: the whole reviewer
   principal ceremony ran on az-minted Graph tokens.
2. **ensure-blueprint** — list blueprints; if none (or none tagged for mandat), create
   blueprint + principal (steps 1–2), operator becomes owner, record `appId` for config.
   One-time per installation; needs the Agent ID Developer/Administrator role.
3. **ensure-role-identity** — per configured role: create agent identity under the blueprint
   (owner path, no extra role), then create the paired agent user **using the blueprint's own
   client-credential token** (step 5's recommended client — machinery mandat's broker already
   has), then the ADO grant + entitlement (steps 6–7, the two steps that stay privileged).
4. Interview/config flow (US-0013) then consumes the registry via pickers.

What stays manual/privileged after the ladder: granting the blueprint's app permission
(one admin-consent action), the `oauth2PermissionGrants` admin consent, ADO org admin for
entitlements. "Global Administrator required" collapses to: one Agent ID role + two consent
actions + ADO admin.

## Spike round 1 (2026-07-16, run on the dogfood tenant): the az scope question, answered halfway

Probe: decode the `scp` claim of an az-minted Graph token (Azure CLI first-party client,
appId `04b07795-8ddb-461a-bbee-02f9e1bf7b46`), then hit the v1.0 registry read endpoint.

- az carries **no Agent-ID-specific scope**. Its `scp`: `Application.ReadWrite.All,
  AppRoleAssignment.ReadWrite.All, AuditLog.Read.All, DelegatedPermissionGrant.ReadWrite.All,
  Directory.AccessAsUser.All, Group.ReadWrite.All, User.Read.All, User.ReadWrite.All` (+ OIDC).
- The dogfood ceremony therefore worked through `Directory.AccessAsUser.All` — effective
  rights equal the signed-in operator's directory roles. Empirical success is proven only
  for a privileged operator on the then-beta surface.
- `GET /v1.0/applications/microsoft.graph.agentIdentityBlueprint` under the az token returns
  200 and lists the dogfood blueprint — the v1.0 **read** surface works via az today.
- `DelegatedPermissionGrant.ReadWrite.All` is present — the ADO `oauth2PermissionGrants`
  step (6) is callable through az when the operator holds admin rights.

Still open, narrowed: whether the v1.0 Agent ID **write** endpoints authorize an operator
holding only the Agent ID Developer role through `Directory.AccessAsUser.All`, or demand the
named `AgentIdentity*` scopes az cannot request. Needs a minimal-role test user. US-0014's
ensure steps must therefore catch a 403 and print the Entra PowerShell alternative
(`Connect-Entra -Scopes` with the exact scope names) instead of failing opaquely.

## Open questions for the spike (pin before implementation)
- Whether agent-user UPN domain selection (verified domains of the tenant) needs a picker.
- Propagation lag handling (entitlement right after user creation failed once in the
  dogfood; retry policy belongs in the ensure step).
- GETTING-STARTED §2 still sketches `/beta` endpoints — update to v1.0 alongside this work.

## Sources

- https://learn.microsoft.com/graph/api/resources/agentid-platform-overview?view=graph-rest-1.0
- https://learn.microsoft.com/graph/api/agentidentityblueprint-list?view=graph-rest-1.0
- https://learn.microsoft.com/graph/api/agentidentityblueprint-post?view=graph-rest-1.0
- https://learn.microsoft.com/graph/api/agentidentityblueprintprincipal-post?view=graph-rest-1.0
- https://learn.microsoft.com/graph/api/agentidentity-post?view=graph-rest-1.0
- https://learn.microsoft.com/graph/api/agentuser-post?view=graph-rest-1.0
- https://learn.microsoft.com/entra/agent-id/create-blueprint
- https://learn.microsoft.com/entra/agent-id/agent-id-creation-channels
- https://learn.microsoft.com/entra/agent-id/autonomous-agent-authentication-authorization-flow
- https://learn.microsoft.com/powershell/module/microsoft.entra.applications/get-entraagentidentity
- https://learn.microsoft.com/graph/permissions-reference
