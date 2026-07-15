package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// pullRequest7 is the canned 201 pull-request response — the §9 recorded-ADO
// double for PR creation, so no test dials dev.azure.com. createdBy is the Dev
// agent user: a PR opened under the delegated agent-user token is authored by
// that user as a directory fact (spike S3), which the fixture stands in for while
// the live createdBy proof (AC-26) stays out of the §9 double's reach.
const pullRequest7 = `{
  "pullRequestId": 7,
  "status": "active",
  "isDraft": true,
  "sourceRefName": "refs/heads/mandat/task-42",
  "targetRefName": "refs/heads/main",
  "url": "https://dev.azure.com/baodo0220/mandat/_apis/git/repositories/mandat/pullRequests/7",
  "createdBy": {
    "id": "8c5e2f1a-0000-4000-8000-000000000001",
    "displayName": "Dev Agent 01",
    "uniqueName": "agent-user-dev-01@baotest.onmicrosoft.com"
  }
}`

// prRecorder captures the single request CreatePR makes so the test can assert
// its shape. It is mutex-guarded because the httptest handler runs on its own
// goroutine (matching fakeADO's discipline under -race).
type prRecorder struct {
	mu   sync.Mutex
	req  capturedReq
	seen bool
}

func (p *prRecorder) record(r *http.Request) {
	body, _ := readAll(r)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.req = capturedReq{
		method:      r.Method,
		path:        r.URL.Path,
		rawQuery:    r.URL.RawQuery,
		authz:       r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"),
		body:        body,
	}
	p.seen = true
}

func (p *prRecorder) recorded(t *testing.T) capturedReq {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seen {
		t.Fatal("no request recorded; CreatePR made no ADO call")
	}
	return p.req
}

// prServer is the recorded-ADO double for the pullrequests endpoint: it records
// the one request and replays a fixed status and body, so the whole PR seam is
// proven without a live call.
func prServer(t *testing.T, status int, body string) (*httptest.Server, *prRecorder) {
	t.Helper()
	rec := &prRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", jsonContentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestCreatePR_PostsDraftPRUnderBearer(t *testing.T) {
	t.Parallel()

	srv, rec := prServer(t, http.StatusCreated, pullRequest7)
	tp := &fakeTokenProvider{token: testToken}
	a := newAdapter(t, srv, tp, nil)

	res, err := a.CreatePR(context.Background(), CreatePRInput{
		Repo:        "mandat",
		Branch:      "mandat/task-42",
		BaseBranch:  "main",
		Title:       "US-0004: add the version subcommand",
		Description: "Opened by mandat under the Dev agent-user mandate.",
	})
	if err != nil {
		t.Fatalf("CreatePR() error = %v, want nil", err)
	}

	got := rec.recorded(t)
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if !strings.HasSuffix(got.path, "/baodo0220/mandat/_apis/git/repositories/mandat/pullrequests") {
		t.Errorf("path = %q, want .../git/repositories/mandat/pullrequests", got.path)
	}
	if !strings.Contains(got.rawQuery, "api-version="+apiVersion) {
		t.Errorf("query = %q, want api-version=%s", got.rawQuery, apiVersion)
	}
	if got.contentType != jsonContentType {
		t.Errorf("content-type = %q, want %q", got.contentType, jsonContentType)
	}
	// The Bearer seam: the request carries the injected provider's delegated
	// agent-user token, no PAT (spike S3, spec §4.2).
	if got.authz != "Bearer "+testToken {
		t.Errorf("authz = %q, want the provider's bearer token", got.authz)
	}

	var sent createPRRequest
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("PR body is not JSON: %v (%s)", err, got.body)
	}
	// isDraft:true is the load-bearing MVP invariant (RFC-0001 §10): draft-only,
	// there is no ready-PR path to assert the other way.
	if !sent.IsDraft {
		t.Error("isDraft = false, want true (draft-PR-only is the MVP autonomy ceiling)")
	}
	if sent.SourceRefName != "refs/heads/mandat/task-42" {
		t.Errorf("sourceRefName = %q, want refs/heads/mandat/task-42", sent.SourceRefName)
	}
	if sent.TargetRefName != "refs/heads/main" {
		t.Errorf("targetRefName = %q, want refs/heads/main", sent.TargetRefName)
	}
	if sent.Title != "US-0004: add the version subcommand" {
		t.Errorf("title = %q, want the input title", sent.Title)
	}

	// The parsed 201 carries the PR id, url, and createdBy = the Dev agent user.
	if res.ID != 7 {
		t.Errorf("result id = %d, want 7", res.ID)
	}
	if res.URL != "https://dev.azure.com/baodo0220/mandat/_apis/git/repositories/mandat/pullRequests/7" {
		t.Errorf("result url = %q, want the PR url from the 201 body", res.URL)
	}
	if res.CreatedBy != devAgentUser {
		t.Errorf("result createdBy = %q, want the Dev agent user %q", res.CreatedBy, devAgentUser)
	}

	// The token reached ADO only through the injected provider, minted for "dev".
	if roles := tp.calls(); len(roles) != 1 || roles[0] != "dev" {
		t.Errorf("token mint calls = %v, want exactly one for role dev", roles)
	}
}

func TestCreatePR_Non2xxReturnsTypedError(t *testing.T) {
	t.Parallel()

	const adoMsg = `{"message":"TF401398: The pull request cannot be created because the source and target are identical."}`
	srv, _ := prServer(t, http.StatusBadRequest, adoMsg)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	_, err := a.CreatePR(context.Background(), CreatePRInput{
		Repo:        "mandat",
		Branch:      "mandat/task-42",
		BaseBranch:  "main",
		Title:       "t",
		Description: "d",
	})
	if err == nil {
		t.Fatal("CreatePR() error = nil, want a typed non-2xx error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not *APIError; want a typed error carrying the status", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("APIError.Status = %d, want %d", apiErr.Status, http.StatusBadRequest)
	}
	if !strings.Contains(apiErr.Body, "TF401398") {
		t.Errorf("APIError.Body = %q, want it to carry the ADO message", apiErr.Body)
	}
}
