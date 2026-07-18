package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/baodq97/mandat/internal/entra"
)

// provision's tests run against fakeGraphServer, an httptest.Server replaying
// the probed Agent-ID read shapes, with the az token source swapped for a canned
// one — no az shellout, no live Graph, and the fake records every method so a
// test proves the reuse path (and its dry-run plan) issues only GETs.

const fakeGraphToken = "fake-graph-token"

// fakeGraphServer serves the three read endpoints and records every request's
// method, so a test asserts the whole command — reads and dry-run plan — writes
// nothing.
type fakeGraphServer struct {
	mu       sync.Mutex
	methods  []string
	postBody string
	posts    []recordedPost

	blueprintsBody string
	identitiesBody string
	usersBody      string

	createStatus int
	createBody   string
	deleteStatus int
	deleteBody   string

	userCreateStatus int
	userCreateBody   string

	spByAppIDBody     string // GET servicePrincipals(appId='...') — the ADO SP resolve (step 6)
	grantsBody        string // GET oauth2PermissionGrants?$filter=... — existing grants (step 6 idempotency)
	grantCreateStatus int    // status for POST oauth2PermissionGrants (0 → 201)

	blueprintCreateStatus int
	blueprintCreateBody   string
	principalCreateStatus int
	principalCreateBody   string
}

// recordedPost is one POST the fake saw — its path, body, and Authorization
// header — so a test that issues more than one write (ensure-blueprint POSTs the
// blueprint then its principal; ensure-role POSTs the identity then the user) can
// assert each body against its endpoint, and prove the ensure-role writes carry
// the blueprint's client-credential token, not the delegated discovery token.
type recordedPost struct {
	path  string
	body  string
	authz string
}

