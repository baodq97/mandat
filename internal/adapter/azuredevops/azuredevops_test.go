package azuredevops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/baodq97/mandat/internal/config"
	"github.com/baodq97/mandat/internal/task"
)

// The whole suite runs against fakeADO, an httptest.Server replaying canned WIQL
// and work-item JSON — the §9 recorded-ADO-fixture double. No test dials
// dev.azure.com: every request the adapter makes is served by, and recorded in,
// the local fake, which is what proves the mapping and the Bearer auth seam
// without a live call (AC-4.4).

const (
	testToken    = "fake-delegated-agent-user-token"
	devAgentUser = "agent-user-dev-01@baotest.onmicrosoft.com"
	testOrg      = "baodo0220"
	testProject  = "mandat"
)

// workItem42 is the happy-path fixture: assigned to the dev agent user, tagged
// for the in-registry `mandat` repo, with the standard ADO acceptance-criteria
// field populated. System.AssignedTo is the identity-reference object ADO
// actually returns, not a bare string.
const workItem42 = `{
  "id": 42,
  "fields": {
    "System.Title": "Add the version subcommand",
    "System.State": "Active",
    "System.Tags": "repo:mandat; needs-review",
    "Microsoft.VSTS.Common.AcceptanceCriteria": "mandat version prints the build version and exits 0",
    "System.AssignedTo": {
      "id": "8c5e2f1a-0000-4000-8000-000000000001",
      "displayName": "Dev Agent 01",
      "uniqueName": "agent-user-dev-01@baotest.onmicrosoft.com"
    }
  },
  "url": "https://example.test/_apis/wit/workItems/42"
}`

// workItem43 is assigned correctly but tagged for a repo absent from the
// registry — the AC-4.7 skip case.
const workItem43 = `{
  "id": 43,
  "fields": {
    "System.Title": "Touch a repo we do not manage",
    "System.State": "New",
    "System.Tags": "repo:not-registered",
    "Microsoft.VSTS.Common.AcceptanceCriteria": "n/a",
    "System.AssignedTo": { "uniqueName": "agent-user-dev-01@baotest.onmicrosoft.com" }
  },
  "url": "https://example.test/_apis/wit/workItems/43"
}`

// workItem44 is tagged for the in-registry repo but assigned to a human — the
// AC-4.2 consent case: it must never be enqueued.
const workItem44 = `{
  "id": 44,
  "fields": {
    "System.Title": "A humans-only task",
    "System.State": "New",
    "System.Tags": "repo:mandat",
    "Microsoft.VSTS.Common.AcceptanceCriteria": "n/a",
    "System.AssignedTo": { "uniqueName": "a-human@baotest.onmicrosoft.com" }
  },
  "url": "https://example.test/_apis/wit/workItems/44"
}`

type capturedReq struct {
	method      string
	path        string
	rawQuery    string
	authz       string
	contentType string
	body        []byte
}

// fakeADO is the recorded-ADO double. It records every request for later
// assertion and serves work-item fixtures by id; the WIQL response is derived
// from the fixture keys, so adding a work item adds it to the poll surface.
type fakeADO struct {
	mu        sync.Mutex
	requests  []capturedReq
	workItems map[int]string
}

func newFakeADO(items map[int]string) *fakeADO {
	return &fakeADO{workItems: items}
}

func (f *fakeADO) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeADO) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := readAll(r)
	f.mu.Lock()
	f.requests = append(f.requests, capturedReq{
		method:      r.Method,
		path:        r.URL.Path,
		rawQuery:    r.URL.RawQuery,
		authz:       r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"),
		body:        body,
	})
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_apis/wit/wiql"):
		f.writeWIQL(w)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"text":"recorded"}`))
	case r.Method == http.MethodPatch && witID(r.URL.Path) != 0:
		f.writeWorkItem(w, witID(r.URL.Path))
	case r.Method == http.MethodGet && witID(r.URL.Path) != 0:
		f.writeWorkItem(w, witID(r.URL.Path))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeADO) writeWIQL(w http.ResponseWriter) {
	f.mu.Lock()
	ids := make([]int, 0, len(f.workItems))
	for id := range f.workItems {
		ids = append(ids, id)
	}
	f.mu.Unlock()
	sort.Ints(ids)

	refs := make([]string, len(ids))
	for i, id := range ids {
		refs[i] = fmt.Sprintf(`{"id":%d,"url":"https://example.test/_apis/wit/workItems/%d"}`, id, id)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"queryType":"flat","workItems":[%s]}`, strings.Join(refs, ","))
}

