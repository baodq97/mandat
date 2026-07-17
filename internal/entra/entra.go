// Package entra implements the read (reuse) side and the agent-identity write
// side of the Microsoft Entra Agent ID registry over stdlib net/http: given a
// Graph bearer token, it enumerates the installation's agent-identity blueprint,
// the agent identities registered under it, and the agent users, then pairs each
// user to its identity — and, on the write side, idempotently ensures an agent
// identity exists (list-then-create) and best-effort deletes a throwaway one.
//
// This is the ensure-read half of US-0014's provisioning ladder — the "auto
// when possible" reuse path that discovers what already exists before any
// create. The three read endpoints are pinned to the Graph v1.0 surface the
// research doc verified against the dogfood tenant (docs/research/
// entra-agent-id-provisioning-surface.md), in the OData cast form Graph uses to
// project the Agent ID resource types:
//
//	GET {graph}/applications/microsoft.graph.agentIdentityBlueprint
//	GET {graph}/servicePrincipals/microsoft.graph.agentIdentity?$top=100
//	GET {graph}/users/microsoft.graph.agentUser?$top=100&$select=...
//
// The write side issues the create mutation behind EnsureAgentIdentity against
// the same v1.0 surface (research write-surface step 4):
//
//	POST {graph}/servicePrincipals/microsoft.graph.agentIdentity
//
// A write that returns 403 becomes a *PrivilegeError naming the missing
// capability, so provision fails with guidance instead of a raw 403 (US-0014
// AC-14.4); every mutation is printable through a WriteCall before it is issued
// (AC-14.7).
//
// The Graph base URL is overridable through Config so a contract test points the
// whole chain at one httptest server. The token never lands on the Client: it is
// minted through TokenSource on each call, set only on the Authorization header,
// and never logged or persisted (US-0014 AC-14.1/AC-14.8).
package entra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultGraphBaseURL = "https://graph.microsoft.com/v1.0"

	// graphResource is the resource az mints a Graph bearer token for — the
	// Graph seam of the same az-shellout ADO discovery uses (internal/discovery).
	graphResource = "https://graph.microsoft.com"

	jsonContentType    = "application/json"
	defaultHTTPTimeout = 30 * time.Second
	maxResponseBytes   = 4 << 20
)

// APIError is the typed error do returns on a non-2xx Graph response, mirroring
// discovery.APIError: it carries the HTTP status and a bounded slice of the
// response body so a caller distinguishes a 401/403 auth failure from a 5xx
// outage (errors.As) instead of string-matching a flattened error.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("entra: status %d: %s", e.Status, e.Body)
}

// PrivilegeError is the typed error a Graph *write* returns on HTTP 403: the
// token authenticated but the caller lacks the capability the mutation needs.
// Unlike a *APIError (any other non-2xx), it carries the attempted method and
// endpoint and names the capability likely missing — the Agent ID Developer or
// Administrator role, or the AgentIdentity.Create.All delegated scope the Azure
// CLI first-party client cannot request (research spike round 1) — so provision
// prints an actionable fail-with-guidance line rather than a raw 403 (US-0014
// AC-14.4). Recover it with errors.As; a forbidden write is never also a
// *APIError.
type PrivilegeError struct {
	Method   string
	Endpoint string
	Body     string
}

func (e *PrivilegeError) Error() string {
	return fmt.Sprintf("entra: %s %s forbidden (403): the token lacks the capability this write needs — "+
		"likely the Agent ID Developer or Administrator role, or the AgentIdentity.Create.All delegated scope "+
		"the Azure CLI token cannot request: %s", e.Method, e.Endpoint, e.Body)
}

// Blueprint is the installation's agent-identity blueprint: the application
// object every agent identity is registered under. AppID is the value config
// records and the reuse path reports (US-0014 AC-14.2).
type Blueprint struct {
	ID          string
	AppID       string
	DisplayName string
}

// AgentIdentity is one agent identity (a service principal) registered under
// the blueprint — the machine principal a RoleAgent acts as.
type AgentIdentity struct {
	ID          string
	DisplayName string
}

// AgentUser is the agent user paired 1:1 to an AgentIdentity. IdentityParentID
// is the Graph-enforced link back to that identity's id; a duplicate link is
// rejected 400, so at most one user names a given identity as parent.
type AgentUser struct {
	ID                string
	DisplayName       string
	UserPrincipalName string
	IdentityParentID  string
}

