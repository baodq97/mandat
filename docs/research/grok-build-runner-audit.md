# Grok Build audit — a production coding-agent runner, read against Mandat's D6

Audit-only. No code, no RFC. Read for what Mandat's runner and remit planes can borrow.

**Source:** `xai-org/grok-build` (cloned depth 50, single squashed publish commit `c68e39f`,
2026-07-16). SpaceXAI's terminal AI coding agent (`grok`), Rust, ~2,237 `.rs` files across
~50 crates, synced periodically from an internal monorepo. Apache-2.0.
**Method:** 4 parallel sub-agent deep-dives (agent core + lifecycle, tools + sandbox,
workspace + context, product + ops), file:line-cited.

**Why this repo matters to Mandat:** Mandat decision D6 runs Claude Code as the runner
subprocess behind a file-based ResultContract. Grok Build is the same category of thing —
a headless-capable coding agent — but ships the remit-enforcement machinery (kernel
sandbox, compiled permission engine, worktree isolation) that Mandat's design so far
specifies as intent. It is both a candidate alternate runner and a reference implementation
for planes 4.5-4.7.

---

## 1. What it is

One Rust binary that runs three ways off a single agent lifecycle: full-screen TUI,
headless (`grok -p`), and an ACP server (`grok agent stdio`) speaking Zed's standard Agent
Client Protocol v0.10.4. Headless is not a separate path — it spawns the agent in-process
and drives the same ACP lifecycle over internal channels, so the headless contract is as
rich as the interactive one.

Architecture is actors over channels throughout: a session actor owns the turn loop, a
chat-state actor builds each request (KV-cache-aware pruning, image eviction near a 50 MB
body ceiling), a sampler actor makes cancellable per-request model calls. The model layer
ships one model (`grok-build`, 500K context) resolved from a server-controlled catalog, but
the sampler natively speaks three wire protocols: Chat Completions, OpenAI Responses, and
**Anthropic Messages** (`/messages`, 128K default max tokens).

## 2. Findings that change how I'd build Mandat

**G1. The headless contract is richer than Claude Code's, exactly where Mandat needs it.**
`grok -p` supports `--output-format plain|json|streaming-json`, `--json-schema` for
structured output taken only from agent-validated `_meta` (never parsed from the text
buffer — "that would bypass validation"), `--worktree`, `--agents <json>` inline agent
definitions, `--allow`/`--deny` permission rules with `ToolPrefix(glob)` semantics,
`--max-turns`, session resume/fork. Exit codes are typed: 0 success, 1 error (incl.
max-turns), **2 managed-config policy violation**. Background processes (bash, subagents,
monitors) are reaped at exit via `x.ai/task/kill` so nothing outlives the run. This is a
stronger ResultContract than Mandat's spec currently sketches — the schema-validated
structured output and the policy-violation exit code are directly adoptable.

**G2. The permission engine is mechanical and orders deny before bypass.** A single-owner
permission actor (5,633 LOC) evaluates compiled policy rules **before** YOLO/bypass mode, so
a deny rule cannot be overridden by bypassPermissions. Precedence deny > ask > allow. Bash
rules are checked per chained segment with wrapper-peeling (`timeout`, `env`, `nice`, `bash
-c` recursion) and fail closed to Ask on an unparseable script. This is the writer≠scorer
and remit-enforcement discipline Mandat wants, proven in code: enforcement lives in an
actor the loop must call, not in a prompt.

**G3. Kernel-grade sandbox — the mechanism Mandat's remit plane hand-waves.** `xai-grok-sandbox`
uses the `nono` crate: **Landlock on Linux, Seatbelt on macOS**, applied once at startup and
irreversible, plus **bwrap re-exec** for path denies (read-only binds for write-deny,
mode-000 bind-over for read-deny) and **per-child seccomp BPF** blocking network syscalls
(`connect/bind/sendto/...` → EPERM). Profiles (`workspace`/`devbox`/`strict`/custom) are
config; project config may only add profiles, never redefine global ones. This is a working
answer to Mandat plane 4.5's "sparse checkout + OS user + diff probe": Landlock + seccomp is
a stronger middle layer than per-role OS users alone.

**G4. Worktree isolation with disposable directories, state as git objects.** `xai-fast-worktree`
is a tiered dispatcher: overlay-on-FUSE (zero copies on their fleet) → btrfs snapshot →
parallel reflink CoW copy (`reflink_copy` on APFS/Btrfs/XFS) → plain git. Removal bypasses
`git worktree remove` for `rm -rf` + manual registration cleanup (~10× faster on 100K-file
repos). The cleverest part for Mandat: subagent isolation doesn't keep worktree directories
alive — dirty state is committed to a **hidden git ref** via a scratch `GIT_INDEX_FILE`
(`read-tree` → `add -A` → `write-tree` → `commit-tree` → `update-ref`) and rehydrated at the
original base on resume, so restored changes reappear as ordinary modifications. Mandat's
per-task worktree plane should steal this: worktrees disposable, state durable as objects.

**G5. Folder-trust is one gate over every code-execution surface.** A single enumerator
lists everything a repo can use to run code (`.mcp.json`, `.envrc`, hooks,
`.claude/settings.json`, plugins, agents dirs) and one decision function gates them all,
stored in `~/.grok/trusted_folders.toml` (atomic 0600), with worktrees collapsing onto the
source repo's trust key. Cleaner than per-feature trust checks — a pattern Mandat's config
plane should copy rather than scatter trust decisions.

