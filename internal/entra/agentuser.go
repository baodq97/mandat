package entra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// AgentUserSpec describes the paired agent user to create under an agent
// identity. IdentityID is the identity's object id — the Graph-enforced 1:1
// identityParentId link; UserPrincipalName must sit under a verified tenant
// domain.
type AgentUserSpec struct {
	IdentityID        string
	DisplayName       string
	MailNickname      string
	UserPrincipalName string
}

// agentUserCreateBody is the v1.0 create payload for the paired agent user
// (write-surface step 5, agentuser-post): accountEnabled, displayName,
// mailNickname, the userPrincipalName under a verified tenant domain, and the
// identityParentId that links the user 1:1 to its agent identity. The link is
// Graph-enforced — a duplicate identityParentId is rejected 400 — so at most one
// user names a given identity as parent.
type agentUserCreateBody struct {
	AccountEnabled    bool   `json:"accountEnabled"`
	DisplayName       string `json:"displayName"`
	MailNickname      string `json:"mailNickname"`
	UserPrincipalName string `json:"userPrincipalName"`
	IdentityParentID  string `json:"identityParentId"`
}

// AgentUserCreateCall returns the exact write CreateAgentUser issues for spec —
// exposed so provision prints the POST (method, endpoint, full body) before
// issuing it (US-0014 AC-14.7) and renders the identical call under --dry-run
// without a write.
func (c *Client) AgentUserCreateCall(spec AgentUserSpec) (WriteCall, error) {
	body, err := json.Marshal(agentUserCreateBody{
		AccountEnabled:    true,
		DisplayName:       spec.DisplayName,
		MailNickname:      spec.MailNickname,
		UserPrincipalName: spec.UserPrincipalName,
		IdentityParentID:  spec.IdentityID,
	})
	if err != nil {
		return WriteCall{}, fmt.Errorf("marshal agent user create body: %w", err)
	}
	return WriteCall{Method: http.MethodPost, Endpoint: c.agentUserCreateURL(), Body: body}, nil
}

func (c *Client) agentUserCreateURL() string {
	return c.base.JoinPath("users", "microsoft.graph.agentUser").String()
}

// RetryPolicy bounds the backoff-and-retry over the create→use propagation lag
// (US-0014 AC-14.5). Attempts, Backoff, and Sleep are all overridable; Sleep is
// the seam a test swaps for a no-op so the retry logic runs without wall-clock
// delay. Zero values take live defaults sized to the ~10s lag the dogfood run
// observed.
type RetryPolicy struct {
	Attempts int
	Backoff  func(attempt int) time.Duration
	Sleep    func(ctx context.Context, d time.Duration) error
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.Attempts <= 0 {
		p.Attempts = 5
	}
	if p.Backoff == nil {
		// Linear 2s, 4s, 6s, 8s — ~20s of headroom over five attempts for a lag
		// that resolved in one ~10s retry live, without hammering Graph.
		p.Backoff = func(attempt int) time.Duration { return time.Duration(attempt) * 2 * time.Second }
	}
	if p.Sleep == nil {
		p.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	return p
}

// isPropagationLag reports whether err is the transient "identity parent not yet
// visible" failure the create→use lag produces: a 400 or 404 from the user create
// issued immediately after the identity create. A 403 fails fast (never retried),
// but the guard for that is the error TYPE, not the status list below: doWrite maps
// a 403 to *PrivilegeError, which is not an *APIError, so errors.As fails and this
// returns false. The status comparison therefore only ever sees a 400/404 from a
// well-formed request racing propagation — do not add a 403 case here expecting it
// to change retry behavior; a 403 never reaches this branch.
func isPropagationLag(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusBadRequest || apiErr.Status == http.StatusNotFound
	}
	return false
}

// CreateAgentUser issues the paired-user create, retrying with backoff over the
// create→use propagation lag (US-0014 AC-14.5): the just-created agent identity
// is not immediately visible to the user create, which 400s "IdentityParent does
// not exist" until propagation catches up (the dogfood live run needed one ~10s
// retry). Only that transient lag (a 400/404) is retried; a 403 (*PrivilegeError)
// or any other error is surfaced on the first attempt. This write uses the
// blueprint's own client-credential token (AgentIdUser.ReadWrite.IdentityParentedBy),
// not a delegated operator token — the Client's TokenSource carries that.
func (c *Client) CreateAgentUser(ctx context.Context, spec AgentUserSpec, policy RetryPolicy) (AgentUser, error) {
	call, err := c.AgentUserCreateCall(spec)
	if err != nil {
		return AgentUser{}, fmt.Errorf("entra: create agent user %q: %w", spec.UserPrincipalName, err)
	}
	policy = policy.withDefaults()

	var lastErr error
	for attempt := range policy.Attempts {
		if attempt > 0 {
			if err := policy.Sleep(ctx, policy.Backoff(attempt)); err != nil {
				return AgentUser{}, err
			}
		}
		var created agentUserEntry
		err := c.doWrite(ctx, call.Method, call.Endpoint, call.Body, &created)
		if err == nil {
			return AgentUser(created), nil
		}
		lastErr = err
		if !isPropagationLag(err) {
			return AgentUser{}, fmt.Errorf("entra: create agent user %q: %w", spec.UserPrincipalName, err)
		}
	}
	return AgentUser{}, fmt.Errorf("entra: create agent user %q: still failing after %d attempts over the create→use lag: %w",
		spec.UserPrincipalName, policy.Attempts, lastErr)
}