func (f *fakeGraphServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.methods = append(f.methods, r.Method)
		if r.Method == http.MethodPost {
			f.postBody = string(reqBody)
			f.posts = append(f.posts, recordedPost{path: r.URL.Path, body: string(reqBody), authz: r.Header.Get("Authorization")})
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/applications/microsoft.graph.agentIdentityBlueprint"):
			w.WriteHeader(f.blueprintCreateStatus)
			_, _ = w.Write([]byte(f.blueprintCreateBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal"):
			w.WriteHeader(f.principalCreateStatus)
			_, _ = w.Write([]byte(f.principalCreateBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/users/microsoft.graph.agentUser"):
			w.WriteHeader(f.userCreateStatus)
			_, _ = w.Write([]byte(f.userCreateBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/servicePrincipals/microsoft.graph.agentIdentity"):
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(f.createBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/oauth2PermissionGrants"):
			st := f.grantCreateStatus
			if st == 0 {
				st = http.StatusCreated
			}
			w.WriteHeader(st)
		case strings.HasSuffix(r.URL.Path, "/oauth2PermissionGrants"):
			body := f.grantsBody
			if body == "" {
				body = `{"value":[]}`
			}
			_, _ = w.Write([]byte(body))
		case strings.Contains(r.URL.Path, "servicePrincipals(appId="):
			if f.spByAppIDBody == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(f.spByAppIDBody))
		case r.Method == http.MethodDelete:
			w.WriteHeader(f.deleteStatus)
			_, _ = w.Write([]byte(f.deleteBody))
		case strings.HasSuffix(r.URL.Path, "/applications/microsoft.graph.agentIdentityBlueprint"):
			_, _ = w.Write([]byte(f.blueprintsBody))
		case strings.HasSuffix(r.URL.Path, "/servicePrincipals/microsoft.graph.agentIdentity"):
			_, _ = w.Write([]byte(f.identitiesBody))
		case strings.HasSuffix(r.URL.Path, "/users/microsoft.graph.agentUser"):
			_, _ = w.Write([]byte(f.usersBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGraphServer) recordedMethods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.methods))
	copy(out, f.methods)
	return out
}

func (f *fakeGraphServer) recordedPostBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postBody
}

func (f *fakeGraphServer) recordedPosts() []recordedPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedPost, len(f.posts))
	copy(out, f.posts)
	return out
}

// dogfoodGraphServer seeds a blueprint and a dev identity paired to a dev user,
// but no reviewer identity — so the reuse report lists a paired identity and the
// dry-run plan has a genuinely-absent role to plan the create for.
func dogfoodGraphServer() *fakeGraphServer {
	return &fakeGraphServer{
		blueprintsBody: `{"value":[{"id":"bp-object-01","appId":"appid-blueprint-01","displayName":"mandat-spike-blueprint"}]}`,
		identitiesBody: `{"value":[{"id":"identity-dev-01","displayName":"mandat-spike-dev"}]}`,
		usersBody:      `{"value":[{"id":"user-dev-01","displayName":"mandat-spike-dev-user","userPrincipalName":"dev@baotest.onmicrosoft.com","identityParentId":"identity-dev-01"}]}`,
		spByAppIDBody:  `{"id":"ado-sp-object-01","appId":"499b84ac-1321-427f-aa17-267ca6975798"}`,
	}
}

// swapGraphTokenSource installs src as the token source provision's factory
// returns for the test's duration, and stubs the az-account derivation so building
// the pinned client needs no az session. A test that swaps it runs non-parallel:
// graphTokenSource and deriveProvisionAccount are package state and -race rejects
// a concurrent write.
func swapGraphTokenSource(t *testing.T, src entra.TokenSource) {
	t.Helper()
	savedFactory := graphTokenSource
	graphTokenSource = func(string) entra.TokenSource { return src }
	t.Cleanup(func() { graphTokenSource = savedFactory })

	savedAccount := deriveProvisionAccount
	deriveProvisionAccount = func(context.Context) (string, error) { return "test-account", nil }
	t.Cleanup(func() { deriveProvisionAccount = savedAccount })
}

// swapProvisionAccount installs fn as provision's az-account-derivation seam for
// the test's duration, so a test drives the default-derive path with no az
// shellout. Non-parallel for the same reason as swapGraphTokenSource.
func swapProvisionAccount(t *testing.T, fn func(context.Context) (string, error)) {
	t.Helper()
	saved := deriveProvisionAccount
	deriveProvisionAccount = fn
	t.Cleanup(func() { deriveProvisionAccount = saved })
}

// swapDeriveSponsor installs fn as provision's sponsor derivation for the test's
// duration, so an ensure test resolves the default sponsor with no az shellout.
// Like swapGraphTokenSource it mutates package state, so its callers run
// non-parallel.
func swapDeriveSponsor(t *testing.T, fn func(context.Context, string) (string, error)) {
	t.Helper()
	saved := deriveSponsor
	deriveSponsor = fn
	t.Cleanup(func() { deriveSponsor = saved })
}

func TestProvision_ReuseReport_ListsBlueprintAndPairedIdentity(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })

	fake := dogfoodGraphServer()
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	// The report names the blueprint appId, the dev identity (displayName +
	// id), and its paired agent user's UPN.
	out := stdout.String()
	for _, want := range []string{
		"appid-blueprint-01",
		"mandat-spike-dev",
		"identity-dev-01",
		"dev@baotest.onmicrosoft.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}

	// The reuse path prints no create-plan without --dry-run.
	if strings.Contains(out, "PLAN (dry-run, no write):") {
		t.Errorf("report contains a PLAN line without --dry-run\n%s", out)
	}

	// The bearer token never appears in the operator-facing output.
	if strings.Contains(out, fakeGraphToken) {
		t.Error("report leaked the Graph token")
	}
}

func TestProvision_DryRun_PrintsPlanForAbsentRoleAndIssuesNoWrites(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })

	fake := dogfoodGraphServer()
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--dry-run", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "PLAN (dry-run, no write):") {
		t.Fatalf("--dry-run printed no PLAN line\n%s", out)
	}
	// The reviewer identity is absent, so the plan covers its step-4 identity
	// and step-5 user creates.
	for _, want := range []string{
		"/servicePrincipals/microsoft.graph.agentIdentity",
		"/users/microsoft.graph.agentUser",
		"mandat-reviewer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q\n%s", want, out)
		}
	}
	// The blueprint and dev identity already exist, so neither is planned.
	if strings.Contains(out, "research step 1: create blueprint") {
		t.Errorf("plan proposed creating a blueprint that already exists\n%s", out)
	}

	// Every request the whole dry run made is a GET: the plan is printed, never
	// issued (US-0014 AC-14.6).
	methods := fake.recordedMethods()
	if len(methods) == 0 {
		t.Fatal("no requests recorded; the reuse read never ran")
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("recorded a %s request; --dry-run must issue only GETs", m)
		}
	}
}

func TestProvision_TokenSourceFailure_ExitsNonZero(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	})

	fake := dogfoodGraphServer()
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--graph-url", srv.URL}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("provision() code = 0, want non-zero when the token cannot be minted (stderr: %s)", stderr.String())
	}
	if !strings.Contains(stderr.String(), "mandat provision:") {
		t.Errorf("stderr = %q, want a mandat provision diagnostic", stderr.String())
	}
}

func TestProvision_EnsureIdentity_ReusesExisting(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-identity", "mandat-spike-dev", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "reused") || !strings.Contains(out, "identity-dev-01") {
		t.Errorf("output missing the reuse report for the existing identity\n%s", out)
	}
	// Reuse issues no POST: the fake records only GET (the list read).
	for _, m := range fake.recordedMethods() {
		if m != http.MethodGet {
			t.Errorf("recorded a %s request; reuse must issue only GETs", m)
		}
	}
	if strings.Contains(out, fakeGraphToken) {
		t.Error("output leaked the Graph token")
	}
}

