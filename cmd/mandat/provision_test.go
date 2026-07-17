package main

import (
	"context"
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

	blueprintsBody string
	identitiesBody string
	usersBody      string

	createStatus int
	createBody   string
	deleteStatus int
	deleteBody   string
}

func (f *fakeGraphServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.methods = append(f.methods, r.Method)
		if r.Method == http.MethodPost {
			f.postBody = string(reqBody)
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/servicePrincipals/microsoft.graph.agentIdentity"):
			w.WriteHeader(f.createStatus)
			_, _ = w.Write([]byte(f.createBody))
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

// dogfoodGraphServer seeds a blueprint and a dev identity paired to a dev user,
// but no reviewer identity — so the reuse report lists a paired identity and the
// dry-run plan has a genuinely-absent role to plan the create for.
func dogfoodGraphServer() *fakeGraphServer {
	return &fakeGraphServer{
		blueprintsBody: `{"value":[{"id":"bp-object-01","appId":"appid-blueprint-01","displayName":"mandat-spike-blueprint"}]}`,
		identitiesBody: `{"value":[{"id":"identity-dev-01","displayName":"mandat-spike-dev"}]}`,
		usersBody:      `{"value":[{"id":"user-dev-01","displayName":"mandat-spike-dev-user","userPrincipalName":"dev@baotest.onmicrosoft.com","identityParentId":"identity-dev-01"}]}`,
	}
}

// swapGraphTokenSource installs src as provision's token source for the test's
// duration. A test that swaps it runs non-parallel: graphTokenSource is package
// state and -race rejects a concurrent write.
func swapGraphTokenSource(t *testing.T, src entra.TokenSource) {
	t.Helper()
	saved := graphTokenSource
	graphTokenSource = src
	t.Cleanup(func() { graphTokenSource = saved })
}

// swapDeriveSponsor installs fn as provision's sponsor derivation for the test's
// duration, so an ensure test resolves the default sponsor with no az shellout.
// Like swapGraphTokenSource it mutates package state, so its callers run
// non-parallel.
func swapDeriveSponsor(t *testing.T, fn func(context.Context) (string, error)) {
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
	swapDeriveSponsor(t, func(context.Context) (string, error) { return "sponsor-signed-in", nil })

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
	swapDeriveSponsor(t, func(context.Context) (string, error) { return "sponsor-signed-in", nil })

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
	swapDeriveSponsor(t, func(context.Context) (string, error) { return "sponsor-signed-in", nil })

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
	swapDeriveSponsor(t, func(context.Context) (string, error) { return "sponsor-signed-in", nil })

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
	swapDeriveSponsor(t, func(context.Context) (string, error) {
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
	swapDeriveSponsor(t, func(context.Context) (string, error) { return "sponsor-signed-in", nil })

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
