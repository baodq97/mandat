# S3: Entra token as git HTTPS credential, and PR creation, against Azure DevOps

Spike from design §11. Charter: an Entra access token acts as the git HTTPS credential
against Azure DevOps repos from a headless CLI. Kill risk if this fails: fall back to
short-lived ADO-scoped tokens fetched per push. Date: 2026-07-16. Status: answered, yes.
This also closes the honest remainder S1 round 3 left open: git-over-HTTPS carrying the
delegated agent-user token, and PR creation under the agent user.

All identifiers below are placeholders. Real tenant ID, org name, project name, object IDs,
and UPNs are operator-local (operator's private notes or memory, never committed).

## Question

Can the paired agent user's delegated Entra token (S1 round 3's `T3`) clone, push, and open
a pull request against an Azure DevOps repo from a clean headless environment, carrying no
personal access token and no standing secret, with the writes attributed to the agent user?

## Method

A clean Docker container (`ubuntu:24.04` plus `git`, `curl`, `jq`, `python3` only) modelled
the Mandat runner sandbox. The three-token chain was minted on the host, so the blueprint
client secret stayed host-side in scratch; only `T3`, the agent user's delegated token
(ADO audience, roughly one-hour TTL, revocable), entered the container as a read-only
mounted file. The container held no PAT, no `az`, and no credential-holder secret, so the
token itself was the sole git credential. As setup, a tenant-admin call created a spike repo
and seeded its `main` branch. Every step was recorded with its HTTP status or git result,
and each write was re-verified from the host against the admin view.

## Evidence

1. **Sandbox holds one credential.** Inside the container the only secret material was
   `T3`. Identity, tooling, and the git credential were all the delegated token; there was
   nothing else to leak or revoke.

2. **Clone over HTTPS with the token as credential.** `git -c
   http.extraheader="Authorization: Bearer <T3>" clone https://dev.azure.com/<ado-org>/<project>/_git/<repo>`
   fetched the seeded `main` (`HEAD 0a56620`). The bearer header carried the whole
   authentication; the URL's username component was ignored.

3. **Commit authored as the agent user.** With `user.email` and author/committer set to the
   agent user's UPN, a new file committed on a feature branch (`92077cf`).

4. **Push the feature branch.** `git push -u origin mandat/s3-<run>` returned `[new branch]`
   and created the ref on the origin. The push carried the same bearer header.

5. **Create the pull request.** `POST
   https://dev.azure.com/<ado-org>/<project>/_apis/git/repositories/<repo-id>/pullrequests?api-version=7.1`
   under `T3` returned HTTP `201`, `pullRequestId 1`, `status active`, source the feature
   branch and target `main`, `createdBy` equal to the agent user's UPN.

6. **Independent host verification.** Read back through the tenant-admin token, PR `1`
   reports `createdBy` = the agent user, and the pushed commit `92077cf` carries the agent
   user as both `author` and `committer`. The write attribution is a directory fact, not a
   display-name convention.

7. **Kill-criterion probe on the write credential.** Disabling the agent user
   (`PATCH /v1.0/users/<agent-user-object-id>` `{ accountEnabled: false }` → HTTP `204`)
   made a fresh chain refuse `T3` on the first attempt with `AADSTS50057: The user account
   is disabled.` Re-enabling (HTTP `204`) restored minting on the first attempt. So the
   sandbox loses the ability to acquire a git credential the moment the identity is
   disabled. The honest nuance: a bearer `T3` already issued stays valid until its TTL
   expires; revocation blocks new credentials immediately and propagates to existing ones
   within the token lifetime.

## Verdict

Yes on both counts. An Entra delegated token is a working git HTTPS credential against Azure
DevOps, and PR creation under the agent user works, both from a headless sandbox that holds
no PAT and no standing secret. The `git push` and PR writes are attributed to the agent
user as a directory fact. This closes the last write-path feasibility gap and the honest
remainder from S1 round 3: read, comment, clone, push, and PR-create all now run under the
revocable, sponsor-linked agent-user token. The §11 fallback (short-lived ADO-scoped tokens
fetched per push) is not needed for Azure DevOps; it stays the portable floor for surfaces
where this chain is not proven.

## What this settles for the product

- **The runner sandbox model is faithful.** A per-task container receiving only a scoped,
  hourly, revocable token, with the token as the git credential and no secret on disk,
  reaches every git and PR surface the pipeline needs. This is the §4.1 and §7 invariant
  demonstrated end to end, not asserted.
- **Attribution survives the write path.** `identity_mode: agent-user-pair` (ADR-0005) now
  carries clone, push, and PR-create with the sponsor link intact, so the agent-user pair is
  the recommended write path on Azure DevOps, not merely an attribution add-on for reads.

## Honest remainder and caveats

- **Revocation is TTL-bounded, not instantaneous.** New-credential denial is immediate; an
  already-minted `T3` works until it expires (about one hour). Shortening effective exposure
  means shorter token lifetimes, not a claim of instant cutoff.
- **Writer ≠ scorer, proven as a follow-on.** S3 proves the creator can open a PR; a
  follow-on probe on the same repo set the "creator cannot approve own PR" branch policy
  (minimum one approver, the creator's vote does not count) and confirmed the gate. The
  agent user cast an approve vote on its own PR, yet completion was refused with HTTP `403`,
  "needs a minimum number of approvals (1) from other users", until a different user
  approves. So reviewer ≠ author holds as an IAM property, not convention.
- **Untested, as in S1 round 3:** conditional-access policies evaluated against the agent
  user, the per-seat Basic license cost per assignable role, and the terms-of-service line
  on a non-human directory seat. Each is a check to run, not a known blocker.

## Assets kept

All objects are prefixed `mandat-spike-` or named for the spike.

- The spike repo `mandat-s3` in the pilot project, seeded `main` plus the feature branch and
  PR `1`, kept as living evidence of the write path.
- The S1 assets reused unchanged: the agent user on its Basic license, the
  oauth2PermissionGrant delegating it to the agent identity on the ADO resource, and its
  project Contributors membership (which also grants repo Contribute, so push and PR-create
  needed no extra permission).

## Relation to other records

Closes the git-over-HTTPS and PR-creation remainder named in
[S1 round 3](./S1-agent-identity-ado.md) and in
[ADR-0005](../adr/ADR-0005-identity-mode-service-principal.md) (proposed). ADR-0005's
"still open, the git credential path" is now answered; its ratification, gated on an
independent red-team pass, is the next governance step.
