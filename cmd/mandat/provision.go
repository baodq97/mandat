package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/baodq97/mandat/internal/entra"
)

// graphPlanBaseURL is the production Graph v1.0 host the dry-run create-plan
// prints its endpoints against. The plan documents the exact call an operator
// (or a later write slice) would issue against real Graph, so it names the
// production host even when --graph-url points the reads at a test server.
const graphPlanBaseURL = "https://graph.microsoft.com/v1.0"

// provisionRoles is the set of RoleAgents the dry-run create-plan checks for —
// the MVP roles (serve.go's dev/reviewer). A role counts as provisioned when an
// agent identity's displayName carries its name; the match is a plan-format
// heuristic for the reuse report only and never gates or writes.
var provisionRoles = []string{"dev", "reviewer"}

// graphTokenSource builds the Graph token source provision reads under, pinned
// to a resolved az account (--subscription accountID) so the mint targets the
// chosen account without switching az's active login — the pin that works where
// --tenant would force a fresh interactive login (US-0014 F1; live probe
// 2026-07-17). A package-level factory var (like init.go's tokenSource seam) so
// provision_test.go injects a fake with no az shellout; production is the
// az-backed source.
var graphTokenSource = entra.AzureCLIGraphTokenSource

// deriveProvisionAccount resolves the az account (subscription) every provision
// mint pins to when --subscription is absent: the active az session's account id
// (NOT its tenant id — --subscription needs an account id, and a tenant that owns
// subscriptions has account id != tenant id). Read explicitly so the Graph and
// sponsor calls pin the same value per invocation rather than inheriting az's
// non-sticky ambient default (pilot F4). A package-level seam so a test resolves
// it with no az shellout; --subscription overrides it.
var deriveProvisionAccount = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "az", "account", "show", "--query", "id", "-o", "tsv").Output()
	if err != nil {
		return "", fmt.Errorf("az account show: %w", err)
	}
	account := strings.TrimSpace(string(out))
	if account == "" {
		return "", errors.New("az account show returned no account id")
	}
	return account, nil
}