// Registry is the composed read of the three surfaces: the installation's
// blueprint (nil when none exists yet), every agent identity, and every agent
// user. PairedUser resolves an identity to its user through IdentityParentID.
type Registry struct {
	Blueprint  *Blueprint
	Identities []AgentIdentity
	Users      []AgentUser
}

// PairedUser returns the agent user linked to identity through its
// IdentityParentID (the 1:1 link Graph enforces). ok is false when no user
// names identity as its parent — a server that omits identityParentId yields no
// pairing, and the caller lists the identity and users unpaired rather than
// guessing.
func (r Registry) PairedUser(identity AgentIdentity) (AgentUser, bool) {
	for _, u := range r.Users {
		if u.IdentityParentID != "" && u.IdentityParentID == identity.ID {
			return u, true
		}
	}
	return AgentUser{}, false
}

// TokenSource obtains a Graph bearer token. It is a func type, not a hardcoded
// az invocation, so a test supplies a fake token with no az call (mirrors
// init.go's tokenSource seam).
type TokenSource func(ctx context.Context) (string, error)

// AzureCLIGraphTokenSource is the production TokenSource: it shells out to az
// for a Graph-scoped bearer token, the same shape as discovery's ADO token
// source but for the Graph resource. When az is missing or the operator is
// logged out it fails fast rather than prompting or blocking, so the caller
// surfaces the auth gap immediately. The token is returned to the in-process
// caller only and never logged or persisted.
func AzureCLIGraphTokenSource(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "az", "account", "get-access-token",
		"--resource", graphResource, "--query", "accessToken", "-o", "tsv").Output()
	if err != nil {
		return "", fmt.Errorf("az account get-access-token: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("az account get-access-token returned no token")
	}
	return token, nil
}

// Config points a Client at the Graph host and its token source. GraphBaseURL
// defaults to the production v1.0 host when empty; a test overrides it with an
// httptest server's URL. HTTPClient defaults to a client with defaultHTTPTimeout
// when nil; TokenSource defaults to AzureCLIGraphTokenSource when nil.
type Config struct {
	GraphBaseURL string
	HTTPClient   *http.Client
	TokenSource  TokenSource
}

// Client runs the Agent ID registry reads against the Graph host fixed at
// construction, minting a token per call through its TokenSource.
type Client struct {
	base   *url.URL
	client *http.Client
	tokens TokenSource
}

