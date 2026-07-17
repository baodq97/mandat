---
id: US-0015
title: Pin the discovery token to the configured Entra tenant
status: open
owner: baodq97
date: 2026-07-17
priority: P2
---

# US-0015: Pin the discovery token to the configured Entra tenant

As an operator running `mandat init` on a machine with more than one Entra tenant session, I
want the Azure DevOps discovery and validation token to mint against the tenant I configure
(`entra.tenant`), not whichever tenant my `az` CLI happens to have active by default, so that
`init` never silently discovers or validates against the wrong organization and never mints a
bearer token against a tenant I did not intend.

## Sources

- `docs/issues/US-0013-first-run-init.md`, AC-13.1: the pinned `az`-token discovery chain
  (`attemptDiscovery`) and the pre-write validation refuse-gate (`validateADOBeforeWrite`)
  this story pins to a tenant.
- `LEARNING-LOOP.md`, entry 22 (2026-07-17): a live `mandat init` dogfood run against
  the dogfood ADO org surfaced that `azCLITokenSource` runs `az account get-access-token --resource
  <ADO resource>` without `--tenant`; the operator's laptop had a different (work) tenant
  active by default, and the intended tenant had to be activated by hand before discovery
  would reach the target org. The AC-13.1 refuse-gate caught the resulting mismatch, but only
  after a token had already been minted against the wrong tenant.
- `cmd/mandat/init.go`: the `azCLITokenSource` function (the `tokenSource` implementation
  that shells out to `az account get-access-token`) and the `adoResourceID` constant it pins
  `--resource` to; `internal/discovery` (the package that consumes whatever token it is
  handed — out of scope for this fix, since the gap is in how the token is minted, not how
  it is used).

## Problem

`azCLITokenSource` requests a bearer token with `az account get-access-token --resource
<adoResourceID> --query accessToken -o tsv` and passes no `--tenant`. The token therefore
resolves against whatever tenant `az` currently has active by default, not necessarily the
tenant the same interview collects into `entra.tenant`. On a single-tenant operator VM the two
values happen to coincide; on a multi-tenant operator workstation they can diverge, and entry
22 records exactly that divergence happening on a real run.

Two call sites share the gap, and both go through the same `tokenSource` function signature
(`func(ctx context.Context) (string, error)` — no tenant parameter, so neither call site has a
way to pin one even once `entra.tenant` is known):

- `attemptDiscovery` (prefills the interactive interview) calls `getToken` before
  `entra.tenant` is ever prompted — `runInteractiveInterview` in `cmd/mandat/init.go` runs
  discovery first and prompts `entra.tenant` several fields later — so the tenant is unknown
  at this call site regardless of the function signature.
- `validateADOBeforeWrite` (US-0013 AC-13.1's pre-write refuse gate) calls `getToken` after the
  full interview completes, so `entra.tenant` is known by then, but the token source still has
  no parameter to receive it.

Beyond wrong discovery results, minting a bearer token against a tenant the operator did not
configure is a latent security concern, not only a usability one: it is exactly the
"always pass `--tenant` explicitly, never rely on the `az` default" operating constraint,
left unenforced in code.

## Non-goal

The production runtime agent-identity token path (Arc + FIC / client-certificate, chartered
under US-0014) is untouched by this story. This story fixes only the operator-facing
discovery/validation token `mandat init` mints for itself through the operator's own `az`
session; it does not change how a deployed `RoleAgent` identity obtains its runtime
credentials.

## Acceptance criteria

- [ ] AC-15.1 Given a resolved `entra.tenant` value, observe the `az account
      get-access-token` invocation issued for ADO discovery (`attemptDiscovery`) and for
      pre-write validation (`validateADOBeforeWrite`) both include `--tenant <entra.tenant>`,
      so the token either call site receives is scoped to the operator's configured tenant,
      never `az`'s ambient default.
- [ ] AC-15.2 Given the interactive interview flow, observe `entra.tenant` is known (prompted
      and answered) before any discovery or validation token is minted: either the
      `entra.tenant` prompt moves ahead of the discovery call in `runInteractiveInterview`, or
      the discovery call is deferred until after `entra.tenant` is collected. Either ordering
      is acceptable; a discovery or validation call issued with no resolved tenant is not.
- [ ] AC-15.3 Given the `--non-interactive` and environment-variable input paths (US-0013
      AC-13.9, AC-13.10), observe the same tenant pin applies: `--entra-tenant` /
      `MANDAT_ENTRA_TENANT` is resolved before any discovery or validation token is
      requested, and the resulting `az` call carries the same `--tenant <value>` flag as the
      interactive path.
- [ ] AC-15.4 A contract test substitutes a fake `az` executable (a test-only binary placed
      on `PATH`, or an equivalent process-boundary fake) for the real CLI and asserts the
      captured argument list contains `--tenant <configured value>`. The test fails if
      `--tenant` is absent from the invocation — reverting the fix reproduces a failing test,
      not a silently-passing one.
- [ ] AC-15.5 Given `entra.tenant` is not yet resolvable (a `--non-interactive` run missing
      the flag/env value, or an interactive run reaching the discovery step before the tenant
      prompt under the AC-15.2 ordering), observe `init` does not fall back to an unpinned
      token: it fails the same way the existing "missing irreducible field" path does
      (US-0013 AC-13.4's `FieldError` shape), not a silent wrong-tenant mint.

## Remit

File-disjoint allowed paths:

- `cmd/mandat/init.go`
- `cmd/mandat/init_test.go`

## Dependencies

Follow-up on US-0013 (done): tightens the same `azCLITokenSource` / `attemptDiscovery` /
`validateADOBeforeWrite` surface that story shipped. No dependency on US-0014; see Non-goal.