// deriveSponsor resolves the default sponsor object id for a created agent
// identity — the signed-in az user. It best-effort pins the lookup to the chosen
// account (--subscription accountID), but az ad signed-in-user show follows the az
// LOGIN context, so when the pinned account is not the active login it resolves
// the wrong user; pass --sponsor explicitly in that case (the flag help says so).
// A package-level seam (like graphTokenSource) so provision_test.go injects a fake
// with no az shellout; production shells az. A created identity is sponsored by a
// named human (the Mandate invariant); the operator can override with --sponsor.
var deriveSponsor = func(ctx context.Context, accountID string) (string, error) {
	args := []string{"ad", "signed-in-user", "show", "--query", "id", "-o", "tsv"}
	if accountID != "" {
		args = append(args, "--subscription", accountID)
	}
	out, err := exec.CommandContext(ctx, "az", args...).Output()
	if err != nil {
		return "", fmt.Errorf("az ad signed-in-user show: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveProvisionAccount is the az account every provision mint pins to: the
// explicit --subscription flag when set, else the active account
// deriveProvisionAccount reads. Overridable by construction, and never left
// unresolved — an unpinned Graph mint would silently target az's active account
// even when the operator meant another (US-0014 F1).
func resolveProvisionAccount(ctx context.Context, accountFlag string) (string, error) {
	if a := strings.TrimSpace(accountFlag); a != "" {
		return a, nil
	}
	return deriveProvisionAccount(ctx)
}

// provision runs US-0014's ensure-read (reuse) path: it discovers the Entra
// Agent-ID registry — the blueprint and each role's agent identity plus paired
// agent user — and reports it, creating nothing. With --dry-run it additionally
// prints the create-plan (method, endpoint, representative body) for any
// blueprint or role identity that is absent, still issuing zero writes. This is
// the "auto when possible" read side that grounds US-0014's create ensure-flows.
func provision(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "print the plan for any absent blueprint/role identity (or the ensure-identity POST); still issues zero writes")
	graphURL := fs.String("graph-url", "", "override the Microsoft Graph base URL (test seam)")
	ensureIdentity := fs.String("ensure-identity", "", "idempotently ensure an agent identity with this displayName exists under the blueprint; prints the POST before issuing it")
	ensureBlueprint := fs.String("ensure-blueprint", "", "idempotently ensure the installation's agent-identity blueprint (and its principal) with this displayName exists; prints the POST(s) before issuing them (needs the Agent ID Developer/Administrator role)")
	sponsor := fs.String("sponsor", "", "sponsor object id(s) for a created agent identity (comma-separated); default = the signed-in az user (pass this explicitly when --subscription pins an account other than your active az login, since az ad signed-in-user follows the login context)")
	subscription := fs.String("subscription", "", "az account/subscription id to pin every az mint to (Graph token via --subscription, sponsor lookup); default = the active az account (az account show --query id)")
	ensureRole := fs.String("ensure-role", "", "idempotently ensure a role's full agent identity + paired user under the blueprint, created AS the blueprint (its own client-credential token: no operator standing privilege). The arg is the identity displayName; the user gets <role>@<--upn-domain>. Needs --blueprint-app-id, --blueprint-tenant, --upn-domain, and one of --blueprint-secret-env/--blueprint-secret-file")
	blueprintAppID := fs.String("blueprint-app-id", "", "the blueprint's appId — the client_id the client-credential token is minted for, and the agentIdentityBlueprintId the identity is created under (--ensure-role)")
	blueprintTenant := fs.String("blueprint-tenant", "", "the tenant the blueprint lives in — the token endpoint the client-credential token is minted against (--ensure-role)")
	blueprintSecretEnv := fs.String("blueprint-secret-env", "", "name of the environment variable holding the blueprint client secret; the value is read at mint time and never written to disk (--ensure-role). Mutually exclusive with --blueprint-secret-file")
	blueprintSecretFile := fs.String("blueprint-secret-file", "", "path to a file holding the blueprint client secret (the systemd LoadCredential= delivery); read at mint time, never written elsewhere (--ensure-role). Mutually exclusive with --blueprint-secret-env")
	upnDomain := fs.String("upn-domain", "", "verified tenant domain for a created agent user's userPrincipalName, e.g. contoso.onmicrosoft.com (--ensure-role)")
	authorityURL := fs.String("authority-url", "", "override the Entra STS authority base URL for the client-credential mint (test seam)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()

	resolvedAccount, err := resolveProvisionAccount(ctx, *subscription)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: resolve az account: %v\n", err)
		return 1
	}

	client, err := entra.New(entra.Config{GraphBaseURL: *graphURL, TokenSource: graphTokenSource(resolvedAccount)})
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: %v\n", err)
		return 1
	}

	switch {
	case *ensureBlueprint != "":
		return runEnsureBlueprint(ctx, client, *ensureBlueprint, *sponsor, resolvedAccount, *dryRun, stdout, stderr)
	case *ensureIdentity != "":
		return runEnsureIdentity(ctx, client, *ensureIdentity, *sponsor, resolvedAccount, *dryRun, stdout, stderr)
	case *ensureRole != "":
		cred := blueprintCred{
			appID:        *blueprintAppID,
			tenant:       *blueprintTenant,
			secretEnv:    *blueprintSecretEnv,
			secretFile:   *blueprintSecretFile,
			authorityURL: *authorityURL,
		}
		return runEnsureRole(ctx, client, cred, *graphURL, *ensureRole, *sponsor, resolvedAccount, *upnDomain, *dryRun, stdout, stderr)
	}

	reg, err := client.DiscoverRegistry(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: discover Entra Agent-ID registry: %v\n", err)
		return 1
	}

	printRegistry(stdout, reg)
	if *dryRun {
		printCreatePlan(stdout, reg)
	}
	return 0
}

// runEnsureIdentity runs US-0014's ensure-role-identity write for one identity.
// It resolves the sponsoring human(s) and the owning blueprint, prints the exact
// POST — full body including the sponsor @odata.bind list — before issuing it
// (AC-14.7), then, unless dryRun, ensures the identity exists idempotently
// (list-then-create, AC-14.3) and reports created-vs-reused. A *PrivilegeError
// (the write returned 403) prints the fail-with-guidance line and exits
// non-zero-but-clean, never a raw Graph 403 dump (AC-14.4).
func runEnsureIdentity(ctx context.Context, client *entra.Client, displayName, sponsorFlag, accountID string, dryRun bool, stdout, stderr io.Writer) int {
	sponsors, err := resolveSponsors(ctx, sponsorFlag, accountID)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: resolve sponsor: %v\n", err)
		return 1
	}

	// The create binds the identity to an existing blueprint (agentIdentityBlueprintId),
	// so ensure-blueprint is a prerequisite: discover it (and the current identities)
	// once, up front.
	reg, err := client.DiscoverRegistry(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: discover Entra Agent-ID registry: %v\n", err)
		return 1
	}
	if reg.Blueprint == nil {
		fmt.Fprintln(stderr, "mandat provision: no agent-identity blueprint found; provision the blueprint first (an identity is created under one).")
		return 1
	}
	blueprintID := reg.Blueprint.ID
	if blueprintID == "" {
		blueprintID = reg.Blueprint.AppID
	}

	call, err := client.AgentIdentityCreateCall(blueprintID, displayName, sponsors)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: %v\n", err)
		return 1
	}

	// AC-14.7: print the exact mutation before it is issued. The POST is sent
	// only when the identity is absent; on a reuse it is never sent.
	fmt.Fprintf(stdout, "WRITE (issued only if the identity is absent): %s %s\n", call.Method, call.Endpoint)
	fmt.Fprintf(stdout, "    body: %s\n", call.Body)

	if dryRun {
		for _, id := range reg.Identities {
			if id.DisplayName == displayName {
				fmt.Fprintf(stdout, "PLAN (dry-run, no write): agent identity %q already exists (id %s); ensure would reuse it and issue zero writes.\n", displayName, id.ID)
				return 0
			}
		}
		fmt.Fprintf(stdout, "PLAN (dry-run, no write): agent identity %q is absent; ensure would issue the POST above. Issued zero writes.\n", displayName)
		return 0
	}

	identity, created, err := client.EnsureAgentIdentity(ctx, blueprintID, displayName, sponsors)
	if err != nil {
		var privErr *entra.PrivilegeError
		if errors.As(err, &privErr) {
			fmt.Fprintf(stderr, "mandat provision: %v\n", privErr)
			fmt.Fprintln(stderr, "  Fix: grant the role, or run the write through Entra PowerShell (Connect-Entra -Scopes AgentIdentity.Create.All), then retry.")
			return 1
		}
		fmt.Fprintf(stderr, "mandat provision: ensure agent identity %q: %v\n", displayName, err)
		return 1
	}

	if created {
		fmt.Fprintf(stdout, "created agent identity %q (id %s).\n", identity.DisplayName, identity.ID)
	} else {
		fmt.Fprintf(stdout, "reused existing agent identity %q (id %s); no write issued.\n", identity.DisplayName, identity.ID)
	}
	return 0
}