func TestProvision_EnsureIdentity_CreatesAbsent(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	fake.createStatus = http.StatusCreated
	fake.createBody = `{"id":"identity-pilot-99","displayName":"mandat-pilot"}`
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-identity", "mandat-pilot", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "created") || !strings.Contains(out, "identity-pilot-99") {
		t.Errorf("output missing the created-identity report\n%s", out)
	}
	// The absent-identity path issues the create POST.
	sawPost := false
	for _, m := range fake.recordedMethods() {
		if m == http.MethodPost {
			sawPost = true
		}
	}
	if !sawPost {
		t.Errorf("no POST recorded; the absent identity must be created (methods: %v)", fake.recordedMethods())
	}

	// The POST body carries the full v1.0 shape: displayName, the discovered
	// blueprint id (dogfoodGraphServer's bp-object-01), and a non-empty
	// sponsors@odata.bind for the derived signed-in user.
	body := fake.recordedPostBody()
	for _, want := range []string{
		`"displayName":"mandat-pilot"`,
		`"agentIdentityBlueprintId":"bp-object-01"`,
		`"sponsors@odata.bind":[`,
		"/users/sponsor-signed-in",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("POST body missing %q\n%s", want, body)
		}
	}
}

func TestProvision_EnsureIdentity_DryRun_PrintsPlanAndIssuesNoWrites(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-identity", "mandat-pilot", "--dry-run", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	// The exact POST is printed (endpoint + full body: displayName, blueprint id,
	// sponsors@odata.bind) and marked no-write (AC-14.7).
	for _, want := range []string{
		"/servicePrincipals/microsoft.graph.agentIdentity",
		`"displayName":"mandat-pilot"`,
		`"agentIdentityBlueprintId":"bp-object-01"`,
		"/users/sponsor-signed-in",
		"no write",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n%s", want, out)
		}
	}
	// Zero writes: every request the dry run made is a GET (the list read that
	// predicts create-vs-reuse).
	methods := fake.recordedMethods()
	if len(methods) == 0 {
		t.Fatal("no requests recorded; dry-run must still read to predict create-vs-reuse")
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("recorded a %s request; --dry-run must issue only GETs", m)
		}
	}
}

func TestProvision_EnsureIdentity_Forbidden_PrintsGuidanceExitsNonZero(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	fake.createStatus = http.StatusForbidden
	fake.createBody = `{"error":{"code":"Authorization_RequestDenied"}}`
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-identity", "mandat-pilot", "--graph-url", srv.URL}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("provision() code = 0, want non-zero on a 403 write (stdout: %s)", stdout.String())
	}

	// Fail-with-guidance (AC-14.4): the operator sees the missing capability and
	// a fix, not a raw 403 dump.
	errOut := stderr.String()
	for _, want := range []string{"403", "Agent ID Developer", "AgentIdentity.Create.All"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("guidance missing %q\n%s", want, errOut)
		}
	}
	if strings.Contains(stdout.String(), fakeGraphToken) || strings.Contains(errOut, fakeGraphToken) {
		t.Error("output leaked the Graph token")
	}
}

func TestProvision_EnsureIdentity_SponsorOverride(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	// deriveSponsor errors so that a code=0 run proves --sponsor took over: if the
	// flag were ignored the derivation would fail and abort non-zero.
	swapDeriveSponsor(t, func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	})

	fake := dogfoodGraphServer()
	fake.createStatus = http.StatusCreated
	fake.createBody = `{"id":"identity-pilot-99","displayName":"mandat-pilot"}`
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-identity", "mandat-pilot", "--sponsor", "sp-a,sp-b", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (--sponsor must bypass deriveSponsor) (stderr: %s)", code, stderr.String())
	}

	// The explicit ids — both of them — bind into the POST body, not the derived
	// default (which would have errored).
	body := fake.recordedPostBody()
	for _, want := range []string{"/users/sp-a", "/users/sp-b"} {
		if !strings.Contains(body, want) {
			t.Errorf("POST body missing overridden sponsor %q\n%s", want, body)
		}
	}
}

func TestProvision_EnsureIdentity_NoBlueprint_Errors(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	fake.blueprintsBody = `{"value":[]}`
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-identity", "mandat-pilot", "--graph-url", srv.URL}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("provision() code = 0, want non-zero when no blueprint exists (stdout: %s)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "blueprint") {
		t.Errorf("stderr = %q, want a missing-blueprint diagnostic", stderr.String())
	}
	// No write is attempted when the prerequisite blueprint is absent.
	for _, m := range fake.recordedMethods() {
		if m == http.MethodPost {
			t.Errorf("recorded a POST with no blueprint; the create must not run")
		}
	}
}