**G6. Enterprise fleet control is cryptographic and asymmetric.** Managed config is an
Ed25519-signed, identity-bound, expiring envelope with a `fail_closed` bit **inside the
signed bytes** "so a local actor can't flip enforcement", verified against compiled-in keys.
Server-side minimum-version enforcement refuses to start below a floor; remote announcements
push banners. Yet the auto-updater's own binary download has **no checksum or signature** —
integrity rests on TLS plus a `--version` smoke test. Strong policy-tamper story, weak
supply-chain story. Mandat's D5 (Entra-signed authority) is the same instinct done through
identity instead of a signed blob; the lesson is to not leave the update path unsigned.

**G7. Half-lit features are a maturity tell, honestly labeled.** Memory ships behind
`GROK_MEMORY=1` with vector recall inert until an embedding model is configured (FTS-only
default). The signed-policy machinery compiles with an empty trusted-key set ("ships dark").
Both fully built, neither fully on — the docs say so plainly, the same honest-gaps discipline
I want in Mandat's own docs.

## 3. Weakest rails (what NOT to copy)

- **Allow-rule chaining hole**: allow rules match the whole command string, so `Bash(git *)`
  auto-approves `git status && rm -rf /`. Deny/ask are per-segment, allow is not — documented,
  not fixed. Mandat's remit must treat allow as per-segment too, or not offer command-prefix
  allow at all.
- **Hooks fail open and are deny-only**: 14 events but only PreToolUse can block, and a
  crashed/timed-out/malformed hook lets the tool proceed. The docs market hooks as an
  allowlist while admitting they aren't a boundary. Mandat gates must fail closed.
- **Sandbox off by default** and silently degrades to unsandboxed on unsupported platforms
  (one log line). Linux deny globs don't cover files created after launch. Path rules are
  uncanonicalized — a `Read(**/.env)` deny is dodgeable via symlink through direct tools.
- **Auto-mode puts an LLM classifier inside the permission gate** with transcript context —
  a prompt-injected transcript can steer it to Allow (mitigated: Block/Unavailable prompts
  instead of silently rejecting). Mandat should keep the LLM out of the enforcement path.

## 4. Comparison — Grok Build vs Mandat vs govkit

| Axis | Grok Build | Mandat (design) | govkit |
|---|---|---|---|
| What it is | Coding-agent runner (TUI/headless/ACP) | Control plane orchestrating runners on trackers | Docs-as-code spec governance |
| Governs | The agent's actions at the syscall level | The agent's mandate + tracker lifecycle | The specification at rest |
| Enforcement | Landlock/seccomp/bwrap + permission actor | Sparse checkout + OS user + Entra identity + diff probe | Deterministic file gate, zero-FP |
| Identity | x.ai account / OIDC / deployment key | Entra agent identity per role | git author, human ratification |
| Runner relation | **is a candidate runner for Mandat D6** | consumes a runner | not a runtime |
| Trust surface | folder-trust + signed managed config | mandate + config-in-git | commit discipline + gate |

Grok Build sits one layer below Mandat: it is the thing that runs inside a Mandat task box.
The two are complementary, not competitive. govkit is orthogonal to both — it governs the
specs that would feed Mandat's PO/SA roles.

## 5. Verdicts

**B1. Adopt the headless contract shape for Mandat's runner interface (G1). BORROW.**
ResultContract should be schema-validated structured output pulled from a validated channel,
not parsed prose; the runner interface should type its exit conditions (success / error /
policy-violation) the way grok does. This firms up D6. Next: fold into the runner-supervisor
section of the spec when it reaches RFC.

**B2. Take worktree-state-as-git-ref for the workspace plane (G4). BORROW.**
Mandat plane 4.5 should make per-task worktrees disposable and commit dirty state to hidden
refs for resume, exactly as `xai-fast-worktree` does. Removes the "keep directories alive"
failure mode. Next: a spike alongside S4.

**B3. Evaluate Grok Build as an alternate/second runner behind the same interface. INVESTIGATE.**
D6 already says the CLI-subprocess seam keeps the runner replaceable. Grok speaks ACP (an
open standard) and Anthropic Messages wire format; a Mandat runner abstraction that targets
ACP rather than one vendor's CLI would be more durable. Trade-off: Grok is Grok-model-first
and its catalog is server-controlled. Next: note ACP as the runner-interface candidate in
the spec; do not commit until the Claude Code path is proven.

**B4. Model the sandbox layer on nono/Landlock, not just OS users (G3). ADAPT.**
Mandat's remit enforcement is stronger with a kernel confinement layer under the per-role OS
user. Landlock (Linux) is the right primitive for a Linux-VM-first product. Next: add a
"kernel confinement" bullet to plane 4.5's three layers, making it four.

**B5. One trust gate over all code-execution surfaces (G5). ADAPT.**
Mandat's config plane should enumerate every repo-controlled execution surface behind one
decision function, not scatter checks. Cheap, and it closes the AI-Autopilot-class hole
where the harness config lived unversioned and ungated.

**Do NOT copy:** allow-rule whole-string matching, fail-open hooks, sandbox-off-by-default,
LLM-in-the-permission-path (§3). Every one is a rail Mandat's design already commits to
making fail-closed; grok is the cautionary instance.

---

*Cloned copy at scratchpad `grok-build/` (session-temporary). Apache-2.0: patterns and code
both portable with attribution + change notices; grok itself ports codex/opencode tool code
under the same terms. This file is audit-only research outside the governed doc tree.*
