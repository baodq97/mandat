---
id: US-0004
title: Tracker adapter — WIQL poll, TaskContract mapping, ADO work-item and PR write
status: open
owner: TBD
date: 2026-07-16
priority: P1
---

# US-0004: Tracker adapter — WIQL poll, TaskContract mapping, ADO work-item and PR write

As a mandat contributor, I want the ADO adapter to poll WIQL for work items assigned to
the Dev agent user, map them to TaskContracts, and write comments and draft PRs under that
identity, so that ingestion and the ADO write path share one seam behind the
`Tracker`/`Forge` interfaces.

## Source

RFC-0001 (accepted) §Scope (30s WIQL poll, consent = assignment), §Load-bearing contracts
(TaskContract), §Package layout (`internal/tracker`, `internal/adapter/azuredevops`),
AC-01..AC-09.

## Scope

- `internal/tracker`: the `Tracker` interface (poll, comment, apply) and the `Forge`
  interface (create PR). Trivially small (two interfaces); merged into this story rather
  than split, per the RFC-0001 package table's own grouping of tracker concerns.
- `internal/adapter/azuredevops`: the ADO implementation — WIQL poll, work-item
  read/comment, draft-PR create via REST.

## Acceptance criteria

- [ ] AC-4.1 Given a recorded ADO WIQL fixture with one work item assigned to
      `<dev-agent-user>`, observe the dispatcher enqueues exactly one task (RFC-0001
      AC-01; §9 double: recorded ADO fixture).
- [ ] AC-4.2 Given a fixture work item not assigned to `<dev-agent-user>`, observe it is
      never enqueued (RFC-0001 AC-02; §9 double: recorded ADO fixture).
- [ ] AC-4.3 Given the same work item polled twice against the fixture, observe no
      duplicate task is created, idempotent on `tracker_ref` (RFC-0001 AC-03; §9 double:
      recorded ADO fixture).
- [ ] AC-4.4 Given the WIQL poll test suite, observe it makes no live ADO call — every
      response comes from the recorded fixture (RFC-0001 AC-04; §9 double: recorded ADO
      fixture).
- [ ] AC-4.5 Given a fixture work item, observe the adapter maps it to a TaskContract with
      `id`, `tracker_ref`, `type=dev-task`, `title`, `acceptance`, `refs=[]`,
      `state=queued`, `role=dev` (RFC-0001 AC-06; §9 double: recorded ADO fixture).
- [ ] AC-4.6 Given a fixture work item whose repo is present in the repo registry, observe
      the TaskContract's `remit` is filled from the registry's defaults for that repo, not
      from any ADO field (RFC-0001 AC-07; §9 double: recorded ADO fixture; consumes
      `internal/config`'s repo registry type — see the gaps list for the cross-story
      sequencing tension).
- [ ] AC-4.7 Given a fixture work item whose repo is absent from the registry, observe no
      TaskContract is produced and a journaled skip is recorded, with no silent default
      (RFC-0001 AC-08; §9 double: recorded ADO fixture).
- [ ] AC-4.8 Given a TaskContract produced by the adapter, observe it round-trips through
      `internal/task`'s validation with no missing-field error (RFC-0001 AC-09, adapter
      side; §9 double: recorded ADO fixture).

## Remit

File-disjoint allowed paths:

- `internal/tracker/**`
- `internal/adapter/azuredevops/**`

## Dependencies

Depends on US-0001 (TaskContract type to map into). Cross-dependency: AC-4.6 needs
`internal/config`'s repo registry, owned by US-0009 (P2), scheduled after this story (P1)
— flagged in the gaps list, not resolved by invention. AC-26 (PR opened under
`<dev-agent-user>`, `createdBy` confirmed) and AC-27 (Reviewer-identity probe) are **not**
provable in this story: RFC-0001 states both require a live integration check against the
kept S1/S3 spike assets, never the §9 fixture double. This story builds the `Forge`
interface and its `CreatePR` call; live proof is out of its contract-test scope.