// runEnsureBlueprint runs US-0014's ensure-blueprint write (AC-14.2). It resolves
// the sponsoring human(s), prints the exact create POST(s) — the blueprint and
// its principal, full bodies including the sponsor @odata.bind list — before
// issuing them (AC-14.7), then, unless dryRun, ensures the single installation
// blueprint exists idempotently (list-then-create) and reports created-vs-reused
// with the appId config.yaml records. A *PrivilegeError (a 403 on either write —
// the blueprint create needs the Agent ID Developer or Administrator role) prints
// the fail-with-guidance line and exits non-zero-but-clean, never a raw Graph 403
// dump (AC-14.2/AC-14.4).
func runEnsureBlueprint(ctx context.Context, client *entra.Client, displayName, sponsorFlag, accountID string, dryRun bool, stdout, stderr io.Writer) int {
	sponsors, err := resolveSponsors(ctx, sponsorFlag, accountID)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: resolve sponsor: %v\n", err)
		return 1
	}

	blueprintCall, err := client.BlueprintCreateCall(displayName, sponsors)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: %v\n", err)
		return 1
	}
	// The principal binds to the appId the blueprint create returns — unknown
	// until that first POST runs — so the preview prints a placeholder appId.
	principalCall, err := client.BlueprintPrincipalCreateCall("<blueprint-appId-from-the-create-above>")
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: %v\n", err)
		return 1
	}

	// AC-14.7: print the exact mutations before issuing. Both POSTs are sent only
	// when no blueprint exists; on a reuse neither is sent.
	for _, call := range []entra.WriteCall{blueprintCall, principalCall} {
		fmt.Fprintf(stdout, "WRITE (issued only if no blueprint exists): %s %s\n", call.Method, call.Endpoint)
		fmt.Fprintf(stdout, "    body: %s\n", call.Body)
	}

	if dryRun {
		existing, err := client.ListBlueprints(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "mandat provision: list blueprints: %v\n", err)
			return 1
		}
		if len(existing) > 0 {
			fmt.Fprintf(stdout, "PLAN (dry-run, no write): a blueprint already exists (appId %s); ensure would reuse it and issue zero writes.\n", existing[0].AppID)
			return 0
		}
		fmt.Fprintln(stdout, "PLAN (dry-run, no write): no blueprint exists; ensure would issue the POST(s) above. Issued zero writes.")
		return 0
	}

	bp, created, err := client.EnsureBlueprint(ctx, displayName, sponsors)
	if err != nil {
		var privErr *entra.PrivilegeError
		if errors.As(err, &privErr) {
			fmt.Fprintf(stderr, "mandat provision: %v\n", privErr)
			fmt.Fprintln(stderr, "  Fix: grant the Agent ID Developer or Administrator role, or run the write through Entra PowerShell (Connect-Entra -Scopes AgentIdentityBlueprint.Create), then retry.")
			return 1
		}
		fmt.Fprintf(stderr, "mandat provision: ensure blueprint %q: %v\n", displayName, err)
		return 1
	}

	if created {
		fmt.Fprintf(stdout, "created agent-identity blueprint %q (appId %s).\n", bp.DisplayName, bp.AppID)
	} else {
		fmt.Fprintf(stdout, "reused existing agent-identity blueprint %q (appId %s); no write issued.\n", bp.DisplayName, bp.AppID)
	}
	return 0
}

