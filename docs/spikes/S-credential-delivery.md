# S-credential-delivery: invariant-preserving git credential delivery to the runner

Spike named by RFC-0001 (§Identity injection, §Open questions). Charter: deliver the
delegated agent-user token to git for an ADO clone and push without the token ever
reaching the per-role child process, never in the child's environment, never in argv,
never written to `.git/config`, never on disk. Kill risk if this fails: no mechanism
exists that both authenticates and preserves the invariant, which would force the runner's
push path onto the token-persisting `http.extraheader` path S3 used and breach the §4.1
identity invariant. Date: 2026-07-16. Status: answered, yes. This resolves RFC-0001's last
open one-way-door dependency and falsifies the red-team's F4 kill criterion.

All identifiers below are placeholders. Real tenant ID, org name, project name, object
IDs, and UPNs are operator-local (operator's private notes or memory, never committed).

## Question

Can a git credential helper deliver the delegated agent-user token (S1 round 3's `T3`,
S3's write credential) to git for an ADO clone and push while keeping the token out of the
per-role child's reach: never in the child environment, never in argv, never written to
`.git/config`, never on disk? RFC-0001 pinned this invariant (§Identity injection,
decision 8) but deferred the concrete mechanism to this spike. The red-team's F4 kill
criterion was that no mechanism might exist that both succeeds and preserves the invariant,
because git before 2.46 emits only Basic auth from a credential helper (not Bearer), and
the one write path S3 already proved (`http.extraheader` Bearer) writes the token into
`.git/config` and process argv where the role OS user can read it.

## Method

A credential-helper script modelling the `mandat git-credential` broker (spec §4.1: the
binary re-invoked as a git credential helper). The helper holds the token itself and
answers `username` and `password` on the git credential-helper `get` protocol over stdin;
the git process it serves never carries the token in its environment or argv. Run with
`git -c credential.helper=<broker>` against the kept spike repo on the deployed git
version, git 2.43, which sits below the 2.46 that first lets a helper return an
`authtype`/Bearer credential. After each operation the token's presence was checked in
`.git/config` and everywhere under `.git/`.

## Evidence

The Basic-password matrix cell clears on git 2.43, the current deployed version, below the
2.46 that helper Bearer would require.

1. **Clone under the broker.** `git -c credential.helper=<broker> clone
   https://dev.azure.com/<ado-org>/<project>/_git/<repo>` fetched the seeded `main` (HEAD
   retrieved). The helper answered the `get` protocol with the agent user's UPN as
   `username` and the delegated token as `password`; git sent `Authorization: Basic`, and
   ADO accepted the delegated agent-user token as a Basic-auth password.

2. **Commit under the agent user.** A new file committed on a feature branch with author
   and committer set to the agent user's UPN.

3. **Push under the broker.** `git push -u origin <branch>` returned `[new branch]` and
   created the ref on the origin. The push drove the same helper for its credential; no
   token sat in the command line or the environment.

4. **Invariant held.** After both clone and push, the token was absent from `.git/config`
   and absent everywhere under `.git/`. The helper supplied it transiently on stdin per
   operation and git never persisted it; nothing in the checkout carried the credential.

## Verdict

Yes. An invariant-preserving delivery mechanism exists and works on git 2.43 today. A
credential helper modelling `mandat git-credential` clones and pushes to ADO under the
delegated agent-user token, and the token never lands in the child's environment, argv,
`.git/config`, or anywhere on disk. The red-team's F4 kill criterion is falsified: the
invariant and a working write path are not mutually exclusive.

Two consequences for the matrix RFC-0001 posed. First, the Basic-password path needs no
git version floor of its own. It works at 2.43, so the RFC matrix's `(git ≥ 2.46)`
annotation on that cell was pessimistic; only the helper `authtype`/Bearer cell needs 2.46.
Second, the token-persisting `http.extraheader` path S3 exercised is unnecessary for the
runner; it stays a portable fallback, not the pinned mechanism. The remaining matrix cells
(helper `authtype`/Bearer on git ≥ 2.46, ephemeral per-operation `http.extraheader`) are
noted but not required.

## What this settles for the product

- **`mandat git-credential` is the pinned delivery mechanism.** The binary re-invokes
  itself as a git credential helper (spec §4.1); the broker mints and answers, and git
  never sees the raw token in a persisted or child-readable place. This satisfies the
  §Identity injection one-way door end to end for the write path.
- **US-0008 AC-8.3/AC-8.4 are unblocked.** The runner's git push path can now be built to
  the Basic-password helper mechanism; RFC-0001's condition that "the runner's push path
  stays unbuilt" is lifted.
- **`mandat doctor` still records the git version.** The Basic-password path asserts no
  specific floor, but doctor records the tested git version so a later move to an
  `authtype`/Bearer mechanism has a baseline (RFC-0001 §Identity injection, §4.10).

## Honest remainder and caveats

- **Proven on the dogfood tenant and repo.** Clone, push, and the invariant check ran
  against the kept spike repo on the dogfood tenant. The delivery mechanism is git-version
  and protocol behaviour, portable across ADO orgs, but the delegated token itself carries
  the same conditional-access caveat as ADR-0005: a tenant CA baseline requiring MFA or a
  compliant device could refuse the non-interactive token, untestable without Entra ID P1
  (RFC-0001 §Open questions).
- **Revocation stays TTL-bounded.** Inherited from S1 round 3 and S3: a token already
  handed to git works until its roughly one-hour TTL expires; revocation blocks new mints
  immediately.

## Assets kept

Reuses the S1/S3 assets unchanged: the paired agent user on its Basic license, the
oauth2PermissionGrant delegating it to the agent identity on the ADO resource, its project
Contributors membership, and the spike repo seeded with `main`. The broker helper script
is a spike artifact modelling `mandat git-credential`, kept as the reference for the
runner's implementation.

## Relation to other records

Resolves the open question named in [RFC-0001](../rfc/RFC-0001-mvp-pipeline.md) (§Identity
injection, §Open questions: invariant-preserving git credential delivery) and falsifies
the F4 kill criterion its red-team raised. Builds on [S3](./S3-git-write-path.md), which
proved the delegated token authenticates against ADO over HTTPS but exercised the
token-persisting `http.extraheader` path; this spike proves the invariant-preserving
delivery S3 left open. Unblocks [US-0008](../issues/US-0008-identity-broker-git-credential.md)
AC-8.3 and AC-8.4.
