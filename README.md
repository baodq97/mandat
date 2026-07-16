# mandat

mandat turns tracker work items (Azure DevOps first, Jira later) into reviewed pull
requests through AI role agents. Every agent acts under an explicit, scoped, revocable
mandate: a Microsoft Entra agent identity sponsored by a named human. The product ships
as one static Go binary with embedded SQLite and runs on one customer Linux VM.

## How it works

1. Poll ADO for work items assigned to the Dev agent user, map each into a `TaskContract`.
2. Provision a git worktree with sparse checkout limited to the item's remit paths.
3. Run Claude Code headless inside that worktree under the Dev role's playbook and remit.
4. Commit and push under the agent's own identity. Hourly Entra tokens are the git
   credential, served by the binary acting as its own git credential helper. No PAT.
5. Open a draft PR and link it back to the work item.
6. Verification re-runs the repo's configured gates and confirms the PR under a separate
   Reviewer agent identity. The writer is never the scorer.
7. The task lands in `in-review`. A human moving the work item to Done is the only act
   that completes it (ratification). Every decision lands in an append-only SQLite journal.

## Design invariants

- SQLite is the only database, permanently. One WAL file under `/var/lib/mandat/`.
- The runner sits behind a file contract. The supervisor validates a schema-checked
  `ResultContract` and never parses agent prose as the outcome.
- Remit is enforced mechanically: sparse checkout, a Claude Code PreToolUse deny hook, a
  post-hoc diff-inside-remit check, and a branch ancestry check. No prompt enforces it.
- Writer != scorer is an IAM property: distinct Entra identities per role plus a
  creator-cannot-approve branch policy.
- Ratification is human-only. No runtime control raises an autonomy ceiling; ceilings
  live in config files the customer reviews like code.
- No PAT anywhere. Production mode keeps zero secrets on disk (Arc plus FIC).
- A new role is a new YAML config entry, never new code.

## Status

Pilot stage (v0.5.x). The walking skeleton is complete and live-proven end to end,
including `pool_size`-bounded concurrent dispatch on a single VM. Product surface beyond
the skeleton stays design-gated by the governed docs.

- Design source: `docs/superpowers/specs/2026-07-15-mandat-system-design.md`
- Governed docs: `docs/product/` (PRD), `docs/rfc/` (RFC), `docs/adr/` (ADR),
  `docs/issues/` (US)
- Learning loop: `LEARNING-LOOP.md`

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/baodq97/mandat/main/install.sh | sh
```

The script detects linux/amd64 and linux/arm64, verifies the checksum, and installs to
`/usr/local/bin/mandat`. Override with env vars: `MANDAT_VERSION` pins a release tag,
`BIN_DIR` picks the install directory.

## Commands

| Command | Purpose |
|---|---|
| `serve` | Poll and dispatch daemon. Flags: `-config`, `-db`, `-role`, `-poll-interval`, `--once` |
| `doctor` | Preflight the install before first dispatch |
| `version` | Print version and build info |
| `git-credential` | Internal. The binary re-invoked as a git credential helper |
| `remit-guard` | Internal. The Claude Code PreToolUse deny hook |

## Get started

See [GETTING-STARTED.md](GETTING-STARTED.md) for the full setup walkthrough: Entra
identities, Azure DevOps wiring, config, and first run.

## Contributing

Read `CLAUDE.md` for the architecture and the code and docs policies. Two keyless gate
families run in CI and locally: `make check` for code, `npx govkit check` for the
governed docs.
