package azuredevops

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// pullRequestsList7 is the canned pullrequests-list response for a found PR —
// the §9 recorded-ADO double for FindPR. It wraps forge_test.go's pullRequest7
// verbatim, so the id-7 PR object has one source shared with CreatePR's own
// fixture rather than a second hand-kept copy that could drift from it.
const pullRequestsList7 = `{"count":1,"value":[` + pullRequest7 + `]}`

// pullRequestsListEmpty is the canned response for a branch with no matching
// PR — the not-found case FindPR must map to Exists=false, nil error.
const pullRequestsListEmpty = `{"count":0,"value":[]}`

// pullRequestsListTwoActive is the canned response for two active PRs on the
// same source branch (RFC-0001's abandoned-PR false-certify hazard's positive
// case: a same-branch overlap, not an abandoned PR, since the search is
// status=active-only). The higher id (9) is listed second and carries a
// different createdBy than id 5, so the assertion proves the tie-break picks
// the highest id rather than Value[0] or Value order.
const pullRequestsListTwoActive = `{
  "count": 2,
  "value": [
    {
      "pullRequestId": 5,
      "status": "active",
      "isDraft": true,
      "sourceRefName": "refs/heads/mandat/task-42",
      "targetRefName": "refs/heads/main",
      "url": "https://dev.azure.com/baodo0220/mandat/_apis/git/repositories/mandat/pullRequests/5",
      "createdBy": {
        "id": "8c5e2f1a-0000-4000-8000-000000000002",
        "displayName": "Someone Else",
        "uniqueName": "someone-else@baotest.onmicrosoft.com"
      }
    },
    {
      "pullRequestId": 9,
      "status": "active",
      "isDraft": true,
      "sourceRefName": "refs/heads/mandat/task-42",
      "targetRefName": "refs/heads/main",
      "url": "https://dev.azure.com/baodo0220/mandat/_apis/git/repositories/mandat/pullRequests/9",
      "createdBy": {
        "id": "8c5e2f1a-0000-4000-8000-000000000001",
        "displayName": "Dev Agent 01",
        "uniqueName": "agent-user-dev-01@baotest.onmicrosoft.com"
      }
    }
  ]
}`

func TestFindPR_MapsFoundPR(t *testing.T) {
	t.Parallel()

	srv, rec := prServer(t, http.StatusOK, pullRequestsList7)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	got, err := a.FindPR(context.Background(), "mandat", "mandat/task-42")
	if err != nil {
		t.Fatalf("FindPR() error = %v, want nil", err)
	}
	if !got.Exists {
		t.Error("Exists = false, want true for a found PR")
	}
	if got.CreatedBy != devAgentUser {
		t.Errorf("CreatedBy = %q, want %q", got.CreatedBy, devAgentUser)
	}
	if !strings.HasSuffix(got.URL, "/baodo0220/mandat/_git/mandat/pullrequest/7") {
		t.Errorf("URL = %q, want the human web URL .../baodo0220/mandat/_git/mandat/pullrequest/7", got.URL)
	}

	got2 := rec.recorded(t)
	if got2.method != http.MethodGet {
		t.Errorf("method = %q, want GET", got2.method)
	}
	if !strings.HasSuffix(got2.path, "/baodo0220/mandat/_apis/git/repositories/mandat/pullrequests") {
		t.Errorf("path = %q, want .../git/repositories/mandat/pullrequests", got2.path)
	}
	q, err := url.ParseQuery(got2.rawQuery)
	if err != nil {
		t.Fatalf("query %q did not parse: %v", got2.rawQuery, err)
	}
	if q.Get("searchCriteria.sourceRefName") != "refs/heads/mandat/task-42" {
		t.Errorf("searchCriteria.sourceRefName = %q, want %q", q.Get("searchCriteria.sourceRefName"), "refs/heads/mandat/task-42")
	}
	if q.Get("searchCriteria.status") != "active" {
		t.Errorf("searchCriteria.status = %q, want %q (an abandoned PR on the same branch must never count as existing)", q.Get("searchCriteria.status"), "active")
	}
	if got2.authz != "Bearer "+testToken {
		t.Errorf("authz = %q, want the provider's bearer token", got2.authz)
	}
}

func TestFindPR_EmptyResultReturnsNotExists(t *testing.T) {
	t.Parallel()

	srv, _ := prServer(t, http.StatusOK, pullRequestsListEmpty)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	got, err := a.FindPR(context.Background(), "mandat", "mandat/task-99")
	if err != nil {
		t.Fatalf("FindPR() error = %v, want nil", err)
	}
	if got.Exists {
		t.Errorf("Exists = true, want false for an empty PR list")
	}
	if got.CreatedBy != "" || got.URL != "" {
		t.Errorf("got = %+v, want the zero PRFinding when no PR matches", got)
	}
}

// TestFindPR_MultipleActivePRsPicksHighestID proves the deterministic tie-break:
// ADO documents no ordering on the pullrequests list response, so FindPR must
// not trust Value[0] when more than one active PR matches — it picks the
// highest pullRequestId (the newest) regardless of response order.
func TestFindPR_MultipleActivePRsPicksHighestID(t *testing.T) {
	t.Parallel()

	srv, _ := prServer(t, http.StatusOK, pullRequestsListTwoActive)
	a := newAdapter(t, srv, &fakeTokenProvider{token: testToken}, nil)

	got, err := a.FindPR(context.Background(), "mandat", "mandat/task-42")
	if err != nil {
		t.Fatalf("FindPR() error = %v, want nil", err)
	}
	if got.CreatedBy != devAgentUser {
		t.Errorf("CreatedBy = %q, want %q (pullRequestId 9, the higher id)", got.CreatedBy, devAgentUser)
	}
	if !strings.HasSuffix(got.URL, "/pullrequest/9") {
		t.Errorf("URL = %q, want the pullrequest/9 URL (the higher id wins the tie-break)", got.URL)
	}
}

// TestFindPR_MintsTokenUnderAdapterRole proves the probe mints its token under
// the adapter's own configured role (Reviewer in production), never a
// hardcoded role — the IAM property that makes writer != scorer real rather
// than conventional (RFC-0001 §4.1). newAdapter pins Role to "dev", so this
// constructs the adapter directly with Role: "reviewer".
func TestFindPR_MintsTokenUnderAdapterRole(t *testing.T) {
	t.Parallel()

	srv, _ := prServer(t, http.StatusOK, pullRequestsList7)
	tp := &fakeTokenProvider{token: testToken}
	a, err := New(Config{
		BaseURL:       srv.URL,
		Org:           testOrg,
		Project:       testProject,
		Role:          "reviewer",
		AgentUserName: devAgentUser,
		Tokens:        tp,
		Remits:        registry(),
	})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	if _, err := a.FindPR(context.Background(), "mandat", "mandat/task-42"); err != nil {
		t.Fatalf("FindPR() error = %v, want nil", err)
	}
	if roles := tp.calls(); len(roles) != 1 || roles[0] != "reviewer" {
		t.Errorf("token mint calls = %v, want exactly one for role reviewer", roles)
	}
}
