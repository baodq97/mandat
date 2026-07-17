package discovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The whole suite runs against fakeADO, an httptest.Server replaying canned
// profile/accounts/projects/repositories JSON — no test dials vssps.visualstudio.com
// or dev.azure.com, and no real token appears anywhere, proving the chain and
// the Bearer auth seam offline.

const testToken = "fake-bearer-token"

// fakeADO is the recorded-ADO double. It serves the four pinned endpoints from
// canned bodies and records every request for later assertion; both the
// vssps and dev.azure.com hosts route through the same server in tests, since
// Config's two base URLs are independently overridable but nothing requires
// them to differ.
type fakeADO struct {
	mu       sync.Mutex
	requests []capturedReq

	profileBody  string
	accountsBody string
	projectsBody string
	reposBody    string
}

type capturedReq struct {
	method   string
	path     string
	rawQuery string
	authz    string
}

func (f *fakeADO) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeADO) handle(w http.ResponseWriter, r *http.Request) {
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
	case strings.HasSuffix(r.URL.Path, "/_apis/profile/profiles/me"):
		_, _ = w.Write([]byte(f.profileBody))
	case strings.HasSuffix(r.URL.Path, "/_apis/accounts"):
		_, _ = w.Write([]byte(f.accountsBody))
	case strings.HasSuffix(r.URL.Path, "/_apis/projects"):
		_, _ = w.Write([]byte(f.projectsBody))
	case strings.HasSuffix(r.URL.Path, "/_apis/git/repositories"):
		_, _ = w.Write([]byte(f.reposBody))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeADO) recorded() []capturedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedReq, len(f.requests))
	copy(out, f.requests)
	return out
}

func newClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{VSSPSBaseURL: srv.URL, AzureDevOpsBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return c
}

func TestDiscover_Success(t *testing.T) {
	t.Parallel()

	fake := &fakeADO{
		profileBody:  `{"id":"11111111-0000-4000-8000-000000000001"}`,
		accountsBody: `{"count":1,"value":[{"accountId":"22222222-0000-4000-8000-000000000002","accountName":"contoso"}]}`,
		projectsBody: `{"count":1,"value":[{"id":"33333333-0000-4000-8000-000000000003","name":"mandat-pilot"}]}`,
		reposBody:    `{"count":1,"value":[{"id":"44444444-0000-4000-8000-000000000004","name":"mandat","remoteUrl":"https://dev.azure.com/contoso/mandat-pilot/_git/mandat"}]}`,
	}
	srv := fake.start(t)
	c := newClient(t, srv)

	got, err := c.Discover(context.Background(), testToken)
	if err != nil {
		t.Fatalf("Discover() error = %v, want nil", err)
	}

	if got.Org.Name != "contoso" {
		t.Errorf("Org.Name = %q, want %q", got.Org.Name, "contoso")
	}
	if got.Org.ID != "22222222-0000-4000-8000-000000000002" {
		t.Errorf("Org.ID = %q, want the accountId", got.Org.ID)
	}
	if len(got.Org.Projects) != 1 || got.Org.Projects[0].Name != "mandat-pilot" {
		t.Fatalf("Org.Projects = %+v, want one project named mandat-pilot", got.Org.Projects)
	}
	repos := got.Org.Projects[0].Repositories
	if len(repos) != 1 || repos[0].Name != "mandat" {
		t.Fatalf("Repositories = %+v, want one repo named mandat", repos)
	}
	if repos[0].RemoteURL != "https://dev.azure.com/contoso/mandat-pilot/_git/mandat" {
		t.Errorf("RemoteURL = %q, want the fixture remoteUrl", repos[0].RemoteURL)
	}

	// Every call in the chain carries the pinned api-version and the bearer
	// token, and never anything else.
	reqs := fake.recorded()
	if len(reqs) != 4 {
		t.Fatalf("made %d requests, want 4 (profile, accounts, projects, repositories)", len(reqs))
	}
	for _, r := range reqs {
		if r.method != http.MethodGet {
			t.Errorf("%s %s: method = %q, want GET", r.path, r.rawQuery, r.method)
		}
		if !strings.Contains(r.rawQuery, "api-version="+apiVersion) {
			t.Errorf("%s query = %q, want api-version=%s", r.path, r.rawQuery, apiVersion)
		}
		if r.authz != "Bearer "+testToken {
			t.Errorf("%s authz = %q, want %q", r.path, r.authz, "Bearer "+testToken)
		}
	}
	if !strings.HasSuffix(reqs[0].path, "/_apis/profile/profiles/me") {
		t.Errorf("first call path = %q, want the profile endpoint", reqs[0].path)
	}
	if !strings.HasSuffix(reqs[1].path, "/_apis/accounts") || !strings.Contains(reqs[1].rawQuery, "memberId=11111111-0000-4000-8000-000000000001") {
		t.Errorf("second call = %q?%q, want the accounts endpoint carrying the profile's member id", reqs[1].path, reqs[1].rawQuery)
	}
	if !strings.HasSuffix(reqs[2].path, "/contoso/_apis/projects") {
		t.Errorf("third call path = %q, want the org's projects endpoint", reqs[2].path)
	}
	if !strings.HasSuffix(reqs[3].path, "/contoso/mandat-pilot/_apis/git/repositories") {
		t.Errorf("fourth call path = %q, want the project's repositories endpoint", reqs[3].path)
	}
}

