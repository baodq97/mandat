package entra

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The suite runs against fakeGraph, an httptest.Server replaying the exact
// blueprint/identity/user JSON shapes the research doc probed against the
// dogfood tenant — no test dials graph.microsoft.com, no az is shelled out, and
// no real token appears anywhere, proving the reads and the Bearer auth seam
// offline.

const testToken = "fake-graph-bearer-token"

// fakeTokenSource is the injected TokenSource: it returns a canned token with no
// az call and records how many times it was asked, so a test can assert the read
// path mints a token.
type fakeTokenSource struct {
	mu    sync.Mutex
	calls int
	token string
	err   error
}

func (f *fakeTokenSource) source(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.token, nil
}

// fakeGraph serves the three pinned Agent ID read endpoints from canned bodies
// and records every request for later assertion.
type fakeGraph struct {
	mu       sync.Mutex
	requests []capturedReq

	blueprintsBody string
	identitiesBody string
	usersBody      string
}

type capturedReq struct {
	method   string
	path     string
	rawQuery string
	authz    string
	body     string
}

func (f *fakeGraph) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGraph) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, capturedReq{
		method:   r.Method,
		path:     r.URL.Path,
		rawQuery: r.URL.RawQuery,
		authz:    r.Header.Get("Authorization"),
	})
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/applications/microsoft.graph.agentIdentityBlueprint"):
		_, _ = w.Write([]byte(f.blueprintsBody))
	case strings.HasSuffix(r.URL.Path, "/servicePrincipals/microsoft.graph.agentIdentity"):
		_, _ = w.Write([]byte(f.identitiesBody))
	case strings.HasSuffix(r.URL.Path, "/users/microsoft.graph.agentUser"):
		_, _ = w.Write([]byte(f.usersBody))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGraph) recorded() []capturedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedReq, len(f.requests))
	copy(out, f.requests)
	return out
}

// dogfoodGraph returns a fakeGraph seeded with the exact shapes the research
// doc probed: one blueprint, a dev + reviewer identity, and a dev user linked
// to the dev identity through identityParentId.
func dogfoodGraph() *fakeGraph {
	return &fakeGraph{
		blueprintsBody: `{"value":[{"id":"bp-object-01","appId":"appid-blueprint-01","displayName":"mandat-spike-blueprint"}]}`,
		identitiesBody: `{"value":[
			{"id":"identity-dev-01","displayName":"mandat-spike-dev"},
			{"id":"identity-reviewer-01","displayName":"mandat-spike-reviewer"}
		]}`,
		usersBody: `{"value":[
			{"id":"user-dev-01","displayName":"mandat-spike-dev-user","userPrincipalName":"dev@baotest.onmicrosoft.com","identityParentId":"identity-dev-01"}
		]}`,
	}
}

func newClient(t *testing.T, srv *httptest.Server, tokens *fakeTokenSource) *Client {
	t.Helper()
	c, err := New(Config{GraphBaseURL: srv.URL, TokenSource: tokens.source})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return c
}

func TestDiscoverRegistry_Success_PairsUserToIdentity(t *testing.T) {
	t.Parallel()

	fake := dogfoodGraph()
	srv := fake.start(t)
	tokens := &fakeTokenSource{token: testToken}
	c := newClient(t, srv, tokens)

	reg, err := c.DiscoverRegistry(context.Background())
	if err != nil {
		t.Fatalf("DiscoverRegistry() error = %v, want nil", err)
	}

	if reg.Blueprint == nil {
		t.Fatal("Registry.Blueprint = nil, want the dogfood blueprint")
	}
	if reg.Blueprint.AppID != "appid-blueprint-01" {
		t.Errorf("Blueprint.AppID = %q, want appid-blueprint-01", reg.Blueprint.AppID)
	}
	if len(reg.Identities) != 2 {
		t.Fatalf("Identities = %+v, want two (dev, reviewer)", reg.Identities)
	}

	// The dev identity pairs to the dev user through identityParentId.
	dev := reg.Identities[0]
	if dev.ID != "identity-dev-01" {
		t.Fatalf("Identities[0].ID = %q, want identity-dev-01", dev.ID)
	}
	user, ok := reg.PairedUser(dev)
	if !ok {
		t.Fatalf("PairedUser(%q) ok = false, want the dev user", dev.ID)
	}
	if user.ID != "user-dev-01" || user.UserPrincipalName != "dev@baotest.onmicrosoft.com" {
		t.Errorf("PairedUser = %+v, want the dev user with its UPN", user)
	}

	// The reviewer identity has no agent user in the fixture: pairing reports
	// not-found rather than mispairing it to the dev user.
	reviewer := reg.Identities[1]
	if _, ok := reg.PairedUser(reviewer); ok {
		t.Errorf("PairedUser(%q) ok = true, want false (no user links to it)", reviewer.ID)
	}
}

