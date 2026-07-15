// Package identity is the credential plane: it mints the delegated agent-user
// token a RoleAgent acts under and hands it to git without the token ever
// escaping this process (RFC-0001 §Identity injection, ADR-0005).
//
// The token is minted through the ADR-0005 three-leg chain against the Entra
// token endpoint (login.microsoftonline.com/<tenant>/oauth2/v2.0/token), proven
// live by S1 round 3:
//
//	leg 1  the blueprint acts for the agent identity   (client_credentials + fmi_path)
//	leg 2  the agent identity does an FIC exchange      (client_credentials + client_assertion=leg1)
//	leg 3  the agent user's delegated token             (user_fic + client_assertion=leg1 + user_federated_identity_credential=leg2)
//
// Each leg carries the previous leg's assertion forward; leg 3's access token
// names the agent user (idtyp=user, upn=<agent-user>) and is what authenticates
// to Azure DevOps. MSAL exposes no user_fic grant, so the legs are raw net/http
// form posts, not an SDK call.
//
// Two invariants govern this package. The blueprint is the single credential
// holder (ADR-0005): the secret or managed-identity assertion feeds leg 1 only
// and is never logged. The delegated token never persists: it lives in the
// in-memory per-role cache below and is handed to git transiently through the
// credential helper (credential.go), never to a file, argv, or the child's env.
package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/baodq97/mandat/internal/config"
)

// AzureDevOpsResource is the well-known Entra application id for the Azure
// DevOps API. Leg 3 requests the delegated token for this resource as
// <resource>/.default (S1 round 3's aud=<ado-app-id>). It is the default the
// caller passes to NewBroker for an ADO installation.
const AzureDevOpsResource = "499b84ac-1321-427f-aa17-267ca6975798"

// defaultAuthEndpoint is the Entra token authority. Tests point endpoint at an
// httptest server; production keeps this value.
const defaultAuthEndpoint = "https://login.microsoftonline.com"

// exchangeScope is the scope legs 1 and 2 request: the federated token-exchange
// audience the chain hops through before leg 3 asks for the ADO resource (S1
// round 3, api://AzureADTokenExchange).
const exchangeScope = "api://AzureADTokenExchange/.default"

// clientAssertionType is the OAuth client_assertion_type leg 2 uses to present
// leg 1's token as its client credential (RFC 7521, jwt-bearer).
const clientAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// expiryRefreshSkew is how far before a token's exp the broker re-mints, so a
// handed-out token never expires mid-operation.
const expiryRefreshSkew = 5 * time.Minute

// maxResponseBytes bounds the token-endpoint response read; an Entra token
// response is a few kilobytes, so this only guards against a runaway body.
const maxResponseBytes = 1 << 20

// ClientCredential authenticates the blueprint in leg 1 of the chain. The
// blueprint is the only principal holding a standing credential (ADR-0005); every
// downstream leg descends from the token leg 1 mints. The interface is sealed to
// this package (unexported method) so a credential can only come from a vetted
// constructor below. Implementations must never log the secret they carry.
type ClientCredential interface {
	apply(ctx context.Context, form url.Values) error
}

// NewSecretCredential is the dev-mode blueprint credential: a client secret
// sourced from config or env by the caller and passed here. The secret is set on
// leg 1's form and is never written to a log or an error (ADR-0005 §7: no on-disk
// secret leak). Production uses NewManagedIdentityCredential instead.
func NewSecretCredential(secret string) ClientCredential {
	return secretCredential{secret: secret}
}

type secretCredential struct {
	secret string
}

func (c secretCredential) apply(_ context.Context, form url.Values) error {
	form.Set("client_secret", c.secret)
	return nil
}

// NewManagedIdentityCredential is the production blueprint credential
// (ADR-0005): the blueprint authenticates leg 1 with a managed-identity
// federated credential, so no secret reaches disk. It is a stub — the Arc/IMDS
// token acquisition and the client_assertion it sets on leg 1 are not built yet;
// apply returns an error until then.
func NewManagedIdentityCredential() ClientCredential {
	return managedIdentityCredential{}
}

type managedIdentityCredential struct{}

func (managedIdentityCredential) apply(_ context.Context, _ url.Values) error {
	// Production contract (ADR-0005): fetch a managed-identity token (Arc/IMDS)
	// and set client_assertion_type=clientAssertionType plus
	// client_assertion=<mi-token> on the leg-1 form. Not yet implemented.
	return errors.New("identity: managed-identity blueprint credential is not implemented (ADR-0005 prod path)")
}

// Broker mints and caches the delegated agent-user token per role. It reads the
// installation-scoped tenant and blueprint from EntraConfig and the role-scoped
// agent-identity and agent-user ids from the role table (config), and requests
// the delegated token for resource. Broker is safe for concurrent use.
type Broker struct {
	entra    config.EntraConfig
	roles    map[string]config.RoleConfig
	cred     ClientCredential
	resource string

	endpoint string
	client   *http.Client
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token  string
	expiry time.Time
}

// NewBroker builds a Broker from the loaded config, a blueprint credential
// (NewSecretCredential in dev, NewManagedIdentityCredential in prod), and the
// Entra resource id the delegated token targets (AzureDevOpsResource for ADO).
func NewBroker(cfg *config.Config, cred ClientCredential, resource string) *Broker {
	return &Broker{
		entra:    cfg.Entra,
		roles:    cfg.Roles,
		cred:     cred,
		resource: resource,
		endpoint: defaultAuthEndpoint,
		client:   &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
		cache:    make(map[string]cachedToken),
	}
}