func (f *fakeADO) writeWorkItem(w http.ResponseWriter, id int) {
	f.mu.Lock()
	item, ok := f.workItems[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(item))
}

func (f *fakeADO) recorded() []capturedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedReq, len(f.requests))
	copy(out, f.requests)
	return out
}

func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

// witID pulls the numeric id out of a .../workitems/{id}[/comments] path; 0 for
// paths without one.
func witID(path string) int {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if strings.EqualFold(p, "workitems") && i+1 < len(parts) {
			id, err := strconv.Atoi(parts[i+1])
			if err != nil {
				return 0
			}
			return id
		}
	}
	return 0
}

// fakeTokenProvider is the injected auth seam: it hands back a fixed token and
// records the role every call minted for, proving the adapter never mints its
// own credential.
type fakeTokenProvider struct {
	mu    sync.Mutex
	token string
	roles []string
}

func (f *fakeTokenProvider) Token(_ context.Context, role string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roles = append(f.roles, role)
	return f.token, nil
}

func (f *fakeTokenProvider) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.roles))
	copy(out, f.roles)
	return out
}

// registry is a real *config.Config, so RemitSource is satisfied structurally
// (the same value cmd wiring would inject) and "remit from config" is proven
// through the production lookup, not a stub.
func registry() *config.Config {
	return &config.Config{
		Repos: map[string]config.RepoConfig{
			"mandat": {
				URL:        "https://dev.azure.com/baodo0220/mandat/_git/mandat",
				BaseBranch: "main",
				Paths:      []string{"cmd/mandat/", "internal/buildinfo/"},
			},
		},
	}
}

func newAdapter(t *testing.T, srv *httptest.Server, tokens TokenProvider, logger *slog.Logger) *Adapter {
	t.Helper()
	a, err := New(Config{
		BaseURL:      srv.URL,
		Org:          testOrg,
		Project:      testProject,
		Role:         "dev",
		DevAgentUser: devAgentUser,
		Tokens:       tokens,
		Remits:       registry(),
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return a
}

func TestPoll_MapsFixtureWorkItem(t *testing.T) {
	t.Parallel()

	fake := newFakeADO(map[int]string{42: workItem42})
	srv := fake.start(t)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	got, err := a.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("Poll() returned %d contracts, want 1", len(got))
	}
	tc := got[0]

	if tc.ID != "ado-baodo0220-42" {
		t.Errorf("ID = %q, want %q", tc.ID, "ado-baodo0220-42")
	}
	if tc.TrackerRef.System != task.TrackerAzureDevOps {
		t.Errorf("tracker_ref.system = %q, want %q", tc.TrackerRef.System, task.TrackerAzureDevOps)
	}
	if tc.TrackerRef.Org != testOrg || tc.TrackerRef.Project != testProject {
		t.Errorf("tracker_ref org/project = %q/%q, want %q/%q", tc.TrackerRef.Org, tc.TrackerRef.Project, testOrg, testProject)
	}
	if tc.TrackerRef.WorkItemID != "42" {
		t.Errorf("tracker_ref.work_item_id = %q, want %q", tc.TrackerRef.WorkItemID, "42")
	}
	if !strings.HasSuffix(tc.TrackerRef.URL, "/baodo0220/mandat/_workitems/edit/42") {
		t.Errorf("tracker_ref.url = %q, want the human edit URL ending /baodo0220/mandat/_workitems/edit/42", tc.TrackerRef.URL)
	}
	if tc.Type != task.TypeDevTask {
		t.Errorf("type = %q, want %q", tc.Type, task.TypeDevTask)
	}
	if tc.Title != "Add the version subcommand" {
		t.Errorf("title = %q, want %q", tc.Title, "Add the version subcommand")
	}
	if tc.Acceptance != "mandat version prints the build version and exits 0" {
		t.Errorf("acceptance = %q, want the acceptance-criteria field text", tc.Acceptance)
	}
	if tc.Refs == nil || len(tc.Refs) != 0 {
		t.Errorf("refs = %#v, want an empty (non-nil) slice", tc.Refs)
	}
	if tc.State != task.StateQueued {
		t.Errorf("state = %q, want %q", tc.State, task.StateQueued)
	}
	if tc.Role != "dev" {
		t.Errorf("role = %q, want %q", tc.Role, "dev")
	}
	if tc.AssignedTo != devAgentUser {
		t.Errorf("assigned_to = %q, want %q", tc.AssignedTo, devAgentUser)
	}
	if tc.SchemaVersion != task.SchemaVersion {
		t.Errorf("schema_version = %d, want %d", tc.SchemaVersion, task.SchemaVersion)
	}

	// Remit comes from the repo registry, not any ADO field (AC-4.6).
	wantRemit := task.Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}}
	if tc.Remit.Repo != wantRemit.Repo || tc.Remit.BaseBranch != wantRemit.BaseBranch {
		t.Errorf("remit repo/base = %q/%q, want %q/%q", tc.Remit.Repo, tc.Remit.BaseBranch, wantRemit.Repo, wantRemit.BaseBranch)
	}
	if strings.Join(tc.Remit.Paths, ",") != strings.Join(wantRemit.Paths, ",") {
		t.Errorf("remit.paths = %v, want %v (registry defaults)", tc.Remit.Paths, wantRemit.Paths)
	}

	// The produced contract round-trips the task package's validation (AC-4.8).
	if err := tc.Validate(); err != nil {
		t.Errorf("produced contract failed task.Validate(): %v", err)
	}
}

