# Getting started with mandat

This is the end-to-end setup for one pilot: a Dev agent that drafts PRs and a Reviewer
agent that verifies them. The Entra steps are manual today and need a Global Administrator.
Work through the steps in order. Every identifier below is a placeholder. Replace the
`<...>` values and the `contoso` org with your own.

## 0. What you need

- A Linux VM (amd64 or arm64) with `git` and the Claude Code CLI `>= 2.1.208` installed,
  plus whatever toolchains your target repo's gates need. If a gate runs `make check` for
  a Go repo, the VM needs Go.
- A Microsoft Entra tenant where you can create agent identity blueprints. The one-time
  identity setup needs Global Administrator.
- An Azure DevOps organization connected to that tenant.
- A Claude subscription token for the runner (`claude setup-token`).

## 1. Install the binary

```sh
curl -fsSL https://raw.githubusercontent.com/baodq97/mandat/main/install.sh | sh
mandat version
```

`version` prints the installed build. If it errors, the install did not complete.

## 2. Provision the Entra identities

Create two principals: a **Dev** agent that writes code and opens PRs, and a **Reviewer**
agent that verifies. They must be different principals. mandat's `doctor` enforces the
distinctness and fails if they share an agent user name.

The Graph agent identity APIs are preview surface. Treat these as sketches and check the
current Microsoft Learn shape before you run them.

**(a) Create the blueprint and its credential.** One agent identity blueprint per
installation. In production use an Arc managed identity as the blueprint credential; for a
pilot use a client secret or certificate.

```
POST https://graph.microsoft.com/beta/agentIdentityBlueprints
{ "displayName": "mandat-blueprint" }
```

Record the blueprint app id as `<blueprint-app-id>`.

**(b) Create an agent identity** sponsored by a named human.

```
POST https://graph.microsoft.com/beta/serviceprincipals/Microsoft.Graph.AgentIdentity
{
  "displayName": "mandat-dev",
  "agentIdentityBlueprintId": "<blueprint-app-id>",
  "sponsors@odata.bind": ["https://graph.microsoft.com/v1.0/users/<sponsor-object-id>"]
}
```

Record the identity object id as `<dev-identity-object-id>`.

**(c) Create the paired agent user** so the tracker can treat the agent as an assignee.

```
POST https://graph.microsoft.com/v1.0/users/microsoft.graph.agentUser
{
  "accountEnabled": true,
  "displayName": "mandat dev",
  "mailNickname": "mandat-dev",
  "userPrincipalName": "mandat-dev@contoso.onmicrosoft.com",
  "identityParentId": "<dev-identity-object-id>"
}
```

Record the agent user object id as `<dev-agent-user-object-id>` and its UPN as
`<dev-agent-user-upn>`.

**(d) Grant delegated access to Azure DevOps.** Without this grant, token minting fails
with `AADSTS65001`.

```
POST https://graph.microsoft.com/v1.0/oauth2PermissionGrants
{
  "clientId": "<dev-identity-object-id>",
  "consentType": "AllPrincipals",
  "resourceId": "<ado-service-principal-object-id>",
  "scope": "user_impersonation"
}
```

**(e) Repeat (b) through (d) for the Reviewer** with its own display name, UPN
`<reviewer-agent-user-upn>`, and object ids.

Entitlement calls right after user creation can hit propagation lag. Retry on failure.

## 3. Wire Azure DevOps

Add both agent users to the org, then scope their permissions.

- Add each agent user via user entitlements. Use `accountLicenseType` `express` (Basic)
  and pass `principalName` for a freshly created user.
- Add the Dev agent to the project **Contributors** group so it can push and open PRs.
- Add the Reviewer agent to the project **Readers** group. The Reviewer only reads and
  probes.
- Note the org and project names for config. This walkthrough uses org `contoso` and
  project `mandat-pilot`.

## 4. Prepare the target repo and board conventions