// TestProvision_Subscription_PinsGraphMintAndSponsorDerive proves --subscription
// pins both az mints provision makes: the Graph token-source factory and the
// sponsor lookup receive the flag value, so neither falls back to az's active
// account (US-0014 F1). Not parallel: swaps package-var seams.
func TestProvision_Subscription_PinsGraphMintAndSponsorDerive(t *testing.T) {
	var gotGraphAccount, gotSponsorAccount string
	savedFactory := graphTokenSource
	graphTokenSource = func(accountID string) entra.TokenSource {
		gotGraphAccount = accountID
		return func(context.Context) (string, error) { return fakeGraphToken, nil }
	}
	t.Cleanup(func() { graphTokenSource = savedFactory })
	swapDeriveSponsor(t, func(_ context.Context, accountID string) (string, error) {
		gotSponsorAccount = accountID
		return "sponsor-signed-in", nil
	})

	fake := dogfoodGraphServer()
	fake.createStatus = http.StatusCreated
	fake.createBody = `{"id":"identity-pilot-99","displayName":"mandat-pilot"}`
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-identity", "mandat-pilot", "--subscription", "chosen-account", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if gotGraphAccount != "chosen-account" {
		t.Errorf("Graph mint pinned to account %q, want the --subscription value %q", gotGraphAccount, "chosen-account")
	}
	if gotSponsorAccount != "chosen-account" {
		t.Errorf("sponsor derive pinned to account %q, want the --subscription value %q", gotSponsorAccount, "chosen-account")
	}
}

// TestProvision_NoSubscriptionFlag_DerivesAccount proves the default: with no
// --subscription, provision derives the active az account (deriveProvisionAccount)
// and pins the Graph mint to it rather than leaving it unpinned. Not parallel:
// swaps package-var seams.
func TestProvision_NoSubscriptionFlag_DerivesAccount(t *testing.T) {
	swapProvisionAccount(t, func(context.Context) (string, error) { return "derived-account", nil })
	var gotGraphAccount string
	savedFactory := graphTokenSource
	graphTokenSource = func(accountID string) entra.TokenSource {
		gotGraphAccount = accountID
		return func(context.Context) (string, error) { return fakeGraphToken, nil }
	}
	t.Cleanup(func() { graphTokenSource = savedFactory })

	fake := dogfoodGraphServer()
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if gotGraphAccount != "derived-account" {
		t.Errorf("Graph mint pinned to account %q, want the derived default %q", gotGraphAccount, "derived-account")
	}
}

// TestProvision_AccountDeriveFails_ExitsNonZero proves provision refuses to run
// with no resolvable account and no --subscription, rather than minting an
// unpinned Graph token against az's active account (US-0014 F1). Not parallel:
// swaps a package-var seam.
func TestProvision_AccountDeriveFails_ExitsNonZero(t *testing.T) {
	swapProvisionAccount(t, func(context.Context) (string, error) { return "", errors.New("az not logged in") })

	var stdout, stderr strings.Builder
	code := provision([]string{}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("provision() code = 0, want non-zero when no account resolves and none is passed (stdout: %s)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "account") {
		t.Errorf("stderr = %q, want an account-resolution diagnostic", stderr.String())
	}
}

func TestProvision_EnsureBlueprint_ReusesExisting(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-blueprint", "mandat-blueprint", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "reused") || !strings.Contains(out, "appid-blueprint-01") {
		t.Errorf("output missing the reuse report for the existing blueprint\n%s", out)
	}
	// Reuse issues no POST: the fake records only GET (the blueprint list read).
	for _, m := range fake.recordedMethods() {
		if m != http.MethodGet {
			t.Errorf("recorded a %s request; reuse must issue only GETs", m)
		}
	}
	if strings.Contains(out, fakeGraphToken) {
		t.Error("output leaked the Graph token")
	}
}

func TestProvision_EnsureBlueprint_CreatesAbsent(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	fake.blueprintsBody = `{"value":[]}`
	fake.blueprintCreateStatus = http.StatusCreated
	fake.blueprintCreateBody = `{"id":"bp-object-99","appId":"appid-blueprint-99","displayName":"mandat-blueprint"}`
	fake.principalCreateStatus = http.StatusCreated
	fake.principalCreateBody = `{"id":"principal-99","appId":"appid-blueprint-99"}`
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-blueprint", "mandat-blueprint", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "created") || !strings.Contains(out, "appid-blueprint-99") {
		t.Errorf("output missing the created-blueprint report\n%s", out)
	}

	// The absent-blueprint path issues both the blueprint POST (displayName +
	// sponsors@odata.bind for the derived signed-in user) and the principal POST
	// (just the appId the blueprint create returned).
	posts := fake.recordedPosts()
	blueprintBody := postBodyForSuffix(t, posts, "/applications/microsoft.graph.agentIdentityBlueprint")
	for _, want := range []string{
		`"displayName":"mandat-blueprint"`,
		`"sponsors@odata.bind":[`,
		"/users/sponsor-signed-in",
	} {
		if !strings.Contains(blueprintBody, want) {
			t.Errorf("blueprint POST body missing %q\n%s", want, blueprintBody)
		}
	}
	principalBody := postBodyForSuffix(t, posts, "/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal")
	if !strings.Contains(principalBody, `"appId":"appid-blueprint-99"`) {
		t.Errorf("principal POST body missing the appId from the blueprint create\n%s", principalBody)
	}
}