func TestDiscoverRegistry_ReadsAllThreeSurfacesAsAuthorizedGETs(t *testing.T) {
	t.Parallel()

	fake := dogfoodGraph()
	srv := fake.start(t)
	tokens := &fakeTokenSource{token: testToken}
	c := newClient(t, srv, tokens)

	if _, err := c.DiscoverRegistry(context.Background()); err != nil {
		t.Fatalf("DiscoverRegistry() error = %v, want nil", err)
	}

	reqs := fake.recorded()
	if len(reqs) != 3 {
		t.Fatalf("made %d requests, want 3 (blueprints, identities, users)", len(reqs))
	}
	for _, r := range reqs {
		if r.method != http.MethodGet {
			t.Errorf("%s: method = %q, want GET (the reuse path never writes)", r.path, r.method)
		}
		if r.authz != "Bearer "+testToken {
			t.Errorf("%s: authz = %q, want the minted bearer token", r.path, r.authz)
		}
	}

	// The identity and user reads carry the probed OData query options
	// verbatim (the $ is not percent-encoded).
	if got := reqByPathSuffix(t, reqs, "/servicePrincipals/microsoft.graph.agentIdentity"); got.rawQuery != "$top=100" {
		t.Errorf("agentIdentity query = %q, want $top=100", got.rawQuery)
	}
	if got := reqByPathSuffix(t, reqs, "/users/microsoft.graph.agentUser"); !strings.Contains(got.rawQuery, "$select=id,displayName,userPrincipalName,identityParentId") {
		t.Errorf("agentUser query = %q, want the identityParentId $select", got.rawQuery)
	}

	if tokens.calls == 0 {
		t.Error("token source was never called; the reads must mint a token")
	}
}

func TestDiscoverRegistry_NoBlueprint_ReportsNilBlueprint(t *testing.T) {
	t.Parallel()

	fake := dogfoodGraph()
	fake.blueprintsBody = `{"value":[]}`
	srv := fake.start(t)
	tokens := &fakeTokenSource{token: testToken}
	c := newClient(t, srv, tokens)

	reg, err := c.DiscoverRegistry(context.Background())
	if err != nil {
		t.Fatalf("DiscoverRegistry() error = %v, want nil", err)
	}
	if reg.Blueprint != nil {
		t.Errorf("Blueprint = %+v, want nil when none is provisioned", reg.Blueprint)
	}
}

func TestPairedUser_OmittedParentLink_NoPairing(t *testing.T) {
	t.Parallel()

	// A server that omits identityParentId yields no pairing rather than a
	// wrong one (the research doc's documented fallback).
	reg := Registry{
		Identities: []AgentIdentity{{ID: "identity-dev-01", DisplayName: "mandat-spike-dev"}},
		Users:      []AgentUser{{ID: "user-dev-01", DisplayName: "mandat-spike-dev-user"}},
	}
	if _, ok := reg.PairedUser(reg.Identities[0]); ok {
		t.Error("PairedUser ok = true with no identityParentId, want false")
	}
}

