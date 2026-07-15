// Package azuredevops implements the tracker.Tracker seam against Azure DevOps
// over stdlib net/http (ADR-0002 rung 1: WIQL, work-item read, comment, and a
// state PATCH are a handful of REST calls that stay below the bar where the
// azure-devops-go SDK earns a direct dependency and its transitive cgo/version
// surface). It polls WIQL for work items assigned to a role's agent user, maps
// each to a task.TaskContract with remit filled from the repo-registry defaults
// (RFC-0001 decision 4), and writes comments and state changes back.
//
// The adapter never mints a token. It takes a TokenProvider and sets
// Authorization: Bearer on every request from it, so internal/identity's broker
// is injected at cmd wiring and the delegated agent-user token is the only
// credential the ADO API ever sees (spec §4.2: every tracker write uses the
// acting RoleAgent's token, so the audit history reads like a team's, not a
// bot's). Remit likewise comes through an injected RemitSource, not a hard
// dependency on internal/config's concrete type.
package azuredevops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/baodq97/mandat/internal/config"
	"github.com/baodq97/mandat/internal/tracker"
)

const (
	// apiVersion pins the stable ADO REST surface for WIQL, work-item read, and
	// the state PATCH.
	apiVersion = "7.1"
	// commentsAPIVersion is separate because the work-item comments resource has
	// never been promoted out of preview; the exact preview minor is confirmed
	// against live ADO at integration (AC-4.4's fixture double does not police
	// the version), so it is isolated here as a one-line bump target.
	commentsAPIVersion = "7.1-preview.3"

	jsonContentType      = "application/json"
	jsonPatchContentType = "application/json-patch+json"

	defaultHTTPTimeout = 30 * time.Second
	maxResponseBytes   = 4 << 20
)

// TokenProvider mints the delegated agent-user access token that authorizes an
// ADO call for role. It is the consumer-side seam that keeps token minting out
// of the adapter (spec §4.2, RFC-0001 §Identity injection): internal/identity's
// broker satisfies this signature structurally and is injected at cmd wiring; a
// fake stands in for the contract test. role is the RoleAgent key whose identity
// the call acts as, so the broker mints for the correct principal.
type TokenProvider interface {
	Token(ctx context.Context, role string) (string, error)
}

// RemitSource resolves the repo-registry remit defaults for a repo key. It is
// the consumer-side view of internal/config: *config.Config satisfies it
// structurally, and the adapter depends on the one method it needs rather than
// the whole config type. A repo absent from the registry returns an error, which
// the adapter turns into a recorded skip, never a silent default (RFC-0001
// AC-08).
type RemitSource interface {
	RemitDefaultsFor(repo string) (config.RemitDefaults, error)
}

// Config constructs an Adapter. BaseURL is the ADO host root
// (https://dev.azure.com in production, an httptest server in the contract
// test); Org and Project scope every REST path; Role names the RoleAgent whose
// token authorizes calls and whose key lands on each produced contract;
// DevAgentUser is the agent-user principal WIQL filters assignment on (consent =
// assignment, spec §4.2). Tokens and Remits are required injected seams;
// HTTPClient and Logger default when nil.
type Config struct {
	BaseURL      string
	Org          string
	Project      string
	Role         string
	DevAgentUser string
	Tokens       TokenProvider
	Remits       RemitSource
	HTTPClient   *http.Client
	Logger       *slog.Logger
}

// Adapter is the Azure DevOps tracker.Tracker. It is safe for the single
// in-flight poll the skeleton drives (RFC-0001 §Scope); it holds no per-poll
// mutable state, so dedup of an already-seen work item rides the stable
// contract id downstream, not adapter memory.
type Adapter struct {
	base         *url.URL
	org          string
	project      string
	role         string
	devAgentUser string
	tokens       TokenProvider
	remits       RemitSource
	client       *http.Client
	logger       *slog.Logger
}

var _ tracker.Tracker = (*Adapter)(nil)