func TestProvision_EnsureBlueprint_Forbidden_PrintsGuidanceExitsNonZero(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	fake.blueprintsBody = `{"value":[]}`
	fake.blueprintCreateStatus = http.StatusForbidden
	fake.blueprintCreateBody = `{"error":{"code":"Authorization_RequestDenied"}}`
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-blueprint", "mandat-blueprint", "--graph-url", srv.URL}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("provision() code = 0, want non-zero on a 403 write (stdout: %s)", stdout.String())
	}

	// Fail-with-guidance (AC-14.2/AC-14.4): the operator sees the 403, the missing
	// Agent ID role, and the blueprint-scope fix, not a raw 403 dump.
	errOut := stderr.String()
	for _, want := range []string{"403", "Agent ID Developer", "AgentIdentityBlueprint.Create"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("guidance missing %q\n%s", want, errOut)
		}
	}
	if strings.Contains(stdout.String(), fakeGraphToken) || strings.Contains(errOut, fakeGraphToken) {
		t.Error("output leaked the Graph token")
	}
}

func TestProvision_EnsureBlueprint_DryRun_PrintsPlanAndIssuesNoWrites(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })

	fake := dogfoodGraphServer()
	fake.blueprintsBody = `{"value":[]}`
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-blueprint", "mandat-blueprint", "--dry-run", "--graph-url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	// Both exact POSTs are printed (endpoints + bodies) and marked no-write (AC-14.7).
	for _, want := range []string{
		"/applications/microsoft.graph.agentIdentityBlueprint",
		"/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal",
		`"displayName":"mandat-blueprint"`,
		"/users/sponsor-signed-in",
		"no write",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n%s", want, out)
		}
	}
	// Zero writes: every request the dry run made is a GET (the blueprint list read
	// that predicts create-vs-reuse).
	methods := fake.recordedMethods()
	if len(methods) == 0 {
		t.Fatal("no requests recorded; dry-run must still read to predict create-vs-reuse")
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("recorded a %s request; --dry-run must issue only GETs", m)
		}
	}
}

func postBodyForSuffix(t *testing.T, posts []recordedPost, suffix string) string {
	t.Helper()
	for _, p := range posts {
		if strings.HasSuffix(p.path, suffix) {
			return p.body
		}
	}
	t.Fatalf("no recorded POST with path suffix %q", suffix)
	return ""
}

