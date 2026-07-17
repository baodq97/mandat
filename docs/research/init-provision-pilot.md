# init/provision auto-derive pilot — live findings

Piloted 2026-07-17 against the dogfood Entra tenant + ADO org (placeholders below; real
identifiers are never committed). An uncommitted prototype ran `mandat init` and `mandat
provision` end to end on the live tenant to de-risk the automatic first-run flow before
governing it. This note records what the live run proved, for US-0016 (init auto-derive),
the US-0014 provision slice, and US-0015 (tenant pin).

## What was piloted

The az-cli-shaped first-run: auto-derive everything the machine can know, keep every value
overridable. `internal/entra` reads the Agent-ID registry over Graph v1.0; `mandat provision`
reports/creates identities; `mandat init` prefills tenant, base branch, and the role identity
fields from that registry.

## Findings

### F1. The least-privilege spike (US-0014 open gap) resolves positive

The research doc left open whether the v1.0 Agent-ID **write** endpoints authorize an operator
holding only a directory role through az's `Directory.AccessAsUser.All`, or demand the named
`AgentIdentity*` scopes az cannot request. The live run answers it: as the **blueprint owner**,
the operator's az-minted Graph token authorized `POST /servicePrincipals/microsoft.graph.agentIdentity`
(201 Created) and `DELETE /servicePrincipals/{id}` (204) with **no Agent-ID-specific scope**.
This matches the Graph doc for agentIdentity-post: *"Owners can create and modify agent
identities associated with a blueprint they own without being assigned an Agent ID role."*
A throwaway identity was created, its idempotent re-run reused it (no second write), and it
was deleted — the tenant was left unchanged.

Consequence: the owner path needs no privileged consent. The privileged steps that remain are
the ones the research doc already isolated (blueprint creation needs the Agent ID
Developer/Administrator role; the ADO `oauth2PermissionGrant` and entitlement stay admin).

### F2. `agentIdentity` create requires three body fields

`POST agentIdentity` with `{displayName}` alone returns `400 Request_BadRequest: "No sponsor
specified."` The documented body needs three fields: `displayName`, `agentIdentityBlueprintId`,
and `sponsors@odata.bind` (an array of `.../v1.0/users/{id}` references). The sponsor is the
named human the agent acts under — mandat's mandate concept, expressed in the Graph object.
The pilot defaults the sponsor to the signed-in operator's object id and lets `--sponsor`
override it. (`agentIdentityBlueprintId` accepted the blueprint's id; object-id and app-id
coincide for the dogfood blueprint, so which one the endpoint wants is not yet disambiguated.)

### F3. init auto-derive works live

Run interactively on the live tenant, `mandat init` presented as bracketed defaults, with no
free-text GUID entry:

- `entra.tenant` — derived from the az session (the tenant id claim), and used to pin the ADO
  discovery/validation token (`--tenant`), which is US-0015's fix.
- `repo base_branch` — derived from the ADO repository `defaultBranch` (stripped of
  `refs/heads/`). An empty repository returns `defaultBranch: null`; that case falls back to
  prompting rather than writing a wrong default.
- the six role-identity fields — derived from the Agent-ID registry: each role's agent identity
  matched by display name, paired to its agent user via `agentUser.identityParentId`, filling
  `agent_identity_id`, `agent_user_id`, and `agent_user_name`.

The written `config.yaml` loaded valid; the run's only non-zero exit came from the
environmental preflight (absent `/var/lib/mandat`, no runtime secret on the dev box), the same
tri-state `doctor` result US-0013 already produces off-VM.

### F4. az default tenant is not sticky

Across separate process invocations the az active tenant reverted to the workstation's default
(a different tenant than the dogfood one). Any command that mints a tenant-scoped token must
set/pin the tenant per invocation rather than trusting the ambient default — the same hazard
US-0015 fixes for the discovery token, generalized to every az call.

## Grounds

- Resolves the open least-privilege spike in `US-0014` (Gaps) for the owner create/delete path.
- Evidence for `US-0016` (init auto-derive from the az session + Agent-ID registry).
- Confirms `US-0015` (tenant pin) live: the derived tenant pins the discovery token.
- Primary Graph surface: `docs/research/entra-agent-id-provisioning-surface.md`.