// New validates the required config and returns an Adapter. It collects every
// missing field into one error rather than failing on the first, matching the
// config/task packages' one-pass validation style.
func New(cfg Config) (*Adapter, error) {
	var missing []string
	if cfg.BaseURL == "" {
		missing = append(missing, "BaseURL")
	}
	if cfg.Org == "" {
		missing = append(missing, "Org")
	}
	if cfg.Project == "" {
		missing = append(missing, "Project")
	}
	if cfg.Role == "" {
		missing = append(missing, "Role")
	}
	if cfg.DevAgentUser == "" {
		missing = append(missing, "DevAgentUser")
	}
	if cfg.Tokens == nil {
		missing = append(missing, "Tokens")
	}
	if cfg.Remits == nil {
		missing = append(missing, "Remits")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("azuredevops: New: missing required config: %s", strings.Join(missing, ", "))
	}

	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("azuredevops: New: parse BaseURL %q: %w", cfg.BaseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("azuredevops: New: BaseURL %q must be an absolute http(s) URL", cfg.BaseURL)
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Adapter{
		base:         base,
		org:          cfg.Org,
		project:      cfg.Project,
		role:         cfg.Role,
		devAgentUser: cfg.DevAgentUser,
		tokens:       cfg.Tokens,
		remits:       cfg.Remits,
		client:       client,
		logger:       logger,
	}, nil
}

// Comment posts a discussion comment onto the work item under the role's
// identity. The comments resource lives at .../workitems/{id}/comments on the
// preview api-version and takes a {"text": ...} body — a different shape from
// the JSON-Patch a field update uses (see ApplyStatus).
func (a *Adapter) Comment(ctx context.Context, workItemID, text string) error {
	ep := a.endpoint(commentsAPIVersion, "workitems", workItemID, "comments")
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("azuredevops: encode comment for work item %s: %w", workItemID, err)
	}
	if err := a.do(ctx, http.MethodPost, ep, jsonContentType, body, nil); err != nil {
		return fmt.Errorf("azuredevops: comment on work item %s: %w", workItemID, err)
	}
	return nil
}

// ApplyStatus sets the work item's System.State to the target tracker state.
// ADO mutates a work item through a JSON-Patch document sent as
// application/json-patch+json; it rejects the plain application/json content
// type. `op: add` on a field path upserts the value, which is how a state
// transition is expressed over REST.
func (a *Adapter) ApplyStatus(ctx context.Context, workItemID, status string) error {
	ep := a.endpoint(apiVersion, "workitems", workItemID)
	patch := []patchOp{{Op: "add", Path: "/fields/System.State", Value: status}}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("azuredevops: encode status patch for work item %s: %w", workItemID, err)
	}
	if err := a.do(ctx, http.MethodPatch, ep, jsonPatchContentType, body, nil); err != nil {
		return fmt.Errorf("azuredevops: apply status %q to work item %s: %w", status, workItemID, err)
	}
	return nil
}

// patchOp is one operation in an ADO JSON-Patch document (RFC 6902). Value is
// any because a work-item field can hold a string, number, or object.
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// endpoint builds an absolute .../{org}/{project}/_apis/wit/<segments> URL with
// the api-version query parameter. JoinPath escapes each segment, so an org or
// project name with a space stays well-formed.
func (a *Adapter) endpoint(apiVer string, segments ...string) string {
	u := a.base.JoinPath(append([]string{a.org, a.project, "_apis", "wit"}, segments...)...)
	q := u.Query()
	q.Set("api-version", apiVer)
	u.RawQuery = q.Encode()
	return u.String()
}

// workItemURL is the human-facing edit URL persisted in tracker_ref.url. It is
// constructed rather than read from the work-item response's `url` field, which
// is the API self-link (.../_apis/wit/...), not the browser URL a human opens.
func (a *Adapter) workItemURL(workItemID string) string {
	return a.base.JoinPath(a.org, a.project, "_workitems", "edit", workItemID).String()
}

// do issues one authorized ADO request. It mints a fresh token per call so the
// credential stays short-lived and bound to the single operation (spec §4.2);
// out, when non-nil, receives the decoded JSON body. A non-2xx status returns a
// *APIError (reachable with errors.As) carrying the status and a bounded slice of
// the response body for diagnosis.
func (a *Adapter) do(ctx context.Context, method, endpoint, contentType string, body []byte, out any) error {
	token, err := a.tokens.Token(ctx, a.role)
	if err != nil {
		return fmt.Errorf("mint token for role %q: %w", a.role, err)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", jsonContentType)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %w", method, endpoint, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(respBody))})
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, endpoint, err)
		}
	}
	return nil
}