// fakeSTS is a minimal Entra STS: it replies to the client-credentials token
// request with a canned access_token, so an --ensure-role test proves the write
// path mints AS the blueprint with no login.microsoftonline.com dial.
func fakeSTS(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + token + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const blueprintCCToken = "blueprint-cc-token"

func TestProvision_EnsureRole_CreatesIdentityAndUserAsBlueprint(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })
	t.Setenv("MANDAT_TEST_BP_SECRET", "s3cr3t")

	fake := dogfoodGraphServer() // has mandat-spike-dev, so role "mandat-dev" is genuinely absent
	fake.createStatus = http.StatusCreated
	fake.createBody = `{"id":"identity-new-01","displayName":"mandat-dev"}`
	fake.userCreateStatus = http.StatusCreated
	fake.userCreateBody = `{"id":"user-new-01","displayName":"mandat-dev user","userPrincipalName":"mandat-dev@contoso.onmicrosoft.com","identityParentId":"identity-new-01"}`
	srv := fake.start(t)
	sts := fakeSTS(t, blueprintCCToken)

	var stdout, stderr strings.Builder
	code := provision([]string{
		"--ensure-role", "mandat-dev",
		"--blueprint-app-id", "blueprint-appid-01",
		"--blueprint-tenant", "tenant-01",
		"--blueprint-secret-env", "MANDAT_TEST_BP_SECRET",
		"--upn-domain", "contoso.onmicrosoft.com",
		"--graph-url", srv.URL,
		"--authority-url", sts.URL,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"created agent identity", "identity-new-01", "created agent user", "user-new-01",
		"granted ADO impersonation", "STEP 7 (ADO org admin", "vsaex.dev.azure.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}

	// Three writes: identity + user AS the blueprint (client-cred token), the ADO
	// impersonation grant on the operator's delegated session — admin consent is the
	// operator's act, creation is the blueprint's. The writer≠scorer IAM split, in code.
	posts := fake.recordedPosts()
	if len(posts) != 3 {
		t.Fatalf("POSTs = %d, want 3 (identity, user, ADO grant)", len(posts))
	}
	for _, p := range posts {
		if strings.HasSuffix(p.path, "/oauth2PermissionGrants") {
			if p.authz != "Bearer "+fakeGraphToken {
				t.Errorf("grant POST authz = %q, want the delegated operator token (admin consent is the operator's)", p.authz)
			}
			continue
		}
		if p.authz != "Bearer "+blueprintCCToken {
			t.Errorf("POST %s authz = %q, want the blueprint client-cred token", p.path, p.authz)
		}
	}

	idBody := postBodyBySuffix(t, posts, "/servicePrincipals/microsoft.graph.agentIdentity")
	if !strings.Contains(idBody, `"agentIdentityBlueprintId":"blueprint-appid-01"`) {
		t.Errorf("identity POST body missing the blueprint appId from --blueprint-app-id\n%s", idBody)
	}
	userBody := postBodyBySuffix(t, posts, "/users/microsoft.graph.agentUser")
	for _, want := range []string{`"userPrincipalName":"mandat-dev@contoso.onmicrosoft.com"`, `"identityParentId":"identity-new-01"`} {
		if !strings.Contains(userBody, want) {
			t.Errorf("user POST body missing %q\n%s", want, userBody)
		}
	}
	grantBody := postBodyBySuffix(t, posts, "/oauth2PermissionGrants")
	for _, want := range []string{
		`"consentType":"Principal"`, `"scope":"user_impersonation"`,
		`"clientId":"identity-new-01"`, `"principalId":"user-new-01"`, `"resourceId":"ado-sp-object-01"`,
	} {
		if !strings.Contains(grantBody, want) {
			t.Errorf("grant POST body missing %q\n%s", want, grantBody)
		}
	}
}

func TestProvision_EnsureRole_ReusesExistingWithoutWrite(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })
	t.Setenv("MANDAT_TEST_BP_SECRET", "s3cr3t")

	// A registry where role "mandat-dev", its paired user, and the ADO grant all
	// already exist — the full-reuse path issues zero writes across all steps.
	fake := &fakeGraphServer{
		blueprintsBody: `{"value":[{"id":"bp-object-01","appId":"appid-blueprint-01","displayName":"mandat-spike-blueprint"}]}`,
		identitiesBody: `{"value":[{"id":"identity-dev-01","displayName":"mandat-dev"}]}`,
		usersBody:      `{"value":[{"id":"user-dev-01","displayName":"mandat-dev user","userPrincipalName":"mandat-dev@contoso.onmicrosoft.com","identityParentId":"identity-dev-01"}]}`,
		spByAppIDBody:  `{"id":"ado-sp-object-01","appId":"499b84ac-1321-427f-aa17-267ca6975798"}`,
		grantsBody:     `{"value":[{"resourceId":"ado-sp-object-01"}]}`,
	}
	srv := fake.start(t)
	sts := fakeSTS(t, blueprintCCToken)

	var stdout, stderr strings.Builder
	code := provision([]string{
		"--ensure-role", "mandat-dev",
		"--blueprint-app-id", "blueprint-appid-01",
		"--blueprint-tenant", "tenant-01",
		"--blueprint-secret-env", "MANDAT_TEST_BP_SECRET",
		"--upn-domain", "contoso.onmicrosoft.com",
		"--graph-url", srv.URL,
		"--authority-url", sts.URL,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"reused existing agent identity", "reused existing agent user", "reused existing ADO impersonation grant"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	for _, m := range fake.recordedMethods() {
		if m == http.MethodPost {
			t.Errorf("issued a POST on the full-reuse path, want none (methods: %v)", fake.recordedMethods())
		}
	}
}

func TestProvision_EnsureRole_DryRun_PrintsPlanAndIssuesNoWrites(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })
	t.Setenv("MANDAT_TEST_BP_SECRET", "s3cr3t")

	fake := dogfoodGraphServer()
	srv := fake.start(t)
	sts := fakeSTS(t, blueprintCCToken)

	var stdout, stderr strings.Builder
	code := provision([]string{
		"--ensure-role", "mandat-dev",
		"--blueprint-app-id", "blueprint-appid-01",
		"--blueprint-tenant", "tenant-01",
		"--blueprint-secret-env", "MANDAT_TEST_BP_SECRET",
		"--upn-domain", "contoso.onmicrosoft.com",
		"--graph-url", srv.URL,
		"--authority-url", sts.URL,
		"--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "WRITE (identity") || !strings.Contains(out, "WRITE (user") {
		t.Errorf("dry-run missing the WRITE previews\n%s", out)
	}
	if !strings.Contains(out, "PLAN (dry-run, no write)") {
		t.Errorf("dry-run missing the PLAN lines\n%s", out)
	}
	for _, m := range fake.recordedMethods() {
		if m == http.MethodPost {
			t.Errorf("dry-run issued a POST, want none (methods: %v)", fake.recordedMethods())
		}
	}
}

func TestProvision_EnsureRole_Step6Forbidden_PrintsAdminCallAndContinues(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })
	t.Setenv("MANDAT_TEST_BP_SECRET", "s3cr3t")

	fake := dogfoodGraphServer()
	fake.createStatus = http.StatusCreated
	fake.createBody = `{"id":"identity-new-01","displayName":"mandat-dev"}`
	fake.userCreateStatus = http.StatusCreated
	fake.userCreateBody = `{"id":"user-new-01","displayName":"mandat-dev user","userPrincipalName":"mandat-dev@contoso.onmicrosoft.com","identityParentId":"identity-new-01"}`
	fake.grantCreateStatus = http.StatusForbidden // operator lacks admin-consent rights for step 6
	srv := fake.start(t)
	sts := fakeSTS(t, blueprintCCToken)

	var stdout, stderr strings.Builder
	code := provision([]string{
		"--ensure-role", "mandat-dev",
		"--blueprint-app-id", "blueprint-appid-01",
		"--blueprint-tenant", "tenant-01",
		"--blueprint-secret-env", "MANDAT_TEST_BP_SECRET",
		"--upn-domain", "contoso.onmicrosoft.com",
		"--ado-org", "contoso-eng",
		"--graph-url", srv.URL,
		"--authority-url", sts.URL,
	}, &stdout, &stderr)
	// AC-14.4: a privilege gap on step 6 does not abort — identity + user still created.
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (a step-6 privilege gap must not abort the ladder); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created agent user") {
		t.Errorf("identity/user creation should have completed before the step-6 gap\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "a tenant admin must run") {
		t.Errorf("stderr missing the fail-with-guidance admin call for step 6\n%s", stderr.String())
	}
	// --ado-org renders step 7's entitlement URL for the admin.
	if !strings.Contains(stdout.String(), "vsaex.dev.azure.com/contoso-eng/") {
		t.Errorf("step 7 output missing --ado-org in the entitlement URL\n%s", stdout.String())
	}
}