func TestDiscover_EmptyAccounts_ReturnsErrNoOrgReachable(t *testing.T) {
	t.Parallel()

	fake := &fakeADO{
		profileBody:  `{"id":"11111111-0000-4000-8000-000000000001"}`,
		accountsBody: `{"count":0,"value":[]}`,
	}
	srv := fake.start(t)
	c := newClient(t, srv)

	_, err := c.Discover(context.Background(), testToken)
	if !errors.Is(err, ErrNoOrgReachable) {
		t.Fatalf("Discover() error = %v, want errors.Is(err, ErrNoOrgReachable)", err)
	}

	// No project or repository call is made when there is no org to resolve.
	if len(fake.recorded()) != 2 {
		t.Errorf("made %d requests, want 2 (profile, accounts) with no org to descend into", len(fake.recorded()))
	}
}

func TestDiscover_MultipleAccounts_ReturnsAmbiguousOrgError(t *testing.T) {
	t.Parallel()

	fake := &fakeADO{
		profileBody: `{"id":"11111111-0000-4000-8000-000000000001"}`,
		accountsBody: `{"count":2,"value":[
			{"accountId":"aaaaaaaa-0000-4000-8000-000000000001","accountName":"contoso"},
			{"accountId":"bbbbbbbb-0000-4000-8000-000000000002","accountName":"other-org"}
		]}`,
	}
	srv := fake.start(t)
	c := newClient(t, srv)

	_, err := c.Discover(context.Background(), testToken)
	if !errors.Is(err, ErrAmbiguousOrg) {
		t.Fatalf("Discover() error = %v, want errors.Is(err, ErrAmbiguousOrg)", err)
	}
	var ambErr *AmbiguousOrgError
	if !errors.As(err, &ambErr) {
		t.Fatalf("Discover() error = %v, want errors.As to an *AmbiguousOrgError", err)
	}
	want := []string{"contoso", "other-org"}
	if len(ambErr.Orgs) != len(want) || ambErr.Orgs[0] != want[0] || ambErr.Orgs[1] != want[1] {
		t.Errorf("AmbiguousOrgError.Orgs = %v, want %v", ambErr.Orgs, want)
	}

	// Ambiguity is resolved by a later interactive prompt, not by guessing a
	// project/repo tree for either org.
	if len(fake.recorded()) != 2 {
		t.Errorf("made %d requests, want 2 (profile, accounts) with the org unresolved", len(fake.recorded()))
	}
}

