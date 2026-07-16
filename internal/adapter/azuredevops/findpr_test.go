package azuredevops

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// pullRequestsList7 is the canned pullrequests-list response for a found PR —
// the §9 recorded-ADO double for FindPR, reusing pullRequest7's createdBy so
// both fixtures agree on the Dev agent user.
const pullRequestsList7 = `{
  "count": 1,
  "value": [
    {
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
    }
  ]
}`

// pullRequestsListEmpty is the canned response for a branch with no matching
// PR — the not-found case FindPR must map to Exists=false, nil error.
const pullRequestsListEmpty = `{"count":0,"value":[]}`

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
	if q.Get("searchCriteria.status") != "all" {
		t.Errorf("searchCriteria.status = %q, want %q", q.Get("searchCriteria.status"), "all")
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
