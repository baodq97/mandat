---
id: US-0015
title: Pin the discovery token to the operator-chosen Entra tenant
status: done
owner: baodq97
date: 2026-07-17
priority: P2
---

# US-0015: Pin the discovery token to the operator-chosen Entra tenant

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
- Entry 22 proposed pinning the mint with `--tenant <entra.tenant>`; the shipped fix instead
  pins with `--subscription <the chosen tenant's az account id>`, after a 2026-07-17 live
  probe found `--tenant` against a non-active tenant forces a fresh interactive login while
  `--subscription` mints cleanly against the chosen account without switching az's active
  login.

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

- [ ] AC-15.1 Given a tenant chosen at the picker (or a sole auto-picked tenant), observe
      the `az account get-access-token` invocation for ADO discovery (`attemptDiscovery`) and
      for pre-write validation (`validateADOBeforeWrite`) both include `--subscription <the
      chosen tenant's az account id>`, so the token either call site receives is minted
      against the operator's chosen tenant's account without switching az's active login —
      never az's ambient default. `--tenant` is deliberately not used: a 2026-07-17 live probe
      showed `--tenant` against a non-active tenant forces a fresh interactive login, while
      `--subscription` mints cleanly.
- [ ] AC-15.2 Given the interactive interview flow, observe `entra.tenant` is known (prompted
      and answered) before any discovery or validation token is minted: either the
      `entra.tenant` prompt moves ahead of the discovery call in `runInteractiveInterview`, or
      the discovery call is deferred until after `entra.tenant` is collected. Either ordering
      is acceptable; a discovery or validation call issued with no resolved tenant is not.
- [ ] AC-15.3 Given the `--non-interactive` path, observe it mints no discovery/validation
      token at all (the az-token refuse-gate is interactive-only), so no unpinned mint can
      occur. Given the `--entra-tenant` / `MANDAT_ENTRA_TENANT` override on the interactive
      path, observe the value is resolved before any mint and written to
      `config.entra.tenant`; the override carries no az account id, so the
      discovery/validation mint falls back to az's active account and the operator must have
      that tenant active first (stated in the `--entra-tenant` flag help). KNOWN LIMITATION:
      only the picker/auto-pick path is account-pinned; the override path is not — tracked as
      follow-up.
- [ ] AC-15.4 A contract test substitutes a fake `az` executable on `PATH` for the real CLI
      and asserts the captured argument list contains `--subscription <configured account
      id>` and does NOT contain `--tenant`. The test fails if `--subscription` is absent —
      reverting the pin reproduces a failing test, not a silently-passing one
      (`TestAzCLITokenSource_PinsSubscriptionFlag`).
- [ ] AC-15.5 Given `entra.tenant` is not resolvable (a `--non-interactive` run missing the
      flag/env value; or an interactive run where tenant enumeration failed or returned
      nothing), observe `init` does not mint an unpinned discovery/validation token: the
      `--non-interactive` path fails validation before any mint (`--entra-tenant is
      required`), and the interactive path skips discovery entirely and prompts
      `entra.tenant` manually rather than minting against an unintended account
      (`TestAzCLITokenSource_EmptyAccount_OmitsFlag`; init.go skips discovery when
      `prefill.tenant == ""`).

## Remit

File-disjoint allowed paths:

- `cmd/mandat/init.go`
- `cmd/mandat/init_test.go`

## Dependencies

Follow-up on US-0013 (done): tightens the same `azCLITokenSource` / `attemptDiscovery` /
`validateADOBeforeWrite` surface that story shipped. No dependency on US-0014; see Non-goal.