func TestPoll_BearerHeaderCarriesProviderToken(t *testing.T) {
	t.Parallel()

	fake := newFakeADO(map[int]string{42: workItem42})
	srv := fake.start(t)
	tp := &fakeTokenProvider{token: testToken}
	a := newAdapter(t, srv, tp, nil)

	if _, err := a.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v, want nil", err)
	}

	reqs := fake.recorded()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded; the poll made no ADO call")
	}
	for _, r := range reqs {
		if r.authz != "Bearer "+testToken {
			t.Errorf("%s %s Authorization = %q, want %q", r.method, r.path, r.authz, "Bearer "+testToken)
		}
	}

	// Every request minted through the injected provider for the "dev" role; the
	// adapter never sources a credential of its own.
	roles := tp.calls()
	if len(roles) != len(reqs) {
		t.Errorf("token minted %d times, want one per request (%d)", len(roles), len(reqs))
	}
	for _, role := range roles {
		if role != "dev" {
			t.Errorf("token minted for role %q, want %q", role, "dev")
		}
	}
}

func TestPoll_WIQLFiltersByAssignment(t *testing.T) {
	t.Parallel()

	fake := newFakeADO(map[int]string{42: workItem42})
	srv := fake.start(t)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	if _, err := a.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() error = %v, want nil", err)
	}

	var wiql capturedReq
	found := false
	for _, r := range fake.recorded() {
		if r.method == http.MethodPost && strings.HasSuffix(r.path, "/_apis/wit/wiql") {
			wiql, found = r, true
			break
		}
	}
	if !found {
		t.Fatal("no WIQL request recorded")
	}
	if !strings.Contains(wiql.rawQuery, "api-version="+apiVersion) {
		t.Errorf("WIQL query params = %q, want api-version=%s", wiql.rawQuery, apiVersion)
	}

	var payload struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(wiql.body, &payload); err != nil {
		t.Fatalf("WIQL body is not JSON: %v (%s)", err, wiql.body)
	}
	if !strings.Contains(payload.Query, "[System.AssignedTo]") || !strings.Contains(payload.Query, devAgentUser) {
		t.Errorf("WIQL query = %q, want an [System.AssignedTo] = <dev agent user> filter", payload.Query)
	}
}

func TestPoll_SkipsRepoAbsentAndUnassigned(t *testing.T) {
	t.Parallel()

	fake := newFakeADO(map[int]string{42: workItem42, 43: workItem43, 44: workItem44})
	srv := fake.start(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, logger)

	got, err := a.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].ID != "ado-baodo0220-42" {
		t.Fatalf("Poll() = %+v, want only the in-registry, assigned item ado-baodo0220-42", got)
	}

	logs := logBuf.String()
	// The repo-absent item (43) and the unassigned item (44) are recorded skips,
	// never silent defaults (AC-4.7, AC-4.2).
	if !strings.Contains(logs, `"work_item_id":"43"`) {
		t.Errorf("no recorded skip for the repo-absent work item 43; logs=%s", logs)
	}
	if !strings.Contains(logs, `"work_item_id":"44"`) {
		t.Errorf("no recorded skip for the unassigned work item 44; logs=%s", logs)
	}
}

