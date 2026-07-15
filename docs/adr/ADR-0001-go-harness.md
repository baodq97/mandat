---
id: ADR-0001
title: Go harness — toolchain, gates, and layout
status: proposed
owner: TBD
date: 2026-07-15
---

# ADR-0001: Go harness — toolchain, gates, and layout

## Context

The repo has a deterministic harness for docs (govkit: `check` gate, PreToolUse audit hook, INDEX sync) but none for code, and Go code is about to exist. Three design decisions constrain the harness: D3 (one static Go binary — cgo creep would break the distribution promise), D4 (SQLite through a pure-Go driver, which only holds while `CGO_ENABLED=0` builds keep passing), and §9 of the system design (race-safe pure cores, contract tests on every I/O seam). The docs harness set the philosophy: gates must be deterministic, keyless, and tuned for zero false positives — a gate that cries wolf gets bypassed.

## Decision

- **Toolchain**: Go 1.26.5, pinned in `go.mod` (patch-level `go` directive; the toolchain auto-download honors it on machines with older installs).
- **Layout**: `cmd/mandat` for the single command, `internal/` for every package (one per plane as they materialize). No `pkg/` directory. Until a governed PRD/RFC is ratified, the tree holds only a harness-proving slice (`cmd/mandat`, `internal/buildinfo`) with no product logic.
- **Task runner**: Makefile. `make check` is the single aggregate gate — format diff, lint, `go test -race -shuffle=on -count=1`, a go.mod tidy check, `govulncheck`, and a `CGO_ENABLED=0` build. CI runs the same Make target, so the gate has one definition and two consumers (local, CI).
- **Linting and formatting share one tool**: golangci-lint v2 with a curated linter set biased to zero false positives, never `enable-all`. Formatting is gofumpt-strict plus goimports, run through `golangci-lint fmt` (`--diff` exits non-zero in the gate) so format and lint cannot drift apart. Exact tool versions are pinned where they run (Makefile, go.mod) — those files are the source of truth, not this ADR.
- **Tool pinning, split by what upstream supports**: `govulncheck` is pinned through the go.mod `tool` directive, the official Go 1.24+ mechanism for dev tools; golangci-lint is a pinned binary install into repo-local `bin/`, because its own docs rule out go-install and tool-directive installs.
- **Vulnerability scanning**: `govulncheck ./...` in the gate.
- **Static-binary invariant as a gate**: the `CGO_ENABLED=0` build step exists to fail the moment a dependency drags in cgo, enforcing D3/D4 mechanically instead of by review vigilance.
- **CI**: one GitHub Actions workflow, two jobs — the docs gate (`npx govkit check`) and the Go gate (the Make targets). Both keyless.

## Consequences

- One command (`make check`) proves the whole repo locally; CI cannot drift from it because it calls the same targets.
- cgo creep, data races, and known-vulnerable dependencies fail at gate time, not at a customer install.
- Pinned tool versions mean periodic bump chores. Each pin lives where its tool runs: go.mod (govulncheck, bumped by Dependabot), the Makefile (golangci-lint, manual), and the CI workflow (govkit and action versions; Dependabot bumps the actions, govkit is manual).
- A curated linter set will occasionally miss what `enable-all` would catch; we accept that for a quiet gate. Suppressions require `//nolint:<linter> // reason` with the reason mandatory.

## Alternatives considered

- **Taskfile or mage** instead of Make: nicer syntax, but an extra dependency on every contributor machine and CI image. Make is already everywhere the product targets (enterprise Linux ops, the Gitea/Caddy distribution pattern D3 references). Rejected.
- **`enable-all` linting**: maximal coverage, but the false-positive noise violates the zero-FP philosophy the docs harness established. Rejected.
- **golangci-lint-action in CI**: brings PR annotations and an analysis cache, but introduces a second gate definition that can drift from the local one. Rejected for `make check` as the single definition; annotations can return later if review pain justifies the drift risk.
- **gofumpt as a separately pinned tool**: a second formatter copy whose version can drift from the one golangci-lint vendors. Rejected; `golangci-lint fmt` is the only formatting entry point.
- **goreleaser from day one**: release automation is real, but pre-MVP there is nothing to release; a `-ldflags` version stamp covers the need (Go 1.24+ already auto-stamps the VCS version). Deferred, not rejected.
- **No code harness until MVP code lands**: repeats the reference system's harness-by-willpower failure the design explicitly corrects (§13). Rejected.
