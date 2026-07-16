package azuredevops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// refHeadsPrefix qualifies a bare branch name into the git ref namespace. ADO's
// pull-request create takes fully-qualified refs (refs/heads/<branch>), never the
// short name, so the adapter prefixes the remit's branch names on the way out.
const refHeadsPrefix = "refs/heads/"

// CreatePRInput is the caller's request to open a pull request. Branch and
// BaseBranch are short names (mandat/task-42, main) the adapter qualifies to
// refs/heads/... on the wire; Repo is the ADO repository id or name under the
// adapter's org/project.
type CreatePRInput struct {
	Repo        string
	Branch      string
	BaseBranch  string
	Title       string
	Description string
	// WorkItemID, when set, links the PR to the source work item at creation
	// (workItemRefs on the wire). Empty omits the link entirely.
	WorkItemID string
}

// CreatePRResult is the parsed 201 pull-request response. CreatedBy is the
// author's UPN: opened under the delegated agent-user token, a PR is authored by
// the Dev agent user as a directory fact (spike S3), the value the verification
// plane's ground-truth probe checks against (probe_failed if it is not the Dev
// agent user, RFC-0001 state machine). URL is the human web URL
// (base/org/project/_git/{repo}/pullrequest/{id}) built by pullRequestURL, not
// the API self-link the response's own url field carries.
type CreatePRResult struct {
	ID        int
	URL       string
	CreatedBy string
}

// CreatePR opens a draft pull request from the source branch to the base branch
// under the role's delegated agent-user token (the Bearer seam do() sets — no
// PAT, spike S3). The PR resource lives under _apis/git, not the _apis/wit area
// endpoint() builds, so its path is assembled here.
func (a *Adapter) CreatePR(ctx context.Context, in CreatePRInput) (CreatePRResult, error) {
	req := createPRRequest{
		SourceRefName: refHeadsPrefix + in.Branch,
		TargetRefName: refHeadsPrefix + in.BaseBranch,
		Title:         in.Title,
		Description:   in.Description,
		// Draft-only is the MVP autonomy ceiling (RFC-0001 §10): no code path here
		// opens a ready PR, so isDraft is a constant, not a caller option.
		IsDraft: true,
	}
	if in.WorkItemID != "" {
		req.WorkItemRefs = []workItemRef{{ID: in.WorkItemID}}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return CreatePRResult{}, fmt.Errorf("azuredevops: encode create-PR body for repo %s: %w", in.Repo, err)
	}

	var resp createPRResponse
	if err := a.do(ctx, http.MethodPost, a.pullRequestsURL(in.Repo).String(), jsonContentType, body, &resp); err != nil {
		return CreatePRResult{}, fmt.Errorf("azuredevops: create pull request in repo %s: %w", in.Repo, err)
	}
	return CreatePRResult{
		ID:        resp.PullRequestID,
		URL:       a.pullRequestURL(in.Repo, resp.PullRequestID),
		CreatedBy: resp.CreatedBy.UniqueName,
	}, nil
}

// PRFinding is the adapter-local result of the PR-existence probe (RFC-0001
// AC-27). It stays local to this package rather than internal/verify's PRInfo
// so the adapter never imports internal/verify; the caller at cmd wiring maps
// it into verify.PRInfo. Exists is false with a nil error when no PR matches
// the source branch. CreatedBy is the PR author's UPN, and URL is the human
// web URL (base/org/project/_git/{repo}/pullrequest/{id}), not the API
// self-link CreatePRResult.URL carries.
type PRFinding struct {
	Exists    bool
	CreatedBy string
	URL       string
}