// blueprintCred names how the blueprint proves itself for a client-credential
// mint: its appId, its tenant, and the reference (env var name or file path) to a
// secret read only at mint time. It never carries the secret value itself —
// config records a reference, not a secret (US-0014 AC-14.8). authorityURL is a
// test seam pointing the mint at a fake STS.
type blueprintCred struct {
	appID        string
	tenant       string
	secretEnv    string
	secretFile   string
	authorityURL string
}

// tokenSource builds the blueprint's client-credential Graph token source from
// the credential reference: exactly one of --blueprint-secret-env or
// --blueprint-secret-file supplies the secret provider. The provider is invoked
// per mint (a rotated secret is picked up without restart) and the value is never
// retained.
func (b blueprintCred) tokenSource() (entra.TokenSource, error) {
	var secret func(ctx context.Context) (string, error)
	switch {
	case b.secretEnv != "" && b.secretFile != "":
		return nil, errors.New("pass only one of --blueprint-secret-env / --blueprint-secret-file")
	case b.secretEnv != "":
		secret = entra.SecretFromEnv(b.secretEnv)
	case b.secretFile != "":
		secret = entra.SecretFromFile(b.secretFile)
	default:
		return nil, errors.New("--ensure-role needs a blueprint secret: --blueprint-secret-env or --blueprint-secret-file")
	}
	return entra.ClientCredentialTokenSource(entra.ClientCredentialConfig{
		TenantID:         b.tenant,
		ClientID:         b.appID,
		Credential:       entra.SecretCredential{Secret: secret},
		AuthorityBaseURL: b.authorityURL,
	})
}