// Token returns the delegated agent-user token for role, minting it through the
// three-leg chain on a cache miss and reusing the cached token until
// expiryRefreshSkew before its exp. The returned string is a credential: callers
// must never log it or write it to disk (RFC-0001 §Identity injection, AC-15).
func (b *Broker) Token(ctx context.Context, role string) (string, error) {
	// One mutex spans the cache check and the mint. Holding it across the network
	// mint serializes concurrent callers for the same role so a cache miss mints
	// once, not once per caller; the MVP runs a single task in flight (RFC-0001),
	// so the coarse lock costs nothing it needs back.
	b.mu.Lock()
	defer b.mu.Unlock()

	if c, ok := b.cache[role]; ok && b.now().Before(c.expiry.Add(-expiryRefreshSkew)) {
		return c.token, nil
	}

	token, expiry, err := b.mint(ctx, role)
	if err != nil {
		return "", err
	}
	b.cache[role] = cachedToken{token: token, expiry: expiry}
	return token, nil
}

func (b *Broker) mint(ctx context.Context, role string) (string, time.Time, error) {
	rc, ok := b.roles[role]
	if !ok {
		return "", time.Time{}, fmt.Errorf("identity: role %q is not in the config role table", role)
	}
	if rc.AgentUserID == "" {
		return "", time.Time{}, fmt.Errorf("identity: role %q has no agent_user_id; the three-leg chain needs the paired agent user (ADR-0005 agent-user-pair)", role)
	}
	if b.resource == "" {
		return "", time.Time{}, errors.New("identity: broker has no resource id for the leg-3 scope")
	}

	// Leg 1: the blueprint acts for the agent identity (client_credentials with
	// fmi_path). cred.apply supplies the blueprint's own authentication.
	leg1 := url.Values{}
	leg1.Set("client_id", b.entra.Blueprint)
	leg1.Set("grant_type", "client_credentials")
	leg1.Set("scope", exchangeScope)
	leg1.Set("fmi_path", rc.AgentIdentityID)
	if err := b.cred.apply(ctx, leg1); err != nil {
		return "", time.Time{}, fmt.Errorf("identity: leg 1 blueprint credential: %w", err)
	}
	t1, err := b.post(ctx, leg1)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: leg 1 (blueprint acts for agent identity): %w", err)
	}

	// Leg 2: the agent identity does an FIC exchange, presenting leg 1's token as
	// its client assertion.
	leg2 := url.Values{}
	leg2.Set("client_id", rc.AgentIdentityID)
	leg2.Set("grant_type", "client_credentials")
	leg2.Set("client_assertion_type", clientAssertionType)
	leg2.Set("client_assertion", t1)
	leg2.Set("scope", exchangeScope)
	t2, err := b.post(ctx, leg2)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: leg 2 (agent-identity FIC exchange): %w", err)
	}

	// Leg 3: the delegated agent-user token (user_fic), carrying leg 1 as the
	// client assertion and leg 2 as the user federated credential, scoped to the
	// ADO resource. Its claims name the agent user.
	leg3 := url.Values{}
	leg3.Set("client_id", rc.AgentIdentityID)
	leg3.Set("grant_type", "user_fic")
	leg3.Set("client_assertion", t1)
	leg3.Set("user_id", rc.AgentUserID)
	leg3.Set("user_federated_identity_credential", t2)
	leg3.Set("scope", b.resource+"/.default")
	t3, err := b.post(ctx, leg3)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: leg 3 (delegated agent-user token): %w", err)
	}

	exp, err := tokenExpiry(t3)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("identity: leg 3 token: %w", err)
	}
	return t3, exp, nil
}

// tokenResponse is the subset of the Entra token endpoint's JSON the broker
// reads: the access token on success, or the error pair on failure. Token
// lifetime comes from the JWT exp claim, not expires_in, so that field is
// deliberately not parsed.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// post sends one leg's form to <endpoint>/<tenant>/oauth2/v2.0/token and returns
// the access token. The request form holds the blueprint secret and the chained
// assertions, so it is never placed in an error; only the endpoint's own
// error/error_description (AAD diagnostic codes such as AADSTS50057, not a
// credential) is surfaced.
func (b *Broker) post(ctx context.Context, form url.Values) (string, error) {
	endpoint := b.endpoint + "/" + b.entra.Tenant + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decode token response (status %d): %w", resp.StatusCode, err)
	}
	if tr.Error != "" || resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint error (status %d): %s: %s", resp.StatusCode, tr.Error, tr.ErrorDescription)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned no access_token (status %d)", resp.StatusCode)
	}
	return tr.AccessToken, nil
}

// tokenExpiry reads the exp claim (RFC 7519, seconds since the Unix epoch) from a
// JWT without verifying its signature. The broker minted the token from a trusted
// endpoint over TLS and needs only its lifetime to schedule a re-mint, so a JWT
// library (ADR-0002 rung 3) is not warranted for one integer; the middle segment
// is base64url-decoded by hand.
func tokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("token is not a three-segment JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("decode token payload: %w", err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("parse token claims: %w", err)
	}
	if claims.Exp == 0 {
		return time.Time{}, errors.New("token has no exp claim")
	}
	return time.Unix(claims.Exp, 0).UTC(), nil
}