func TestProvision_EnsureRole_MissingSecret_Errors(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })

	fake := dogfoodGraphServer()
	srv := fake.start(t)

	var stdout, stderr strings.Builder
	code := provision([]string{
		"--ensure-role", "mandat-dev",
		"--blueprint-app-id", "blueprint-appid-01",
		"--blueprint-tenant", "tenant-01",
		"--upn-domain", "contoso.onmicrosoft.com",
		"--sponsor", "sponsor-explicit",
		"--graph-url", srv.URL,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("provision() code = 0, want non-zero when no blueprint secret is given")
	}
	if !strings.Contains(stderr.String(), "blueprint secret") {
		t.Errorf("stderr missing the missing-secret guidance\n%s", stderr.String())
	}
}

func TestProvision_EnsureRole_UserForbidden_PrintsConsentGuidance(t *testing.T) {
	swapGraphTokenSource(t, func(context.Context) (string, error) { return fakeGraphToken, nil })
	swapDeriveSponsor(t, func(context.Context, string) (string, error) { return "sponsor-signed-in", nil })
	t.Setenv("MANDAT_TEST_BP_SECRET", "s3cr3t")

	fake := dogfoodGraphServer()
	fake.createStatus = http.StatusCreated
	fake.createBody = `{"id":"identity-new-01","displayName":"mandat-dev"}`
	fake.userCreateStatus = http.StatusForbidden
	fake.userCreateBody = `{"error":{"code":"Authorization_RequestDenied"}}`
	srv := fake.start(t)
	sts := fakeSTS(t, blueprintCCToken)

	var stdout, stderr strings.Builder
	code := provision([]string{
		"--ensure-role", "mandat-dev",
		"--blueprint-app-id", "blueprint-appid-01",
		"--blueprint-tenant", "tenant-01",
		"--blueprint-secret-env", "MANDAT_TEST_BP_SECRET",
		"--upn-domain", "contoso.onmicrosoft.com",
		"--graph-url", srv.URL,
		"--authority-url", sts.URL,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("provision() code = 0, want non-zero on a 403 user create")
	}
	if !strings.Contains(stderr.String(), "AgentIdUser.ReadWrite.IdentityParentedBy") {
		t.Errorf("stderr missing the consent guidance for the user-create permission\n%s", stderr.String())
	}
}

// postBodyBySuffix returns the recorded POST body whose path ends with suffix.
func postBodyBySuffix(t *testing.T, posts []recordedPost, suffix string) string {
	t.Helper()
	for _, p := range posts {
		if strings.HasSuffix(p.path, suffix) {
			return p.body
		}
	}
	t.Fatalf("no recorded POST with path suffix %q", suffix)
	return ""
}

// swapAz installs test doubles for the three az seams ensure-auth uses (az
// detection, account-show, device-code login) for the test's duration. It mutates
// package state, so its callers run non-parallel.
func swapAz(t *testing.T,
	path func() (string, error),
	acct func(context.Context) (azAccount, error),
	login func(context.Context, string, io.Writer, io.Writer) error,
) {
	t.Helper()
	sp, sa, sl := azPath, azLoggedInAccount, azLogin
	if path != nil {
		azPath = path
	}
	if acct != nil {
		azLoggedInAccount = acct
	}
	if login != nil {
		azLogin = login
	}
	t.Cleanup(func() { azPath, azLoggedInAccount, azLogin = sp, sa, sl })
}

func TestProvision_EnsureAuth_AlreadyLoggedIn_ReportsNoNewSession(t *testing.T) {
	loginRan := false
	swapAz(t,
		func() (string, error) { return "/usr/bin/az", nil },
		func(context.Context) (azAccount, error) {
			return azAccount{ID: "acct-01", TenantID: "tenant-01", Name: "baotest"}, nil
		},
		func(context.Context, string, io.Writer, io.Writer) error { loginRan = true; return nil },
	)
	// ensure-auth must dispatch before any account resolution (AC-14.1): a signed-out
	// operator's account resolution would itself fail, which is the state ensure-auth
	// fixes. This spy hard-gates the ordering deterministically on any machine — a
	// reorder that resolves the account first trips it, not just a timing regression.
	accountResolved := false
	swapProvisionAccount(t, func(context.Context) (string, error) {
		accountResolved = true
		return "must-not-be-resolved-for-ensure-auth", nil
	})

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-auth"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if loginRan {
		t.Error("az login was driven while already signed in; AC-14.1 wants no new session")
	}
	if accountResolved {
		t.Error("account resolution ran for --ensure-auth; it must dispatch before resolveProvisionAccount (AC-14.1)")
	}
	out := stdout.String()
	if !strings.Contains(out, "already signed in") || !strings.Contains(out, "acct-01") {
		t.Errorf("output missing the already-signed-in report\n%s", out)
	}
}