// runEnsureRole runs US-0014's full ensure-role-identity ladder for one role AS
// the blueprint (AC-14.3): it creates the agent identity and its paired agent user
// under the owned blueprint using the blueprint's OWN client-credential token, so
// no operator standing privilege is required — the blueprint acts on its consented
// application permissions (intrinsic CreateAsManager for the identity,
// AgentIdUser.ReadWrite.IdentityParentedBy for the user), proven live against the
// dogfood tenant 2026-07-17. Existence is checked through the operator's delegated
// discovery client (idempotency), the writes go through the client-credential
// client, and the user create carries the retry-backoff over the create→use lag
// (AC-14.5). Every POST is printed before it is issued (AC-14.7); --dry-run prints
// the plan and issues zero writes (AC-14.6).
func runEnsureRole(ctx context.Context, delegated *entra.Client, cred blueprintCred, graphURL, role, sponsorFlag, accountID, upnDomain string, dryRun bool, stdout, stderr io.Writer) int {
	if cred.appID == "" || cred.tenant == "" {
		fmt.Fprintln(stderr, "mandat provision: --ensure-role needs --blueprint-app-id and --blueprint-tenant (the blueprint mints its own client-credential token).")
		return 1
	}
	if strings.TrimSpace(upnDomain) == "" {
		fmt.Fprintln(stderr, "mandat provision: --ensure-role needs --upn-domain (the verified tenant domain for the agent user's UPN).")
		return 1
	}
	sponsors, err := resolveSponsors(ctx, sponsorFlag, accountID)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: resolve sponsor: %v\n", err)
		return 1
	}
	ccSource, err := cred.tokenSource()
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: %v\n", err)
		return 1
	}
	cc, err := entra.New(entra.Config{GraphBaseURL: graphURL, TokenSource: ccSource})
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: %v\n", err)
		return 1
	}

	// Existence check via the operator's delegated session (the reuse path), so
	// the ladder stays idempotent without asking the blueprint token to list.
	reg, err := delegated.DiscoverRegistry(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: discover Entra Agent-ID registry: %v\n", err)
		return 1
	}

	identity, haveIdentity := entra.AgentIdentity{}, false
	for _, id := range reg.Identities {
		if id.DisplayName == role {
			identity, haveIdentity = id, true
			break
		}
	}

	idCall, err := cc.AgentIdentityCreateCall(cred.appID, role, sponsors)
	if err != nil {
		fmt.Fprintf(stderr, "mandat provision: %v\n", err)
		return 1
	}
	userSpec := entra.AgentUserSpec{DisplayName: role + " user", MailNickname: role, UserPrincipalName: role + "@" + upnDomain}

	// AC-14.7: print both mutations before any is issued.
	fmt.Fprintf(stdout, "WRITE (identity, issued only if absent, AS the blueprint): %s %s\n    body: %s\n", idCall.Method, idCall.Endpoint, idCall.Body)
	planParent := identity.ID
	if !haveIdentity {
		planParent = "<agent-identity-id-from-the-identity-create-above>"
	}
	planSpec := userSpec
	planSpec.IdentityID = planParent
	if userCall, err := cc.AgentUserCreateCall(planSpec); err == nil {
		fmt.Fprintf(stdout, "WRITE (user, issued only if absent, AS the blueprint): %s %s\n    body: %s\n", userCall.Method, userCall.Endpoint, userCall.Body)
	}

	if dryRun {
		if haveIdentity {
			fmt.Fprintf(stdout, "PLAN (dry-run, no write): identity %q exists (id %s); ensure would reuse it.\n", role, identity.ID)
		} else {
			fmt.Fprintf(stdout, "PLAN (dry-run, no write): identity %q absent; ensure would issue the identity POST above.\n", role)
		}
		fmt.Fprintln(stdout, "PLAN (dry-run, no write): the user POST above follows once the identity id is known. Issued zero writes.")
		return 0
	}

	if !haveIdentity {
		identity, err = cc.CreateAgentIdentity(ctx, cred.appID, role, sponsors)
		if err != nil {
			return handleWriteErr(stderr, fmt.Sprintf("create identity %q", role), err,
				"the blueprint client-credential could not create the identity; confirm the blueprint appId/tenant/secret and that the app is an agent-identity blueprint principal.")
		}
		fmt.Fprintf(stdout, "created agent identity %q (id %s) AS the blueprint.\n", identity.DisplayName, identity.ID)
	} else {
		fmt.Fprintf(stdout, "reused existing agent identity %q (id %s); no write issued.\n", identity.DisplayName, identity.ID)
	}

	for _, u := range reg.Users {
		if u.IdentityParentID != "" && u.IdentityParentID == identity.ID {
			fmt.Fprintf(stdout, "reused existing agent user %q (id %s); no write issued.\n", u.UserPrincipalName, u.ID)
			return 0
		}
	}

	userSpec.IdentityID = identity.ID
	user, err := cc.CreateAgentUser(ctx, userSpec, entra.RetryPolicy{})
	if err != nil {
		return handleWriteErr(stderr, fmt.Sprintf("create user %q", userSpec.UserPrincipalName), err,
			"consent AgentIdUser.ReadWrite.IdentityParentedBy (appRole 4aa6e624-eee0-40ab-bdd8-f9639038a614) on the blueprint via admin-consent, then retry.")
	}
	fmt.Fprintf(stdout, "created agent user %q (id %s) parented to the identity AS the blueprint.\n", user.UserPrincipalName, user.ID)
	return 0
}

