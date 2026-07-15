---
id: ADR-0006
title: Runner subprocess contract — Claude Code CLI headless over the Agent SDK
status: accepted
owner: baodq97
date: 2026-07-16
---

# ADR-0006: Runner subprocess contract — Claude Code CLI headless over the Agent SDK

## Context

mandat drives Claude Code to do the software-engineering work (D2). The runner plane spawns the agent once per task, feeds it a TaskContract, and reads back a schema-validated ResultContract file; the supervisor validates that file against schema and never parses prose (§4.6). This ADR settles the runner's integration seam: drive the Claude Code CLI headless as a subprocess, or embed the Claude Agent SDK in-process. D6 already names the direction; this records the grounds and pins the interface as the contract.

Two constraints decide the seam. The product is one Go static binary built `CGO_ENABLED=0` (D3, ADR-0001), so a second language runtime on the VM is disqualifying. The runner already sits behind a file contract (§4.6), so the host consumes JSON and a result file, not an in-process object model. Verified against the Claude Code docs and the local CLI (v2.1.210).

## Decision

Drive the Claude Code CLI headless as a per-task subprocess over stdio and files (D6). Do not embed the Agent SDK. Three grounds carry it:

1. **There is no Go Agent SDK.** The SDK ships only as Python and TypeScript (§12). Embedding it forces a Node or Python sidecar onto the VM, a second runtime that breaks the single-binary promise (D3).
2. **The SDK is a thin wrapper over the same CLI binary.** Its result and system message types are the exact JSON the CLI emits under `--output-format stream-json`. A Go host parses that JSON directly and skips the wrapper.
3. **The SDK's one net-new capability is the one mandat rejects.** Over the CLI the SDK adds only in-process callbacks (`canUseTool`, function `hooks`, in-process custom tools). mandat replaces those deliberately with mechanical OS-level isolation (per-role OS user, sparse-checkout worktree, post-hoc diff-inside-remit, §4.5–§4.6) plus the schema-validated result file. Using the SDK would adopt a dependency for the exact behaviour the mandate model refuses to trust to in-process code.

## Consequences

The runner stays replaceable, but the portable interface is the subprocess boundary plus the ResultContract file, not the flag set: swapping in another headless runner (Gemini CLI, Codex are the named candidates, §12) needs a per-runner invocation adapter, while nothing above the seam changes. The seam is contract-tested per §9, never mocked at the pure core.

The invocation contract, pinned:

- **Invocation.** `claude -p --output-format stream-json --verbose`; `--input-format stream-json` for streaming multi-turn input; `--model <role tier>`; `--permission-mode dontAsk` (no human is present to prompt in a headless run); `--allowedTools` / `--disallowedTools` as a coarse filter; `--add-dir` bounded to the task worktree as the sole cwd; `--append-system-prompt[-file]` for the role playbook plus the remit statement; `--settings` inline JSON to inject a PreToolUse deny hook; `--mcp-config` with `--strict-mcp-config`; `--bare` for reproducible runs (skips auto-discovery of ambient hooks, skills, MCP, and CLAUDE.md, and the keychain, so the role playbook loads from the per-role config dir and auth comes from `ANTHROPIC_API_KEY` or an `apiKeyHelper`); `--max-budget-usd` as a cost guardrail.
- **stream-json is telemetry; the ResultContract file is the contract.** The `system/init` event carries `session_id`; the terminal `result` event carries `total_cost_usd`, `usage`, `num_turns`, `is_error`, `subtype`, and `duration_ms`. Both are recorded per run in the Journal (§4.9); neither is parsed as the task outcome, which comes only from the validated file (§4.6).
- **Session continuity.** Pin a deterministic `--session-id <uuid>` at spawn, record it in the Journal before spawning, and `claude -p --resume <uuid>` from the same worktree to continue a task (for example after `needs-human`). Resume is scoped to the worktree.
- **Isolation.** Set `HOME` and `CLAUDE_CONFIG_DIR` per child process so each per-role OS user gets its own config and session store; the PreToolUse command hook is the mechanical deny layer, the same mechanism the repo already runs for `govkit audit-write`.
- **Version floor.** Require CLI ≥ 2.1.208, which fixes a truncated final `result` line on large piped stdout; that truncation would corrupt the telemetry mandat records (`session_id`, cost, `usage`), not the ResultContract itself, which the subprocess writes to a fixed worktree path and the supervisor reads independently of stdout. Feature-detect via the init event's capability list where the CLI exposes it (per docs, CLI ≥ 2.1.205), otherwise fall back to the version `mandat doctor` asserts before first dispatch (§4.10).

The consequence for comments and code: the flag set and the stream-json event shape are an external contract that the code cannot express, so they are documented here and referenced from the runner package rather than restated in prose (ADR-0003). The interface is pinned to the smallest surface the pipeline needs (ADR-0004); flags outside this list are not wired until a task requires them.

## Alternatives considered

- **Claude Agent SDK embedded.** Rejected: no Go SDK exists, so it forces a Node or Python sidecar that breaks the single-binary D3; it is a thin wrapper over the same CLI JSON the Go host already parses; and its in-process callbacks are the capability mandat replaces with OS-level isolation (§4.5–§4.6). Adopting it buys nothing the CLI path lacks and costs the second runtime.
- **Raw Anthropic Messages API with a hand-built agent loop.** Rejected: D2 chose to drive Claude Code rather than rebuild the agent harness (tools, permissioning, context management) that mandat would then own and maintain. This alternative reverses D2.
- **A fake `claude` as the product runner.** Rejected: the scripted fake binary emitting stream-json and ResultContracts is a contract-test double only (§9), never the product path.