- The target repo lives in the same ADO project.
- Work items are **Issues** (Basic process), **assigned** to the Dev agent user's UPN.
- Tag each item `repo:<repo-key>` so mandat knows which registry entry applies.
- Put the acceptance criteria in the **Acceptance Criteria** field. The adapter lifts that
  text unparsed into the `TaskContract`.

mandat moves the item to the configured in-progress state (default `Doing`) on dispatch,
comments at dispatch, PR open, and hold, and links the PR. A human moves the item to Done.
mandat never writes a Done state.

## 5. Configure with `mandat init`

```sh
sudo mandat init
```

`init` writes `/etc/mandat/config.yaml`, root-owned. It discovers the ADO org, project,
and repo URL from your `az` login where reachable, and prompts for everything it cannot
discover: the Entra identity ids and UPNs from step 2, `auth.mode`, `entra.tenant` and
`entra.blueprint`, the repo's remit paths and gates, `autonomy_ceiling`, and
`budget.max_usd_per_run`. `paths` is the mechanical remit: a gate that needs a file needs
it listed, or the gate re-run fails in the sparse worktree.

`init` also writes both role playbooks from built-in templates and asks whether to
install a systemd user unit for always-on `serve` (default no; see step 7). Before
writing anything it prints a diff of the changes and asks you to confirm. Run it again
anytime: it offers each value already on disk as the prompt default, so pressing Enter
through every prompt leaves the file byte-identical.

For CI or a scripted install, pass `--non-interactive` and supply every value as a flag.

`init` does not set runtime secrets. Keep these outside the config file, and in the env
file the systemd unit sources (`/etc/mandat/mandat.env`) if you asked `init` to install
one:

- `MANDAT_CLIENT_SECRET_FILE` points at a `0600` file holding the blueprint client secret,
  or set `MANDAT_CLIENT_SECRET` inline. Production mode (Arc) needs neither.
- `CLAUDE_CODE_OAUTH_TOKEN` carries the Claude subscription token for the runner.
- `MANDAT_DIRECT_SPAWN=1` skips per-role OS users. Use it for pilots that have not
  provisioned separate OS users yet.

## 6. Preflight with doctor

`init` already ran these checks as its closing preflight. Re-run them anytime:

```sh
mandat doctor --config /etc/mandat/config.yaml
```

Fix every FAIL before the first run.

## 7. First run

Run one supervised cycle first:

```sh
mandat serve --once --config /etc/mandat/config.yaml
```

Drop a work item on the board (assigned to the Dev UPN, tagged `repo:app`, acceptance
criteria filled) and watch it move: `Doing` plus a dispatch comment, then a linked draft
PR, then verification, then `in-review`. A hold lands as `needs-human` with a reason
comment.

If you asked `init` to install the systemd unit, run the two enable commands it printed
for always-on `serve`. Otherwise `mandat serve --config /etc/mandat/config.yaml` runs in
the foreground.

## 8. Operations

- **Upgrade**: re-run the install one-liner. The script stages and renames the binary
  atomically, so it swaps under a running unit. Restart the unit afterward:
  `systemctl --user restart mandat.service`.
- **Budget**: tune `budget.max_usd_per_run` and `budget.max_usd_in_flight` in config.
- **Requeue a held task** (manual runbook today; a first-class command is on the backlog):
  1. Delete the task's row in the `tasks` table of the journal DB.
  2. Remove the task worktree directory under `/var/lib/mandat/tasks/`.
  3. Run `git worktree prune` in the repo mirror.
  4. Delete the stale task branch.

## 9. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `AADSTS65001` on token mint | The `oauth2PermissionGrant` from step 2(d) is missing. Grant `user_impersonation` on the ADO service principal to the agent identity. |
| Work items never picked up | Assignment must be the agent user's UPN, the item must carry the `repo:<key>` tag, and the Acceptance Criteria field must be non-empty. |
| `ETXTBSY` on upgrade | An old `install.sh` overwrote a running binary. Re-run the current one (staged rename) or stop the unit first. |
| Task held at `gate_red` but the gate passes locally | Usually a transient first-run tool bootstrap in a fresh worktree. Requeue the task (step 8). |