func TestDiscover_AuthFailure_ReturnsTypedAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"the token is invalid or expired"}`))
	}))
	t.Cleanup(srv.Close)
	c := newClient(t, srv)

	_, err := c.Discover(context.Background(), testToken)
	if err == nil {
		t.Fatal("Discover() error = nil, want a transport/auth failure")
	}
	if errors.Is(err, ErrNoOrgReachable) || errors.Is(err, ErrAmbiguousOrg) {
		t.Fatalf("Discover() error = %v, want it distinguishable from the no-org and ambiguous cases", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Discover() error = %v, want errors.As to an *APIError", err)
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Errorf("APIError.Status = %d, want %d", apiErr.Status, http.StatusUnauthorized)
	}
}

func TestDiscover_TransportFailure_ConnectionRefused(t *testing.T) {
	t.Parallel()

	// A closed server's URL still resolves but refuses the connection, giving a
	// deterministic, offline transport failure distinct from any HTTP response.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	c := newClient(t, srv)

	_, err := c.Discover(context.Background(), testToken)
	if err == nil {
		t.Fatal("Discover() error = nil, want a transport failure")
	}
	if errors.Is(err, ErrNoOrgReachable) || errors.Is(err, ErrAmbiguousOrg) {
		t.Fatalf("Discover() error = %v, want it distinguishable from the no-org and ambiguous cases", err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("Discover() error = %v, want a connection-level failure, not an *APIError", err)
	}
}

func TestDiscover_RequiresToken(t *testing.T) {
	t.Parallel()

	fake := &fakeADO{}
	srv := fake.start(t)
	c := newClient(t, srv)

	if _, err := c.Discover(context.Background(), ""); err == nil {
		t.Fatal("Discover(\"\") error = nil, want an error for a missing token")
	}
	if len(fake.recorded()) != 0 {
		t.Errorf("made %d requests with no token, want 0", len(fake.recorded()))
	}
}

func TestNew_DefaultsBaseURLs(t *testing.T) {
	t.Parallel()

	c, err := New(Config{})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if got := c.vssps.String(); got != defaultVSSPSBaseURL {
		t.Errorf("vssps base = %q, want the production default %q", got, defaultVSSPSBaseURL)
	}
	if got := c.base.String(); got != defaultAzureDevOpsBaseURL {
		t.Errorf("dev.azure.com base = %q, want the production default %q", got, defaultAzureDevOpsBaseURL)
	}
}

func TestNew_RejectsInvalidBaseURL(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{VSSPSBaseURL: "not-a-url"}); err == nil {
		t.Fatal("New() with a relative VSSPSBaseURL: error = nil, want an error")
	}
	if _, err := New(Config{AzureDevOpsBaseURL: "not-a-url"}); err == nil {
		t.Fatal("New() with a relative AzureDevOpsBaseURL: error = nil, want an error")
	}
}

func TestValidateOrgAccess_Success_ReturnsNil(t *testing.T) {
	t.Parallel()

	fake := &fakeADO{projectsBody: `{"count":1,"value":[{"id":"33333333-0000-4000-8000-000000000003","name":"mandat-pilot"}]}`}
	srv := fake.start(t)
	c := newClient(t, srv)

	if err := c.ValidateOrgAccess(context.Background(), testToken, "contoso"); err != nil {
		t.Fatalf("ValidateOrgAccess() error = %v, want nil", err)
	}

	// The probe is exactly the one projects call, carrying the pinned
	// api-version and the bearer token — never the full four-call descent.
	reqs := fake.recorded()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want 1 (the projects probe)", len(reqs))
	}
	if !strings.HasSuffix(reqs[0].path, "/contoso/_apis/projects") {
		t.Errorf("probe path = %q, want the org's projects endpoint", reqs[0].path)
	}
	if !strings.Contains(reqs[0].rawQuery, "api-version="+apiVersion) {
		t.Errorf("probe query = %q, want api-version=%s", reqs[0].rawQuery, apiVersion)
	}
	if reqs[0].authz != "Bearer "+testToken {
		t.Errorf("probe authz = %q, want %q", reqs[0].authz, "Bearer "+testToken)
	}
}

func TestValidateOrgAccess_AuthFailure_ReturnsTypedAPIError(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"the token cannot reach this organization"}`))
			}))
			t.Cleanup(srv.Close)
			c := newClient(t, srv)

			err := c.ValidateOrgAccess(context.Background(), testToken, "contoso")
			if err == nil {
				t.Fatalf("ValidateOrgAccess() error = nil, want a %d auth failure", status)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("ValidateOrgAccess() error = %v, want errors.As to an *APIError", err)
			}
			if apiErr.Status != status {
				t.Errorf("APIError.Status = %d, want %d", apiErr.Status, status)
			}
		})
	}
}

