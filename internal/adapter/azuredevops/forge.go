package azuredevops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
}

// CreatePRResult is the parsed 201 pull-request response. CreatedBy is the
// author's UPN: opened under the delegated agent-user token, a PR is authored by
// the Dev agent user as a directory fact (spike S3), the value the verification
// plane's ground-truth probe checks against (probe_failed if it is not the Dev
// agent user, RFC-0001 state machine).
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
	u := a.base.JoinPath(a.org, a.project, "_apis", "git", "repositories", in.Repo, "pullrequests")
	q := u.Query()
	q.Set("api-version", apiVersion)
	u.RawQuery = q.Encode()

	body, err := json.Marshal(createPRRequest{
		SourceRefName: refHeadsPrefix + in.Branch,
		TargetRefName: refHeadsPrefix + in.BaseBranch,
		Title:         in.Title,
		Description:   in.Description,
		// Draft-only is the MVP autonomy ceiling (RFC-0001 §10): no code path here
		// opens a ready PR, so isDraft is a constant, not a caller option.
		IsDraft: true,
	})
	if err != nil {
		return CreatePRResult{}, fmt.Errorf("azuredevops: encode create-PR body for repo %s: %w", in.Repo, err)
	}

	var resp createPRResponse
	if err := a.do(ctx, http.MethodPost, u.String(), jsonContentType, body, &resp); err != nil {
		return CreatePRResult{}, fmt.Errorf("azuredevops: create pull request in repo %s: %w", in.Repo, err)
	}
	return CreatePRResult{
		ID:        resp.PullRequestID,
		URL:       resp.URL,
		CreatedBy: resp.CreatedBy.UniqueName,
	}, nil
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
	SourceRefName string `json:"sourceRefName"`
	TargetRefName string `json:"targetRefName"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	IsDraft       bool   `json:"isDraft"`
}

// createPRResponse is the subset of the 201 pull-request response the adapter
// reads. createdBy is an identity reference whose uniqueName is the author's UPN,
// reusing the adoIdentity shape the work-item read already decodes.
type createPRResponse struct {
	PullRequestID int         `json:"pullRequestId"`
	URL           string      `json:"url"`
	CreatedBy     adoIdentity `json:"createdBy"`
}