func TestListBlueprints_AuthFailure_ReturnsTypedAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"Authorization_RequestDenied"}}`))
	}))
	t.Cleanup(srv.Close)
	tokens := &fakeTokenSource{token: testToken}
	c := newClient(t, srv, tokens)

	_, err := c.ListBlueprints(context.Background())
	if err == nil {
		t.Fatal("ListBlueprints() error = nil, want a 403 auth failure")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("ListBlueprints() error = %v, want errors.As to an *APIError", err)
	}
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("APIError.Status = %d, want %d", apiErr.Status, http.StatusForbidden)
	}
}

func TestDiscoverRegistry_TokenSourceFailure_Surfaces(t *testing.T) {
	t.Parallel()

	fake := dogfoodGraph()
	srv := fake.start(t)
	tokens := &fakeTokenSource{err: errors.New("az not logged in")}
	c := newClient(t, srv, tokens)

	if _, err := c.DiscoverRegistry(context.Background()); err == nil {
		t.Fatal("DiscoverRegistry() error = nil, want the token-source failure surfaced")
	}
	// No request reaches the server when the token cannot be minted.
	if len(fake.recorded()) != 0 {
		t.Errorf("made %d requests with no token, want 0", len(fake.recorded()))
	}
}

func TestNew_DefaultsGraphBaseURL(t *testing.T) {
	t.Parallel()

	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if got := c.base.String(); got != defaultGraphBaseURL {
		t.Errorf("graph base = %q, want the production default %q", got, defaultGraphBaseURL)
	}
}

func TestNew_RejectsInvalidBaseURL(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{GraphBaseURL: "not-a-url"}); err == nil {
		t.Fatal("New() with a relative GraphBaseURL: error = nil, want an error")
	}
}

func reqByPathSuffix(t *testing.T, reqs []capturedReq, suffix string) capturedReq {
	t.Helper()
	for _, r := range reqs {
		if strings.HasSuffix(r.path, suffix) {
			return r
		}
	}
	t.Fatalf("no recorded request with path suffix %q", suffix)
	return capturedReq{}
}

// The write suite runs against writeGraph, an httptest.Server that records every
// request (method, path, body, authz) and lets a test script the create POST and
// delete DELETE responses — proving the ensure/create/delete path and the 403 →
// *PrivilegeError mapping offline, with no az shellout and no real token.

// writeGraph records each request and serves the Agent-ID write endpoints: a
// canned identity list on the GET read, and scripted status+body on the create
// POST and the delete DELETE.
type writeGraph struct {
	mu       sync.Mutex
	requests []capturedReq

	listBody     string
	createStatus int
	createBody   string
	deleteStatus int
	deleteBody   string
}

func (g *writeGraph) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.requests = append(g.requests, capturedReq{
			method:   r.Method,
			path:     r.URL.Path,
			rawQuery: r.URL.RawQuery,
			authz:    r.Header.Get("Authorization"),
			body:     string(reqBody),
		})
		g.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/servicePrincipals/microsoft.graph.agentIdentity"):
			_, _ = w.Write([]byte(g.listBody))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/servicePrincipals/microsoft.graph.agentIdentity"):
			w.WriteHeader(g.createStatus)
			_, _ = w.Write([]byte(g.createBody))
		case r.Method == http.MethodDelete:
			w.WriteHeader(g.deleteStatus)
			_, _ = w.Write([]byte(g.deleteBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *writeGraph) recorded() []capturedReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]capturedReq, len(g.requests))
	copy(out, g.requests)
	return out
}

func TestEnsureAgentIdentity_ExistingName_ReusesWithoutWrite(t *testing.T) {
	t.Parallel()

	fake := &writeGraph{
		listBody: `{"value":[
			{"id":"identity-dev-01","displayName":"mandat-spike-dev"},
			{"id":"identity-pilot-01","displayName":"mandat-pilot"}
		]}`,
	}
	srv := fake.start(t)
	tokens := &fakeTokenSource{token: testToken}
	c := newClient(t, srv, tokens)

	id, created, err := c.EnsureAgentIdentity(context.Background(), "bp-object-01", "mandat-pilot", []string{"sponsor-01"})
	if err != nil {
		t.Fatalf("EnsureAgentIdentity() error = %v, want nil", err)
	}
	if created {
		t.Error("created = true, want false when the identity already exists")
	}
	if id.ID != "identity-pilot-01" {
		t.Errorf("identity.ID = %q, want identity-pilot-01 (the existing one)", id.ID)
	}
	// Reuse issues the list GET and no write.
	for _, r := range fake.recorded() {
		if r.method != http.MethodGet {
			t.Errorf("recorded a %s request; reuse must issue only the list GET", r.method)
		}
	}
}

func TestEnsureAgentIdentity_AbsentName_CreatesViaPost(t *testing.T) {
	t.Parallel()

	fake := &writeGraph{
		listBody:     `{"value":[{"id":"identity-dev-01","displayName":"mandat-spike-dev"}]}`,
		createStatus: http.StatusCreated,
		createBody:   `{"id":"identity-pilot-99","displayName":"mandat-pilot"}`,
	}
	srv := fake.start(t)
	tokens := &fakeTokenSource{token: testToken}
	c := newClient(t, srv, tokens)

	id, created, err := c.EnsureAgentIdentity(context.Background(), "bp-object-01", "mandat-pilot", []string{"sponsor-99"})
	if err != nil {
		t.Fatalf("EnsureAgentIdentity() error = %v, want nil", err)
	}
	if !created {
		t.Error("created = false, want true when the identity is absent")
	}
	if id.ID != "identity-pilot-99" || id.DisplayName != "mandat-pilot" {
		t.Errorf("identity = %+v, want the created identity-pilot-99/mandat-pilot", id)
	}

	// The ensure lists (GET) then creates (POST); the POST carries the bearer
	// token and the full v1.0 body — displayName, agentIdentityBlueprintId, and a
	// non-empty sponsors@odata.bind (the field the live 400 demanded).
	reqs := fake.recorded()
	var post *capturedReq
	for i := range reqs {
		if reqs[i].method == http.MethodPost {
			post = &reqs[i]
		}
	}
	if post == nil {
		t.Fatalf("no POST recorded; the absent-identity path must create via POST (requests: %+v)", reqs)
	}
	if post.authz != "Bearer "+testToken {
		t.Errorf("POST authz = %q, want the minted bearer token", post.authz)
	}
	for _, want := range []string{
		`"displayName":"mandat-pilot"`,
		`"agentIdentityBlueprintId":"bp-object-01"`,
		`"sponsors@odata.bind":[`,
		"/users/sponsor-99",
	} {
		if !strings.Contains(post.body, want) {
			t.Errorf("POST body missing %q\n%s", want, post.body)
		}
	}
}

func TestCreateAgentIdentity_Forbidden_ReturnsPrivilegeError(t *testing.T) {
	t.Parallel()

	fake := &writeGraph{
		createStatus: http.StatusForbidden,
		createBody:   `{"error":{"code":"Authorization_RequestDenied","message":"Insufficient privileges"}}`,
	}
	srv := fake.start(t)
	tokens := &fakeTokenSource{token: testToken}
	c := newClient(t, srv, tokens)

	_, err := c.CreateAgentIdentity(context.Background(), "bp-object-01", "mandat-pilot", []string{"sponsor-01"})
	if err == nil {
		t.Fatal("CreateAgentIdentity() error = nil, want a 403 privilege failure")
	}
	var privErr *PrivilegeError
	if !errors.As(err, &privErr) {
		t.Fatalf("CreateAgentIdentity() error = %v, want errors.As to *PrivilegeError", err)
	}
	if privErr.Method != http.MethodPost {
		t.Errorf("PrivilegeError.Method = %q, want POST", privErr.Method)
	}
	if !strings.HasSuffix(privErr.Endpoint, "/servicePrincipals/microsoft.graph.agentIdentity") {
		t.Errorf("PrivilegeError.Endpoint = %q, want the agentIdentity create endpoint", privErr.Endpoint)
	}
	// A forbidden write is a *PrivilegeError, never also a plain *APIError.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("403 also matched *APIError (%v); a forbidden write must be a *PrivilegeError only", apiErr)
	}
}