func TestValidateOrgAccess_TransportFailure_ConnectionRefused(t *testing.T) {
	t.Parallel()

	// A closed server's URL still resolves but refuses the connection, giving a
	// deterministic, offline transport failure distinct from any HTTP response.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	c := newClient(t, srv)

	err := c.ValidateOrgAccess(context.Background(), testToken, "contoso")
	if err == nil {
		t.Fatal("ValidateOrgAccess() error = nil, want a transport failure")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("ValidateOrgAccess() error = %v, want a connection-level failure, not an *APIError", err)
	}
}

func TestDiscover_MapsDefaultBranch_StripsRefsHeadsPrefix(t *testing.T) {
	t.Parallel()

	fake := &fakeADO{
		profileBody:  `{"id":"11111111-0000-4000-8000-000000000001"}`,
		accountsBody: `{"count":1,"value":[{"accountId":"22222222-0000-4000-8000-000000000002","accountName":"contoso"}]}`,
		projectsBody: `{"count":1,"value":[{"id":"33333333-0000-4000-8000-000000000003","name":"mandat-pilot"}]}`,
		reposBody:    `{"count":1,"value":[{"id":"44444444-0000-4000-8000-000000000004","name":"mandat","remoteUrl":"https://dev.azure.com/contoso/mandat-pilot/_git/mandat","defaultBranch":"refs/heads/main"}]}`,
	}
	srv := fake.start(t)
	c := newClient(t, srv)

	got, err := c.Discover(context.Background(), testToken)
	if err != nil {
		t.Fatalf("Discover() error = %v, want nil", err)
	}
	repos := got.Org.Projects[0].Repositories
	if len(repos) != 1 || repos[0].DefaultBranch != "main" {
		t.Errorf("Repositories[0].DefaultBranch = %q, want %q (refs/heads/ stripped)", repos[0].DefaultBranch, "main")
	}
}

func TestDiscover_NullDefaultBranch_LeavesEmpty(t *testing.T) {
	t.Parallel()

	fake := &fakeADO{
		profileBody:  `{"id":"11111111-0000-4000-8000-000000000001"}`,
		accountsBody: `{"count":1,"value":[{"accountId":"22222222-0000-4000-8000-000000000002","accountName":"contoso"}]}`,
		projectsBody: `{"count":1,"value":[{"id":"33333333-0000-4000-8000-000000000003","name":"mandat-pilot"}]}`,
		reposBody:    `{"count":1,"value":[{"id":"44444444-0000-4000-8000-000000000004","name":"mandat","remoteUrl":"https://dev.azure.com/contoso/mandat-pilot/_git/mandat","defaultBranch":null}]}`,
	}
	srv := fake.start(t)
	c := newClient(t, srv)

	got, err := c.Discover(context.Background(), testToken)
	if err != nil {
		t.Fatalf("Discover() error = %v, want nil", err)
	}
	repos := got.Org.Projects[0].Repositories
	if len(repos) != 1 || repos[0].DefaultBranch != "" {
		t.Errorf("Repositories[0].DefaultBranch = %q, want empty for a null defaultBranch", repos[0].DefaultBranch)
	}
}
