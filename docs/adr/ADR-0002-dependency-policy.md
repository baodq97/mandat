---
id: ADR-0002
title: Dependency policy — prefer libraries over own code, vet and stay current
status: accepted
owner: baodq97
date: 2026-07-15
---

# ADR-0002: Dependency policy — prefer libraries over own code, vet and stay current

## Context

Product code is about to exist, and the first real dependencies with it (tracker API clients, a SQLite driver, YAML parsing). Two forces pull against each other: D3/D4 demand a single static CGO-free binary with a small, trustworthy dependency surface, while hand-rolling what a maintained library already does well produces more code to own, review, and get wrong. The owner set the direction explicitly: use what exists, write as little as possible, but vet what gets used — staleness and security both.

## Decision

**Selection ladder** — when a capability is needed, take the first rung that fits:

1. **Go standard library** (including `golang.org/x` modules).
2. **The official SDK** for the service being integrated (e.g. Azure SDK for Go, official Anthropic SDK if one ships for Go).
3. **A popular, maintained third-party library**.
4. **Hand-written code — last resort**, kept as small as possible, only when no rung above fits or when a candidate fails vetting.

**Vetting checklist** — every new direct dependency must pass, recorded in the PR description that introduces it:

- **Alive**: a release or substantive commit within the last 12 months; issues get maintainer responses.
- **Adopted**: meaningful ecosystem usage (imported-by count on pkg.go.dev, or the de-facto standard for the niche).
- **Secure**: no open vulnerabilities (`go tool govulncheck` passes after adding it; check the OSV/GitHub advisory record for the module).
- **Current**: pinned at the latest stable release at introduction time, never an old version or an unreleased commit.
- **Static-compatible**: builds under `CGO_ENABLED=0` (the gate enforces this mechanically).
- **License**: compatible with commercial distribution (no copyleft that captures the binary).

**Mechanical enforcement** — policy backed by the harness, not by memory:

- Dependabot (`.github/dependabot.yml`) raises weekly grouped PRs for Go modules and GitHub Actions; every bump must pass the full `make check` gate before merge. "Stay latest" is automated for those two ecosystems; the two pins outside them (golangci-lint in the Makefile, govkit in the CI workflow) remain manual bumps and are the known residue of this policy.
- `go tool govulncheck` remains a blocking step in `make check` — known-vulnerable dependencies cannot pass the gate.
- `make deps-check` reports direct dependencies with newer versions available. Advisory by design: blocking CI on "a newer version was released last night" would violate the zero-false-positive gate philosophy (ADR-0001).
- The `CGO_ENABLED=0` build step in the gate rejects any dependency that drags in cgo.

The judgment rungs (is it popular enough, is it abandoned) stay human/reviewer decisions guided by this checklist — a star-count threshold in CI would be a badge, not a gate.

## Consequences

- Less hand-written code to own; new capabilities start with a library search, not a blank file.
- Every dependency addition carries a small documentation cost (the checklist in the PR) — accepted, it is the paper trail the vetting decision lives in.
- Weekly Dependabot PRs create recurring review traffic; grouping keeps it to roughly one PR per ecosystem per week, and the gate does the verification work.
- The latest-version rule means occasional churn from upstream breaking changes; the gate catches breakage at bump time, which is exactly when it is cheapest to handle.

## Alternatives considered

- **Dependency-free purism** (hand-roll everything): maximal control, but repeats the reference system's pattern of owning code a library community already maintains better. Rejected.
- **Blocking CI when any dependency is outdated**: enforces "latest" hard, but fails builds on upstream release timing the team does not control — a false-positive generator. Rejected; Dependabot + advisory `deps-check` covers it.
- **Vendoring (`go mod vendor`)**: reproducibility is already covered by go.sum and module proxy sums; vendoring adds repo weight without adding trust. Deferred unless an air-gapped customer build requires it.
- **A CI popularity/staleness gate** (stars, last-commit age): not deterministic-zero-FP; niche-but-correct libraries would fail it. Kept as a review checklist instead.