// handleWriteErr renders a failed Graph write: a *PrivilegeError (403) prints the
// typed message plus an actionable consent/fix hint and exits non-zero-but-clean
// (never a raw 403 dump, AC-14.4); any other error prints what failed.
func handleWriteErr(stderr io.Writer, what string, err error, fixHint string) int {
	var privErr *entra.PrivilegeError
	if errors.As(err, &privErr) {
		fmt.Fprintf(stderr, "mandat provision: %v\n", privErr)
		fmt.Fprintf(stderr, "  Fix: %s\n", fixHint)
		return 1
	}
	fmt.Fprintf(stderr, "mandat provision: %s: %v\n", what, err)
	return 1
}

// resolveSponsors returns the sponsor object ids for a created agent identity:
// the explicit --sponsor ids (comma-separated) when set, else the single
// signed-in az user from deriveSponsor. The owner requires sponsor ids be
// overridable (US-0014), so the flag wins over the derived default.
func resolveSponsors(ctx context.Context, sponsorFlag, accountID string) ([]string, error) {
	if strings.TrimSpace(sponsorFlag) != "" {
		var sponsors []string
		for _, p := range strings.Split(sponsorFlag, ",") {
			if id := strings.TrimSpace(p); id != "" {
				sponsors = append(sponsors, id)
			}
		}
		if len(sponsors) == 0 {
			return nil, fmt.Errorf("--sponsor %q listed no non-empty id", sponsorFlag)
		}
		return sponsors, nil
	}

	id, err := deriveSponsor(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("derived signed-in user id is empty; pass --sponsor <object-id>")
	}
	return []string{id}, nil
}

// printRegistry renders the reuse report: the blueprint appId and each agent
// identity with its paired agent user, in an aligned az-cli-style table. It
// reports state only and prints no token.
func printRegistry(out io.Writer, reg entra.Registry) {
	fmt.Fprintln(out, "Entra Agent-ID registry (reuse path, nothing created):")
	if reg.Blueprint != nil {
		fmt.Fprintf(out, "  blueprint: %s (appId %s)\n", reg.Blueprint.DisplayName, reg.Blueprint.AppID)
	} else {
		fmt.Fprintln(out, "  blueprint: none found")
	}

	fmt.Fprintln(out, "  agent identities:")
	if len(reg.Identities) == 0 {
		fmt.Fprintln(out, "    (none found)")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "    IDENTITY\tID\tPAIRED USER\tUPN")
	for _, id := range reg.Identities {
		user, ok := reg.PairedUser(id)
		if ok {
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\n", id.DisplayName, id.ID, user.DisplayName, user.UserPrincipalName)
		} else {
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\n", id.DisplayName, id.ID, "(none paired)", "-")
		}
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(out, "mandat provision: render registry: %v\n", err)
	}
}