func TestPoll_StableIDIsIdempotent(t *testing.T) {
	t.Parallel()

	fake := newFakeADO(map[int]string{42: workItem42})
	srv := fake.start(t)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	first, err := a.Poll(context.Background())
	if err != nil {
		t.Fatalf("first Poll() error = %v", err)
	}
	second, err := a.Poll(context.Background())
	if err != nil {
		t.Fatalf("second Poll() error = %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("polls returned %d and %d contracts, want 1 each", len(first), len(second))
	}
	// A stable, tracker-derived id is what lets the dispatcher dedup on re-poll
	// (RFC-0001 AC-03); the adapter itself keeps no cursor.
	if first[0].ID != second[0].ID {
		t.Errorf("id changed across polls: %q then %q", first[0].ID, second[0].ID)
	}
}

func TestComment_IssuesPreviewPostWithTextBody(t *testing.T) {
	t.Parallel()

	fake := newFakeADO(map[int]string{42: workItem42})
	srv := fake.start(t)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	if err := a.Comment(context.Background(), "42", "queued by mandat"); err != nil {
		t.Fatalf("Comment() error = %v, want nil", err)
	}

	reqs := fake.recorded()
	if len(reqs) != 1 {
		t.Fatalf("Comment() made %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost {
		t.Errorf("method = %q, want POST", r.method)
	}
	if !strings.HasSuffix(r.path, "/baodo0220/mandat/_apis/wit/workitems/42/comments") {
		t.Errorf("path = %q, want .../workitems/42/comments", r.path)
	}
	if !strings.Contains(r.rawQuery, "api-version="+commentsAPIVersion) {
		t.Errorf("query = %q, want the preview api-version=%s", r.rawQuery, commentsAPIVersion)
	}
	if r.contentType != jsonContentType {
		t.Errorf("content-type = %q, want %q", r.contentType, jsonContentType)
	}
	if r.authz != "Bearer "+testToken {
		t.Errorf("authz = %q, want the provider's bearer token", r.authz)
	}
	var payload map[string]string
	if err := json.Unmarshal(r.body, &payload); err != nil {
		t.Fatalf("comment body is not JSON: %v (%s)", err, r.body)
	}
	if payload["text"] != "queued by mandat" {
		t.Errorf("comment body = %v, want {\"text\":\"queued by mandat\"}", payload)
	}
}

func TestApplyStatus_IssuesJSONPatchStateUpdate(t *testing.T) {
	t.Parallel()

	fake := newFakeADO(map[int]string{42: workItem42})
	srv := fake.start(t)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	if err := a.ApplyStatus(context.Background(), "42", "Resolved"); err != nil {
		t.Fatalf("ApplyStatus() error = %v, want nil", err)
	}

	reqs := fake.recorded()
	if len(reqs) != 1 {
		t.Fatalf("ApplyStatus() made %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", r.method)
	}
	if !strings.HasSuffix(r.path, "/baodo0220/mandat/_apis/wit/workitems/42") {
		t.Errorf("path = %q, want .../workitems/42", r.path)
	}
	if !strings.Contains(r.rawQuery, "api-version="+apiVersion) {
		t.Errorf("query = %q, want api-version=%s", r.rawQuery, apiVersion)
	}
	// The JSON-Patch content type is the load-bearing ADO quirk: ADO rejects a
	// state update sent as plain application/json.
	if r.contentType != jsonPatchContentType {
		t.Errorf("content-type = %q, want %q", r.contentType, jsonPatchContentType)
	}
	if r.authz != "Bearer "+testToken {
		t.Errorf("authz = %q, want the provider's bearer token", r.authz)
	}

	var patch []patchOp
	if err := json.Unmarshal(r.body, &patch); err != nil {
		t.Fatalf("patch body is not a JSON-Patch array: %v (%s)", err, r.body)
	}
	if len(patch) != 1 {
		t.Fatalf("patch has %d ops, want 1", len(patch))
	}
	if patch[0].Op != "add" || patch[0].Path != "/fields/System.State" || patch[0].Value != "Resolved" {
		t.Errorf("patch op = %+v, want add /fields/System.State = Resolved", patch[0])
	}
}

func TestNew_RejectsMissingConfig(t *testing.T) {
	t.Parallel()

	_, err := New(Config{BaseURL: "https://dev.azure.com"})
	if err == nil {
		t.Fatal("New() with an incomplete config: error = nil, want a missing-field error")
	}
	// One-pass validation names every gap, matching the config/task packages.
	for _, field := range []string{"Org", "Project", "Role", "DevAgentUser", "Tokens", "Remits"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("New() error = %q, want it to name missing field %q", err, field)
		}
	}
}