// New validates cfg's Graph base URL (defaulting it to the production host when
// empty) and returns a Client.
func New(cfg Config) (*Client, error) {
	graphBaseURL := cfg.GraphBaseURL
	if graphBaseURL == "" {
		graphBaseURL = defaultGraphBaseURL
	}
	base, err := parseAbsoluteURL(graphBaseURL)
	if err != nil {
		return nil, fmt.Errorf("entra: New: GraphBaseURL: %w", err)
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	tokens := cfg.TokenSource
	if tokens == nil {
		tokens = AzureCLIGraphTokenSource
	}

	return &Client{base: base, client: client, tokens: tokens}, nil
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

// ListBlueprints reads the agent-identity blueprints visible to the token. An
// installation owns at most one; a caller treats the first as the installation
// blueprint and an empty list as "none provisioned yet".
func (c *Client) ListBlueprints(ctx context.Context) ([]Blueprint, error) {
	var resp blueprintsResponse
	if err := c.do(ctx, c.blueprintsURL(), &resp); err != nil {
		return nil, fmt.Errorf("entra: list blueprints: %w", err)
	}
	out := make([]Blueprint, len(resp.Value))
	for i, b := range resp.Value {
		out[i] = Blueprint(b)
	}
	return out, nil
}

// ListAgentIdentities reads the agent identities registered under the
// blueprint.
func (c *Client) ListAgentIdentities(ctx context.Context) ([]AgentIdentity, error) {
	var resp agentIdentitiesResponse
	if err := c.do(ctx, c.agentIdentitiesURL(), &resp); err != nil {
		return nil, fmt.Errorf("entra: list agent identities: %w", err)
	}
	out := make([]AgentIdentity, len(resp.Value))
	for i, a := range resp.Value {
		out[i] = AgentIdentity(a)
	}
	return out, nil
}

// ListAgentUsers reads the agent users, selecting the identityParentId link so
// the caller can pair each user to its identity.
func (c *Client) ListAgentUsers(ctx context.Context) ([]AgentUser, error) {
	var resp agentUsersResponse
	if err := c.do(ctx, c.agentUsersURL(), &resp); err != nil {
		return nil, fmt.Errorf("entra: list agent users: %w", err)
	}
	out := make([]AgentUser, len(resp.Value))
	for i, u := range resp.Value {
		out[i] = AgentUser(u)
	}
	return out, nil
}

// DiscoverRegistry composes the three reads into a Registry: the installation
// blueprint (the first blueprint returned, nil when none exists), every agent
// identity, and every agent user. It is the reuse path — it creates nothing and
// mutates nothing. Pairing is resolved on demand through Registry.PairedUser.
func (c *Client) DiscoverRegistry(ctx context.Context) (Registry, error) {
	blueprints, err := c.ListBlueprints(ctx)
	if err != nil {
		return Registry{}, err
	}
	identities, err := c.ListAgentIdentities(ctx)
	if err != nil {
		return Registry{}, err
	}
	users, err := c.ListAgentUsers(ctx)
	if err != nil {
		return Registry{}, err
	}

	reg := Registry{Identities: identities, Users: users}
	if len(blueprints) > 0 {
		bp := blueprints[0]
		reg.Blueprint = &bp
	}
	return reg, nil
}

// WriteCall describes the exact Graph mutation a write method issues — its HTTP
// method, endpoint, and JSON body (nil for a body-less DELETE). The caller reads
// it to print the tenant mutation before it is issued (US-0014 AC-14.7) and to
// render the --dry-run plan without issuing a write.
type WriteCall struct {
	Method   string
	Endpoint string
	Body     []byte
}

// AgentIdentityCreateCall returns the exact write CreateAgentIdentity issues for
// displayName under blueprintID, sponsored by sponsors. It is exposed so
// provision can print the POST (method, endpoint, and the full body — including
// the sponsor @odata.bind list) before issuing it and render the identical call
// under --dry-run without a write. Sponsor ids are rendered against the Client's
// own Graph base so the printed and issued bodies agree (a test asserts them;
// live uses the real host).
func (c *Client) AgentIdentityCreateCall(blueprintID, displayName string, sponsors []string) (WriteCall, error) {
	binds := make([]string, len(sponsors))
	for i, id := range sponsors {
		binds[i] = c.sponsorBindURL(id)
	}
	body, err := json.Marshal(agentIdentityCreateBody{
		DisplayName:              displayName,
		AgentIdentityBlueprintID: blueprintID,
		SponsorsODataBind:        binds,
	})
	if err != nil {
		return WriteCall{}, fmt.Errorf("marshal agent identity create body: %w", err)
	}
	return WriteCall{Method: http.MethodPost, Endpoint: c.agentIdentityCreateURL(), Body: body}, nil
}

// CreateAgentIdentity registers one agent identity (a service principal) under
// the owned blueprint (blueprintID), sponsored by the human object ids in
// sponsors, through write-surface step 4:
//
//	POST {graph}/servicePrincipals/microsoft.graph.agentIdentity
//
// The v1.0 body carries three fields (agentidentity-post): displayName,
// agentIdentityBlueprintId, and sponsors@odata.bind — the dogfood live run
// returned 400 "No sponsor specified" without them. This is the blueprint-owner
// path on the operator's delegated token, proven to need no Agent ID role for
// this step (az authorized the POST live). It decodes and returns the created
// identity. A 403 surfaces as *PrivilegeError; any other non-2xx as *APIError.
func (c *Client) CreateAgentIdentity(ctx context.Context, blueprintID, displayName string, sponsors []string) (AgentIdentity, error) {
	call, err := c.AgentIdentityCreateCall(blueprintID, displayName, sponsors)
	if err != nil {
		return AgentIdentity{}, fmt.Errorf("entra: create agent identity %q: %w", displayName, err)
	}
	var created agentIdentityEntry
	if err := c.doWrite(ctx, call.Method, call.Endpoint, call.Body, &created); err != nil {
		return AgentIdentity{}, fmt.Errorf("entra: create agent identity %q: %w", displayName, err)
	}
	return AgentIdentity(created), nil
}

// EnsureAgentIdentity is the idempotent ensure for one agent identity (US-0014
// AC-14.3): it lists the registered identities and, when one already carries
// displayName, returns it with created=false and issues no write; otherwise it
// creates the identity under blueprintID sponsored by sponsors and returns it
// with created=true. The match is exact on displayName — the caller passes the
// fully-qualified role name.
func (c *Client) EnsureAgentIdentity(ctx context.Context, blueprintID, displayName string, sponsors []string) (identity AgentIdentity, created bool, err error) {
	existing, err := c.ListAgentIdentities(ctx)
	if err != nil {
		return AgentIdentity{}, false, err
	}
	for _, id := range existing {
		if id.DisplayName == displayName {
			return id, false, nil
		}
	}
	id, err := c.CreateAgentIdentity(ctx, blueprintID, displayName, sponsors)
	if err != nil {
		return AgentIdentity{}, false, err
	}
	return id, true, nil
}

func (c *Client) blueprintsURL() string {
	return c.base.JoinPath("applications", "microsoft.graph.agentIdentityBlueprint").String()
}

func (c *Client) agentIdentitiesURL() string {
	u := c.base.JoinPath("servicePrincipals", "microsoft.graph.agentIdentity")
	// Set RawQuery verbatim: the OData $-prefixed system query options are
	// left literal (url.Values.Encode would percent-encode the $) to match the
	// endpoint shape the research doc probed.
	u.RawQuery = "$top=100"
	return u.String()
}

func (c *Client) agentUsersURL() string {
	u := c.base.JoinPath("users", "microsoft.graph.agentUser")
	u.RawQuery = "$top=100&$select=id,displayName,userPrincipalName,identityParentId"
	return u.String()
}

// agentIdentityCreateURL is the write endpoint (no OData query): the identity
// collection the create POST registers a new identity into.
func (c *Client) agentIdentityCreateURL() string {
	return c.base.JoinPath("servicePrincipals", "microsoft.graph.agentIdentity").String()
}

// sponsorBindURL renders one sponsor object id as the Graph @odata.bind
// reference the create body needs — {graph}/users/{id} — against the Client's
// configured base so the printed plan and the issued write agree.
func (c *Client) sponsorBindURL(id string) string {
	return c.base.JoinPath("users", id).String()
}

// do mints a token, issues one authorized GET, and decodes its JSON body into
// out. A non-2xx status returns a *APIError (reachable with errors.As) carrying
// the status and a bounded slice of the response body. The token is set only on
// the Authorization header and never logged.
func (c *Client) do(ctx context.Context, endpoint string, out any) error {
	token, err := c.tokens(ctx)
	if err != nil {
		return fmt.Errorf("obtain Graph token: %w", err)
	}

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

// doWrite mints a token and issues one authorized write (POST or DELETE) carrying
// body, decoding a 2xx JSON response into out when out is non-nil (a 204 with an
// empty body decodes nothing). It mirrors do for writes: a 403 returns a
// *PrivilegeError naming the missing capability; any other non-2xx returns a
// *APIError; both are reachable with errors.As. The token is set only on the
// Authorization header and never logged.
func (c *Client) doWrite(ctx context.Context, method, endpoint string, body []byte, out any) error {
	token, err := c.tokens(ctx)
	if err != nil {
		return fmt.Errorf("obtain Graph token: %w", err)
	}

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", jsonContentType)
	if body != nil {
		req.Header.Set("Content-Type", jsonContentType)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, endpoint, err)
	}
	if resp.StatusCode == http.StatusForbidden {
		return &PrivilegeError{Method: method, Endpoint: endpoint, Body: strings.TrimSpace(string(respBody))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %w", method, endpoint, &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(respBody))})
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, endpoint, err)
		}
	}
	return nil
}

// blueprintsResponse is the blueprint-list envelope; each entry is one
// agent-identity blueprint the token can see.
type blueprintsResponse struct {
	Value []blueprintEntry `json:"value"`
}

type blueprintEntry struct {
	ID          string `json:"id"`
	AppID       string `json:"appId"`
	DisplayName string `json:"displayName"`
}

// agentIdentitiesResponse is the agent-identity-list envelope.
type agentIdentitiesResponse struct {
	Value []agentIdentityEntry `json:"value"`
}

type agentIdentityEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// agentIdentityCreateBody is the v1.0 create payload for write-surface step 4
// (agentidentity-post): the blueprint-owner path takes the new identity's
// displayName, the owning blueprint's id, and a Graph @odata.bind list of the
// human sponsors' /users/{id} references. The dogfood live run returned 400
// "No sponsor specified" until all three were present — an agent identity is
// sponsored by a named human (the Mandate invariant).
type agentIdentityCreateBody struct {
	DisplayName              string   `json:"displayName"`
	AgentIdentityBlueprintID string   `json:"agentIdentityBlueprintId"`
	SponsorsODataBind        []string `json:"sponsors@odata.bind"`
}

// agentUsersResponse is the agent-user-list envelope.
type agentUsersResponse struct {
	Value []agentUserEntry `json:"value"`
}

type agentUserEntry struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	UserPrincipalName string `json:"userPrincipalName"`
	IdentityParentID  string `json:"identityParentId"`
}
