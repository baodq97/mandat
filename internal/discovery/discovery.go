// Package discovery implements the Azure DevOps read-only discovery chain over
// stdlib net/http: given a bearer token for the ADO resource, it resolves the
// operator's accessible organization, that organization's projects, and each
// project's git repositories (with their remote clone URLs).
//
// The chain is pinned to four calls at api-version=7.1, in order:
//
//	GET {vssps}/_apis/profile/profiles/me            (the caller's member id)
//	GET {vssps}/_apis/accounts?memberId={id}          (accessible organizations)
//	GET {base}/{org}/_apis/projects                   (the org's projects)
//	GET {base}/{org}/{project}/_apis/git/repositories (each project's repos)
//
// The vssps and dev.azure.com hosts are both overridable through Config so a
// test can point the whole chain at a single httptest server. The token is
// never stored on the Client and never logged; it is passed to Discover and
// set only on the Authorization header of each outbound request.
//
// A caller distinguishes the four discovery outcomes without string matching:
// a nil error with a populated Result is success; errors.Is(err,
// ErrNoOrgReachable) is zero accessible organizations; errors.As(err,
// &*AmbiguousOrgError) is more than one (its Orgs field lists the names, for a
// later interactive slice to prompt with); anything else is a transport or
// auth failure, typically an *APIError from a non-2xx response.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// apiVersion pins every call in the chain to the stable ADO REST surface.
	apiVersion = "7.1"

	defaultVSSPSBaseURL       = "https://app.vssps.visualstudio.com"
	defaultAzureDevOpsBaseURL = "https://dev.azure.com"

	jsonContentType    = "application/json"
	defaultHTTPTimeout = 30 * time.Second
	maxResponseBytes   = 4 << 20
)

// ErrNoOrgReachable is returned when the token's accounts list came back
// empty: the operator has no accessible Azure DevOps organization.
var ErrNoOrgReachable = errors.New("discovery: token has access to no Azure DevOps organization")

// ErrAmbiguousOrg is the sentinel an *AmbiguousOrgError wraps. Check with
// errors.Is; use errors.As to recover the *AmbiguousOrgError and read its Orgs
// field for the list of names to prompt with.
var ErrAmbiguousOrg = errors.New("discovery: token has access to more than one Azure DevOps organization")

// AmbiguousOrgError is returned when the token's accounts list came back with
// more than one organization: discovery cannot pick one automatically, and a
// later interactive slice must prompt the operator to choose from Orgs.
type AmbiguousOrgError struct {
	Orgs []string
}

func (e *AmbiguousOrgError) Error() string {
	return fmt.Sprintf("discovery: %d Azure DevOps organizations reachable (%s); cannot resolve automatically",
		len(e.Orgs), strings.Join(e.Orgs, ", "))
}

// Is reports whether target is ErrAmbiguousOrg, so callers can branch with
// errors.Is without a type assertion.
func (e *AmbiguousOrgError) Is(target error) bool {
	return target == ErrAmbiguousOrg
}

// APIError is the typed error do() returns on a non-2xx ADO response. It
// carries the HTTP status and a bounded slice of the response body so a
// caller can distinguish, say, a 401/403 auth failure from a 5xx outage
// (errors.As), instead of string-matching a flattened error.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("discovery: status %d: %s", e.Status, e.Body)
}

// Repository is one discovered git repository: its stable id, name, and the
// remote clone URL a later slice would git-clone or push to.
type Repository struct {
	ID        string
	Name      string
	RemoteURL string
}

// Project is one discovered ADO project and its git repositories.
type Project struct {
	ID           string
	Name         string
	Repositories []Repository
}

// Org is the resolved Azure DevOps organization and its full project/repo
// tree.
type Org struct {
	ID       string
	Name     string
	Projects []Project
}

// Result is the successful discovery outcome: the single accessible
// organization, its projects, and each project's git repositories.
type Result struct {
	Org Org
}

// Config points a Client at the vssps and dev.azure.com hosts. Both base URLs
// default to the production hosts when empty; a test overrides one or both
// with an httptest server's URL. HTTPClient defaults to a client with
// defaultHTTPTimeout when nil.
type Config struct {
	VSSPSBaseURL       string
	AzureDevOpsBaseURL string
	HTTPClient         *http.Client
}

// Client runs the discovery chain against the hosts fixed at construction.
type Client struct {
	vssps  *url.URL
	base   *url.URL
	client *http.Client
}

// New validates cfg's base URLs (defaulting either that is empty to its
// production host) and returns a Client.
func New(cfg Config) (*Client, error) {
	vsspsBaseURL := cfg.VSSPSBaseURL
	if vsspsBaseURL == "" {
		vsspsBaseURL = defaultVSSPSBaseURL
	}
	adoBaseURL := cfg.AzureDevOpsBaseURL
	if adoBaseURL == "" {
		adoBaseURL = defaultAzureDevOpsBaseURL
	}

	vssps, err := parseAbsoluteURL(vsspsBaseURL)
	if err != nil {
		return nil, fmt.Errorf("discovery: New: VSSPSBaseURL: %w", err)
	}
	base, err := parseAbsoluteURL(adoBaseURL)
	if err != nil {
		return nil, fmt.Errorf("discovery: New: AzureDevOpsBaseURL: %w", err)
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}

	return &Client{vssps: vssps, base: base, client: client}, nil
}

func parseAbsoluteURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%q must be an absolute http(s) URL", raw)
	}
	return u, nil
}

// Discover runs the pinned chain for token: resolve the member profile, list
// accessible organizations, and, when exactly one is reachable, enumerate its
// projects and each project's repositories. token is set only on the
// Authorization header of each request and is never logged or retained
// between calls.
func (c *Client) Discover(ctx context.Context, token string) (Result, error) {
	if token == "" {
		return Result{}, errors.New("discovery: token is required")
	}

	var profile profileResponse
	if err := c.do(ctx, c.profileURL(), token, &profile); err != nil {
		return Result{}, fmt.Errorf("discovery: resolve member profile: %w", err)
	}
	if profile.ID == "" {
		return Result{}, errors.New("discovery: profile response carried no member id")
	}

	var accounts accountsResponse
	if err := c.do(ctx, c.accountsURL(profile.ID), token, &accounts); err != nil {
		return Result{}, fmt.Errorf("discovery: list accounts for member %s: %w", profile.ID, err)
	}

	switch len(accounts.Value) {
	case 0:
		return Result{}, ErrNoOrgReachable
	case 1:
		// The lone accessible org is the one to resolve further.
	default:
		names := make([]string, len(accounts.Value))
		for i, acc := range accounts.Value {
			names[i] = acc.AccountName
		}
		return Result{}, &AmbiguousOrgError{Orgs: names}
	}

	org := accounts.Value[0]
	var projects projectsResponse
	if err := c.do(ctx, c.projectsURL(org.AccountName), token, &projects); err != nil {
		return Result{}, fmt.Errorf("discovery: list projects for org %s: %w", org.AccountName, err)
	}

	resolved := Org{ID: org.AccountID, Name: org.AccountName, Projects: make([]Project, 0, len(projects.Value))}
	for _, p := range projects.Value {
		var repos repositoriesResponse
		if err := c.do(ctx, c.repositoriesURL(org.AccountName, p.Name), token, &repos); err != nil {
			return Result{}, fmt.Errorf("discovery: list repositories for org %s project %s: %w", org.AccountName, p.Name, err)
		}
		repositories := make([]Repository, 0, len(repos.Value))
		for _, r := range repos.Value {
			repositories = append(repositories, Repository(r))
		}
		resolved.Projects = append(resolved.Projects, Project{ID: p.ID, Name: p.Name, Repositories: repositories})
	}

	return Result{Org: resolved}, nil
}

func (c *Client) profileURL() string {
	u := c.vssps.JoinPath("_apis", "profile", "profiles", "me")
	return withAPIVersion(u)
}

func (c *Client) accountsURL(memberID string) string {
	u := c.vssps.JoinPath("_apis", "accounts")
	q := u.Query()
	q.Set("memberId", memberID)
	q.Set("api-version", apiVersion)
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) projectsURL(org string) string {
	u := c.base.JoinPath(org, "_apis", "projects")
	return withAPIVersion(u)
}

func (c *Client) repositoriesURL(org, project string) string {
	u := c.base.JoinPath(org, project, "_apis", "git", "repositories")
	return withAPIVersion(u)
}

func withAPIVersion(u *url.URL) string {
	q := u.Query()
	q.Set("api-version", apiVersion)
	u.RawQuery = q.Encode()
	return u.String()
}

// do issues one authorized GET and decodes its JSON body into out. A non-2xx
// status returns a *APIError (reachable with errors.As) carrying the status
// and a bounded slice of the response body for diagnosis.
func (c *Client) do(ctx context.Context, endpoint, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build GET %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", jsonContentType)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read GET %s response: %w", endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: %w", endpoint, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))})
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode GET %s response: %w", endpoint, err)
	}
	return nil
}

// profileResponse is the subset of the profiles/me response Discover reads:
// the member id that scopes the accounts lookup.
type profileResponse struct {
	ID string `json:"id"`
}

// accountsResponse is the accounts-list envelope; each entry is one
// organization reachable by the token's member id.
type accountsResponse struct {
	Value []accountEntry `json:"value"`
}

type accountEntry struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
}

// projectsResponse is the projects-list envelope for one organization.
type projectsResponse struct {
	Value []projectEntry `json:"value"`
}

type projectEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// repositoriesResponse is the git-repositories-list envelope for one project.
type repositoriesResponse struct {
	Value []repositoryEntry `json:"value"`
}

type repositoryEntry struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	RemoteURL string `json:"remoteUrl"`
}
