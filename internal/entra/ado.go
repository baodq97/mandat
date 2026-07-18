package entra

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	// ADOAppID is Azure DevOps's well-known first-party appId — the resource an
	// agent user impersonates. Resolving it to a per-tenant service principal
	// object id yields the resourceId the oauth2PermissionGrant needs.
	ADOAppID = "499b84ac-1321-427f-aa17-267ca6975798"

	// ADOImpersonationScope is the delegated permission an agent user needs to act
	// against Azure DevOps as itself.
	ADOImpersonationScope = "user_impersonation"

	// consentTypePrincipal grants the delegated permission for one named principal
	// (the agent user), not org-wide: the live dogfood grant uses Principal, not the
	// AllPrincipals an older beta sketch showed, so the grant is scoped to exactly
	// the agent user it provisions.
	consentTypePrincipal = "Principal"
)

// ResolveServicePrincipalID resolves a service principal's object id from its
// appId through the OData (appId='...') key segment. It turns Azure DevOps's
// well-known appId into the per-tenant resourceId the ADO oauth2PermissionGrant
// binds to (the id differs per tenant; the appId does not).
func (c *Client) ResolveServicePrincipalID(ctx context.Context, appID string) (string, error) {
	var sp struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, c.servicePrincipalByAppIDURL(appID), &sp); err != nil {
		return "", fmt.Errorf("entra: resolve service principal for appId %q: %w", appID, err)
	}
	if sp.ID == "" {
		return "", fmt.Errorf("entra: service principal for appId %q has no id", appID)
	}
	return sp.ID, nil
}

func (c *Client) servicePrincipalByAppIDURL(appID string) string {
	// OData key-lookup: the (appId='...') segment is left literal — JoinPath would
	// percent-encode the parens and quotes the key syntax requires.
	return strings.TrimRight(c.base.String(), "/") + "/servicePrincipals(appId='" + appID + "')"
}

// OAuth2GrantSpec is the delegated grant that lets an agent user impersonate on a
// resource: the client (the agent identity's SP), the principal being consented
// (the agent user), the resource SP (Azure DevOps), and the scope.
type OAuth2GrantSpec struct {
	ClientID    string
	PrincipalID string
	ResourceID  string
	Scope       string
}

type oauth2GrantBody struct {
	ClientID    string `json:"clientId"`
	ConsentType string `json:"consentType"`
	PrincipalID string `json:"principalId"`
	ResourceID  string `json:"resourceId"`
	Scope       string `json:"scope"`
}

// OAuth2GrantCall returns the exact write CreateOAuth2Grant issues for spec —
// exposed so provision prints the POST (method, endpoint, full body) before
// issuing it (US-0014 AC-14.7) and renders the identical call under --dry-run and
// in the fail-with-guidance output an admin runs by hand (AC-14.4).
func (c *Client) OAuth2GrantCall(spec OAuth2GrantSpec) (WriteCall, error) {
	body, err := json.Marshal(oauth2GrantBody{
		ClientID:    spec.ClientID,
		ConsentType: consentTypePrincipal,
		PrincipalID: spec.PrincipalID,
		ResourceID:  spec.ResourceID,
		Scope:       spec.Scope,
	})
	if err != nil {
		return WriteCall{}, fmt.Errorf("marshal oauth2 grant body: %w", err)
	}
	return WriteCall{Method: http.MethodPost, Endpoint: c.oauth2GrantsURL(), Body: body}, nil
}

func (c *Client) oauth2GrantsURL() string {
	return c.base.JoinPath("oauth2PermissionGrants").String()
}

// CreateOAuth2Grant issues the delegated grant (step 6). It uses the caller's
// token — the operator's delegated session, which holds DelegatedPermissionGrant.
// ReadWrite.All — not the blueprint client-credential: admin consent is the
// operator's privileged act, not the blueprint's. A 403 surfaces as
// *PrivilegeError so the caller can print the exact call for a tenant admin.
func (c *Client) CreateOAuth2Grant(ctx context.Context, spec OAuth2GrantSpec) error {
	call, err := c.OAuth2GrantCall(spec)
	if err != nil {
		return err
	}
	if err := c.doWrite(ctx, call.Method, call.Endpoint, call.Body, nil); err != nil {
		return fmt.Errorf("entra: create oauth2 grant (client %s -> resource %s): %w", spec.ClientID, spec.ResourceID, err)
	}
	return nil
}

// HasOAuth2Grant reports whether a delegated grant already binds principalID to
// resourceID — the idempotency check so a re-run does not stack duplicate grants
// on the same agent user.
func (c *Client) HasOAuth2Grant(ctx context.Context, principalID, resourceID string) (bool, error) {
	var resp struct {
		Value []struct {
			ResourceID string `json:"resourceId"`
		} `json:"value"`
	}
	if err := c.do(ctx, c.oauth2GrantsFilterURL(principalID), &resp); err != nil {
		return false, fmt.Errorf("entra: list oauth2 grants for principal %q: %w", principalID, err)
	}
	for _, g := range resp.Value {
		if g.ResourceID == resourceID {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) oauth2GrantsFilterURL(principalID string) string {
	// The $ of the OData $filter is kept literal (like the read URLs); the filter
	// value's spaces and quotes are percent-encoded to %20/%27 so the request line
	// is valid and Graph decodes principalId eq '<id>'.
	filter := strings.ReplaceAll(url.QueryEscape("principalId eq '"+principalID+"'"), "+", "%20")
	return c.oauth2GrantsURL() + "?$filter=" + filter
}
