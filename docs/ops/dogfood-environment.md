# Dogfood environment

Operations runbook for the environment the S1 to S5 spikes and the dogfood deployment run
against (design PRD-0001, spec §11). Written for onboarding: a new operator or agent
should be able to reproduce the whole setup from this file plus the operator-local values
below.

All identifiers below are placeholders. Real tenant ID, org name, project name, and
object IDs are operator-local (see the operator's private notes or memory, never
committed).

## Topology

- One personal Microsoft Entra tenant.
- One personal Azure DevOps org, connected to that tenant. Azure DevOps enforces this as a
  hard constraint: it only lets you add identities from the tenant an org is connected to
  ([Use service principals and managed identities in Azure DevOps, "Tenant
  restrictions"](https://learn.microsoft.com/azure/devops/integrate/get-started/authentication/service-principal-managed-identity?view=azure-devops#implementation-guide)).
  Check the binding two ways: the `x-vss-resourcetenant` response header on any org API
  call, or `https://dev.azure.com/<ado-org>/_settings/organizationAad` in a browser.
- The WSL workstation is the operator seat, where az CLI, Graph calls, and ADO REST calls
  run today.
- A future Arc-enrolled Linux VM, not yet provisioned, is the target for the runner spikes
  (S2, S4).

## Identity model and its caveat

The operator's cached Azure CLI credential is a guest in the personal tenant, holding
Global Administrator there. The admin chain for this whole environment runs through an
external home account rather than a native member account. This is fragile and is a
tracked risk: a policy change on the home tenant, a suspended home account, or a revoked
guest invitation each breaks the admin chain.

Never touch the company or default tenant with any command run for this project. Always
pass `--tenant <personal-tenant-id>` explicitly on every `az` call, and do not rely on the
CLI's default tenant, since it can silently point at a different directory depending on
prior `az login` state.

## Token acquisition patterns

Azure DevOps, delegated (the signed-in operator):

```bash
az account get-access-token \
  --tenant <personal-tenant-id> \
  --resource 499b84ac-1321-427f-aa17-267ca6975798   # the Azure DevOps first-party app id
```

Microsoft Graph, delegated:

```bash
az account get-access-token \
  --tenant <personal-tenant-id> \
  --resource-type ms-graph
```

As the blueprint, client credentials (unattended, used to mint agent-identity tokens):

```
POST https://login.microsoftonline.com/<personal-tenant-id>/oauth2/v2.0/token
Content-Type: application/x-www-form-urlencoded

client_id=<blueprint-app-id>
&client_secret=<operator-local, never in this file>
&scope=https://graph.microsoft.com/.default
&grant_type=client_credentials
```

## Verification probes

Standing health checks. Run top to bottom on any new session or after a suspected drift.

| Probe | Expected result |
|---|---|
| Org tenant binding: any authenticated org API call | `x-vss-resourcetenant` header equals `<personal-tenant-id>` |
| ADO project list: `GET https://dev.azure.com/<ado-org>/_apis/projects?api-version=7.1` | `200` |
| Graph `/me`: `GET https://graph.microsoft.com/v1.0/me` | `200` |
| Graph `/organization`: `GET https://graph.microsoft.com/v1.0/organization` | `200` |
| `serviceprincipalentitlements` reachability: `GET https://vsaex.dev.azure.com/<ado-org>/_apis/serviceprincipalentitlements?api-version=7.1-preview.1` | `405` (method not allowed on GET is the healthy signal here; it means the endpoint exists and auth cleared. A `401`/`403` means auth is broken, not the endpoint) |

## Secret handling rules

- Mint secrets with short expiry, days rather than months.
- Capture them to local scratch only; keep them out of terminal output, where a transcript
  or screen share could leak them.
- Keep them out of commits, docs, and commit messages entirely.
- Rotate or delete each secret as soon as the spike that needed it is done.

## Naming and cleanup

Every object created for a spike is prefixed `mandat-spike-`, so cleanup is a name filter,
not a memory exercise.

Cleanup commands:

```
DELETE https://graph.microsoft.com/beta/serviceprincipals/<agent-identity-id>   # agent identity
DELETE https://graph.microsoft.com/v1.0/applications/<object-id>                # blueprint or plain app registration
DELETE https://vsaex.dev.azure.com/<ado-org>/_apis/serviceprincipalentitlements/<entitlement-id>   # org entitlement
```

## Where values live

Real IDs and names are operator-local: they live in the operator's private notes or
memory, never in this repo. This file carries only shapes and placeholders, so it stays
safe to keep in a public repo.

Onboarding sequence for a new operator or agent: obtain the operator-local values from the
owner, substitute them for the placeholders above, then run the verification probes top to
bottom before starting any spike work.