// printCreatePlan prints the create-plan for whatever the reuse read did not
// find: the blueprint pair (research steps 1–2) when no blueprint exists, and
// the identity/user pair (steps 4–5) for each configured role whose identity is
// absent. Every line is a plan, never a call — provision issues zero writes
// (US-0014 AC-14.6). When everything already exists it says so, since the
// dogfood tenant is already fully provisioned.
func printCreatePlan(out io.Writer, reg entra.Registry) {
	fmt.Fprintln(out)

	planned := 0
	if reg.Blueprint == nil {
		printPlannedCall(out, blueprintCreateCall())
		printPlannedCall(out, blueprintPrincipalCreateCall())
		planned += 2
	}

	blueprintID := "<new-agent-identity-blueprint-id>"
	if reg.Blueprint != nil {
		blueprintID = reg.Blueprint.ID
	}
	for _, role := range provisionRoles {
		if roleProvisioned(reg, role) {
			continue
		}
		printPlannedCall(out, agentIdentityCreateCall(role, blueprintID))
		printPlannedCall(out, agentUserCreateCall(role))
		planned += 2
	}

	if planned == 0 {
		fmt.Fprintf(out, "PLAN (dry-run, no write): blueprint and all desired roles (%s) already exist; nothing to create.\n",
			strings.Join(provisionRoles, ", "))
	}
}

// roleProvisioned reports whether some agent identity's displayName carries
// role's name — the plan-format heuristic for "this role is already
// provisioned". Case-insensitive substring, never a gate.
func roleProvisioned(reg entra.Registry, role string) bool {
	role = strings.ToLower(role)
	for _, id := range reg.Identities {
		if strings.Contains(strings.ToLower(id.DisplayName), role) {
			return true
		}
	}
	return false
}

// plannedCall is one create call the dry run would issue: the research-doc step
// it realizes, the HTTP method and endpoint, and a representative JSON body.
type plannedCall struct {
	step     string
	method   string
	endpoint string
	body     map[string]any
}

func printPlannedCall(out io.Writer, call plannedCall) {
	body, err := json.Marshal(call.body)
	if err != nil {
		body = []byte("{}")
	}
	fmt.Fprintf(out, "PLAN (dry-run, no write): %s %s  (%s)\n", call.method, call.endpoint, call.step)
	fmt.Fprintf(out, "    body: %s\n", body)
}

func blueprintCreateCall() plannedCall {
	return plannedCall{
		step:     "research step 1: create blueprint",
		method:   "POST",
		endpoint: graphPlanBaseURL + "/applications/microsoft.graph.agentIdentityBlueprint",
		body: map[string]any{
			"displayName": "mandat-blueprint",
			"sponsors":    []string{"<operator-object-id>"},
		},
	}
}

func blueprintPrincipalCreateCall() plannedCall {
	return plannedCall{
		step:     "research step 2: create blueprint principal",
		method:   "POST",
		endpoint: graphPlanBaseURL + "/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal",
		body: map[string]any{
			"appId": "<blueprint-appId-from-step-1>",
		},
	}
}

func agentIdentityCreateCall(role, blueprintID string) plannedCall {
	return plannedCall{
		step:     "research step 4: create agent identity under the owned blueprint",
		method:   "POST",
		endpoint: graphPlanBaseURL + "/servicePrincipals/microsoft.graph.agentIdentity",
		body: map[string]any{
			"displayName":              "mandat-" + role,
			"agentIdentityBlueprintId": blueprintID,
		},
	}
}

func agentUserCreateCall(role string) plannedCall {
	return plannedCall{
		step:     "research step 5: create paired agent user",
		method:   "POST",
		endpoint: graphPlanBaseURL + "/users/microsoft.graph.agentUser",
		body: map[string]any{
			"accountEnabled":    true,
			"displayName":       "mandat-" + role + "-user",
			"mailNickname":      "mandat-" + role,
			"userPrincipalName": "mandat-" + role + "@<verified-tenant-domain>",
			"identityParentId":  "<agent-identity-id-from-step-4>",
		},
	}
}