func TestProvision_EnsureAuth_LoggedOut_DrivesDeviceCodeLogin(t *testing.T) {
	calls := 0
	gotTenant := ""
	swapAz(t,
		func() (string, error) { return "/usr/bin/az", nil },
		func(context.Context) (azAccount, error) {
			calls++
			if calls == 1 {
				return azAccount{}, errors.New("Please run 'az login' to setup account")
			}
			return azAccount{ID: "acct-01", TenantID: "tenant-01", Name: "baotest"}, nil
		},
		func(_ context.Context, tenant string, stdout, _ io.Writer) error {
			gotTenant = tenant
			_, _ = io.WriteString(stdout, "To sign in, use a web browser to open https://microsoft.com/devicelogin and enter the code ABCD1234\n")
			return nil
		},
	)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-auth", "--tenant", "tenant-01"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if gotTenant != "tenant-01" {
		t.Errorf("az login tenant = %q, want tenant-01", gotTenant)
	}
	out := stdout.String()
	for _, want := range []string{"device-code login", "ABCD1234", "signed in: account acct-01"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestProvision_EnsureAuth_AzMissing_Errors(t *testing.T) {
	swapAz(t,
		func() (string, error) { return "", errors.New("exec: \"az\": not found") },
		func(context.Context) (azAccount, error) {
			t.Error("account-show reached despite az missing")
			return azAccount{}, nil
		},
		func(context.Context, string, io.Writer, io.Writer) error {
			t.Error("login reached despite az missing")
			return nil
		},
	)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-auth"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("provision() code = 0, want non-zero when az is not on PATH")
	}
	if !strings.Contains(stderr.String(), "az CLI not found") {
		t.Errorf("stderr missing the az-not-found guidance\n%s", stderr.String())
	}
}

func TestProvision_EnsureAuth_LoggedOutNoTenant_Errors(t *testing.T) {
	swapAz(t,
		func() (string, error) { return "/usr/bin/az", nil },
		func(context.Context) (azAccount, error) { return azAccount{}, errors.New("signed out") },
		func(context.Context, string, io.Writer, io.Writer) error {
			t.Error("login ran without a tenant")
			return nil
		},
	)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-auth"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("provision() code = 0, want non-zero when signed out with no --tenant")
	}
	if !strings.Contains(stderr.String(), "--tenant") {
		t.Errorf("stderr missing the missing-tenant guidance\n%s", stderr.String())
	}
}

func TestProvision_EnsureAuth_TenantMismatch_Warns(t *testing.T) {
	swapAz(t,
		func() (string, error) { return "/usr/bin/az", nil },
		func(context.Context) (azAccount, error) {
			return azAccount{ID: "acct-01", TenantID: "tenant-active"}, nil
		},
		func(context.Context, string, io.Writer, io.Writer) error {
			t.Error("login ran while already signed in")
			return nil
		},
	)

	var stdout, stderr strings.Builder
	code := provision([]string{"--ensure-auth", "--tenant", "tenant-wanted"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("provision() code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "note:") || !strings.Contains(out, "tenant-wanted") {
		t.Errorf("output missing the tenant-mismatch warning\n%s", out)
	}
}
