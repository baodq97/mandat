package entra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	// defaultAuthorityBaseURL is the public Entra STS host the client-credentials
	// token is minted against; the token endpoint is {authority}/{tenant}/oauth2/v2.0/token.
	defaultAuthorityBaseURL = "https://login.microsoftonline.com"

	// graphDefaultScope is the .default scope that requests every application
	// permission consented to the blueprint on Microsoft Graph — the client-cred
	// grant does not enumerate scopes, it takes the consented set wholesale.
	graphDefaultScope = "https://graph.microsoft.com/.default"
)

// ClientCredential is the blueprint's proof of identity on the OAuth2
// client-credentials token request: a client secret today, a federated
// client-assertion (Azure Arc / FIC) in production. It sets exactly the
// credential fields on the token-request form and nothing else, so the token
// source stays credential-agnostic — moving a pilot from a disk secret to
// zero-secret FIC is a config swap, never a code change (the design invariant:
// production keeps no secret on disk).
type ClientCredential interface {
	apply(ctx context.Context, form url.Values) error
}

// SecretCredential authenticates the blueprint with a client secret obtained from
// an external provider at mint time — an env var or a systemd credential file,
// never a literal in config.yaml. The provider is invoked per mint so a rotated
// secret is picked up without restarting mandat, and the secret is never retained
// on the struct beyond the call that sends it.
type SecretCredential struct {
	Secret func(ctx context.Context) (string, error)
}

func (s SecretCredential) apply(ctx context.Context, form url.Values) error {
	if s.Secret == nil {
		return errors.New("SecretCredential.Secret provider is nil")
	}
	secret, err := s.Secret(ctx)
	if err != nil {
		return fmt.Errorf("read client secret: %w", err)
	}
	if strings.TrimSpace(secret) == "" {
		return errors.New("client secret provider returned empty")
	}
	form.Set("client_secret", secret)
	return nil
}

// SecretFromEnv reads the blueprint client secret from environment variable name
// at mint time. Config records only the variable NAME (a credential reference),
// never the secret value; the operator supplies the value out of band (a systemd
// EnvironmentFile, an exported shell var), so no secret lands in any file mandat
// writes (US-0014 AC-14.8).
func SecretFromEnv(name string) func(ctx context.Context) (string, error) {
	return func(context.Context) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("environment variable %q is unset or empty", name)
		}
		return v, nil
	}
}

// SecretFromFile reads the blueprint client secret from the file at path at mint
// time — the systemd LoadCredential= delivery, where the secret is decrypted into
// a file under $CREDENTIALS_DIRECTORY that only the unit's user can read. Config
// records the path (a credential reference), never the secret.
func SecretFromFile(path string) func(ctx context.Context) (string, error) {
	return func(context.Context) (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read client secret file %q: %w", path, err)
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			return "", fmt.Errorf("client secret file %q is empty", path)
		}
		return s, nil
	}
}

// ClientCredentialConfig configures a blueprint client-credentials token source.
// AuthorityBaseURL and HTTPClient are test/override seams; the rest identify the
// blueprint and how it proves itself.
type ClientCredentialConfig struct {
	TenantID   string           // the tenant the blueprint lives in
	ClientID   string           // the blueprint's appId
	Credential ClientCredential // secret today, FIC assertion in production
	Scope      string           // default graphDefaultScope

	AuthorityBaseURL string // default defaultAuthorityBaseURL (test seam)
	HTTPClient       *http.Client
}

// ClientCredentialTokenSource returns a TokenSource that mints a Graph bearer
// token AS the blueprint through the OAuth2 client-credentials grant — the
// production auth path where the blueprint acts on its own consented application
// permissions (AgentIdentity.CreateAsManager, AgentIdUser.ReadWrite.IdentityParentedBy),
// not on an operator's delegated az token. This is what makes writer≠scorer an
// IAM property independent of any human's standing privilege: the blueprint
// provisions its role identities autonomously, so a customer operator who cannot
// create agent identities directly still stands up the installation. The token is
// minted per call, set only on the Authorization header, and never logged or
// persisted (US-0014 AC-14.1/AC-14.8).
func ClientCredentialTokenSource(cfg ClientCredentialConfig) (TokenSource, error) {
	if strings.TrimSpace(cfg.TenantID) == "" {
		return nil, errors.New("entra: ClientCredentialTokenSource: TenantID is required")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("entra: ClientCredentialTokenSource: ClientID is required")
	}
	if cfg.Credential == nil {
		return nil, errors.New("entra: ClientCredentialTokenSource: Credential is required")
	}

	authority := cfg.AuthorityBaseURL
	if authority == "" {
		authority = defaultAuthorityBaseURL
	}
	base, err := parseAbsoluteURL(authority)
	if err != nil {
		return nil, fmt.Errorf("entra: ClientCredentialTokenSource: AuthorityBaseURL: %w", err)
	}
	scope := cfg.Scope
	if scope == "" {
		scope = graphDefaultScope
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	endpoint := base.JoinPath(cfg.TenantID, "oauth2", "v2.0", "token").String()

	// A per-call mint is deliberate: provision is a short-lived command issuing a
	// handful of Graph calls, so the extra token POSTs are cheap and a cache's
	// expiry bookkeeping would buy nothing. A retried write (agent-user create)
	// even benefits — each attempt carries a fresh token, ruling token staleness
	// out as a retry cause.
	return func(ctx context.Context) (string, error) {
		form := url.Values{}
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", cfg.ClientID)
		form.Set("scope", scope)
		if err := cfg.Credential.apply(ctx, form); err != nil {
			return "", fmt.Errorf("entra: client-credentials: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return "", fmt.Errorf("entra: build token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", jsonContentType)

		resp, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("entra: client-credentials token request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if err != nil {
			return "", fmt.Errorf("entra: read token response: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("entra: client-credentials token: %w", &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))})
		}

		var tok tokenResponse
		if err := json.Unmarshal(body, &tok); err != nil {
			return "", fmt.Errorf("entra: decode token response: %w", err)
		}
		if tok.AccessToken == "" {
			return "", errors.New("entra: client-credentials token response carried no access_token")
		}
		return tok.AccessToken, nil
	}, nil
}

// tokenResponse is the subset of the OAuth2 token response mandat consumes: the
// bearer token itself. expires_in is not decoded because the source mints per
// call and never caches.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
}