// FindPR looks up the active pull request open from branch in repo, authorized
// through the adapter's own role — a reviewer-role Adapter instance mints
// Reviewer tokens, so the call is the out-of-band probe a distinct principal
// from the Dev agent user makes (writer != scorer, RFC-0001 §4.1). The search
// is restricted to status=active rather than "all": the run's own draft PR is
// always active in this slice, and ADO documents no ordering on the list
// response, so a same-branch re-run whose earlier PR was abandoned could have
// that dead PR sort first and get false-certified as existing under an
// unfiltered search. When more than one active PR matches, the tie-break picks
// the highest pullRequestId (the newest) deterministically rather than
// trusting response order.
func (a *Adapter) FindPR(ctx context.Context, repo, branch string) (PRFinding, error) {
	u := a.pullRequestsURL(repo)
	q := u.Query()
	q.Set("searchCriteria.sourceRefName", refHeadsPrefix+branch)
	q.Set("searchCriteria.status", "active")
	u.RawQuery = q.Encode()

	var resp findPRResponse
	if err := a.do(ctx, http.MethodGet, u.String(), "", nil, &resp); err != nil {
		return PRFinding{}, fmt.Errorf("azuredevops: find pull request for repo %s branch %s: %w", repo, branch, err)
	}
	if len(resp.Value) == 0 {
		return PRFinding{}, nil
	}
	pr := resp.Value[0]
	for _, candidate := range resp.Value[1:] {
		if candidate.PullRequestID > pr.PullRequestID {
			pr = candidate
		}
	}
	return PRFinding{
		Exists:    true,
		CreatedBy: pr.CreatedBy.UniqueName,
		URL:       a.pullRequestURL(repo, pr.PullRequestID),
	}, nil
}

// pullRequestURL is the human web URL a browser opens for a pull request,
// distinct from the API self-link the list response's own `url` field carries
// (mirrors workItemURL's same distinction for work items).
func (a *Adapter) pullRequestURL(repo string, id int) string {
	return a.base.JoinPath(a.org, a.project, "_git", repo, "pullrequest", strconv.Itoa(id)).String()
}

// pullRequestsURL builds the .../{org}/{project}/_apis/git/repositories/{repo}/pullrequests
// URL with the api-version query set, the _apis/git sibling of endpoint()'s
// _apis/wit area. It returns a *url.URL rather than a string, unlike endpoint(),
// because FindPR layers its own searchCriteria params on top of the api-version
// query this helper already sets.
func (a *Adapter) pullRequestsURL(repo string) *url.URL {
	u := a.base.JoinPath(a.org, a.project, "_apis", "git", "repositories", repo, "pullrequests")
	q := u.Query()
	q.Set("api-version", apiVersion)
	u.RawQuery = q.Encode()
	return u
}

// findPRResponse is the pullrequests list envelope; each entry reuses
// createPRResponse's shape since the list and create responses carry the same
// pull-request fields.
type findPRResponse struct {
	Value []createPRResponse `json:"value"`
}

// APIError is the typed error do() returns on a non-2xx ADO response. It carries
// the HTTP status and the raw response body (ADO's {"message": ...} envelope) so
// a caller can branch on the status — a PR create colliding on an existing
// source/target, say — instead of string-matching a flattened error; do() wraps
// it with the method and endpoint, and callers reach it with errors.As.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("status %d: %s", e.Status, e.Body)
}

type createPRRequest struct {
	SourceRefName string        `json:"sourceRefName"`
	TargetRefName string        `json:"targetRefName"`
	Title         string        `json:"title"`
	Description   string        `json:"description"`
	IsDraft       bool          `json:"isDraft"`
	WorkItemRefs  []workItemRef `json:"workItemRefs,omitempty"`
}

// workItemRef links a PR to a work item by id on create; ADO accepts the id as
// a string in this wire shape regardless of the work item's numeric id.
type workItemRef struct {
	ID string `json:"id"`
}

// createPRResponse is the subset of the 201 pull-request response the adapter
// reads. createdBy is an identity reference whose uniqueName is the author's UPN,
// reusing the adoIdentity shape the work-item read already decodes.
type createPRResponse struct {
	PullRequestID int         `json:"pullRequestId"`
	CreatedBy     adoIdentity `json:"createdBy"`
}
