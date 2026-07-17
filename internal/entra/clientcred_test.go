package entra

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tokenEndpoint is a fake Entra STS: it records the form each token request
// posts and replies with a canned access_token (or a scripted error), so the
// client-credentials mint is proven offline with no login.microsoftonline.com
// dial and no real secret.
type tokenEndpoint struct {
	mu    sync.Mutex
	forms []url.Values

	status int
	body   string
}

func (e *tokenEndpoint) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		e.mu.Lock()
		e.forms = append(e.forms, form)
		e.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if e.status != 0 {
			w.WriteHeader(e.status)
		}
		_, _ = w.Write([]byte(e.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *tokenEndpoint) recorded() []url.Values {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]url.Values, len(e.forms))
	copy(out, e.forms)
	return out
}

func staticSecret(s string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return s, nil }
}

func TestClientCredentialTokenSource_MintsBearerTokenFromSecret(t *testing.T) {
	t.Parallel()

	sts := &tokenEndpoint{body: `{"access_token":"minted-blueprint-token","token_type":"Bearer","expires_in":3599}`}
	srv := sts.start(t)

	src, err := ClientCredentialTokenSource(ClientCredentialConfig{
		TenantID:         "tenant-01",
		ClientID:         "blueprint-appid-01",
		Credential:       SecretCredential{Secret: staticSecret("s3cr3t")},
		AuthorityBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("ClientCredentialTokenSource() error = %v, want nil", err)
	}

	token, err := src(context.Background())
	if err != nil {
		t.Fatalf("mint error = %v, want nil", err)
	}
	if token != "minted-blueprint-token" {
		t.Errorf("token = %q, want minted-blueprint-token", token)
	}

	forms := sts.recorded()
	if len(forms) != 1 {
		t.Fatalf("token requests = %d, want 1", len(forms))
	}
	f := forms[0]
	if got := f.Get("grant_type"); got != "client_credentials" {
		t.Errorf("grant_type = %q, want client_credentials", got)
	}
	if got := f.Get("client_id"); got != "blueprint-appid-01" {
		t.Errorf("client_id = %q, want the blueprint appId", got)
	}
	if got := f.Get("client_secret"); got != "s3cr3t" {
		t.Errorf("client_secret = %q, want the provided secret", got)
	}
	if got := f.Get("scope"); got != graphDefaultScope {
		t.Errorf("scope = %q, want %q (all consented Graph app permissions)", got, graphDefaultScope)
	}
}

func TestClientCredentialTokenSource_HitsTenantTokenEndpoint(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"access_token":"t"}`))
	}))
	t.Cleanup(srv.Close)

	src, err := ClientCredentialTokenSource(ClientCredentialConfig{
		TenantID:         "tenant-xyz",
		ClientID:         "app",
		Credential:       SecretCredential{Secret: staticSecret("x")},
		AuthorityBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("ClientCredentialTokenSource() error = %v", err)
	}
	if _, err := src(context.Background()); err != nil {
		t.Fatalf("mint error = %v", err)
	}
	if want := "/tenant-xyz/oauth2/v2.0/token"; gotPath != want {
		t.Errorf("token endpoint path = %q, want %q", gotPath, want)
	}
}

func TestClientCredentialTokenSource_STSError_ReturnsTypedAPIError(t *testing.T) {
	t.Parallel()

	sts := &tokenEndpoint{status: http.StatusBadRequest, body: `{"error":"invalid_client","error_description":"AADSTS7000215"}`}
	srv := sts.start(t)

	src, err := ClientCredentialTokenSource(ClientCredentialConfig{
		TenantID:         "tenant-01",
		ClientID:         "blueprint-appid-01",
		Credential:       SecretCredential{Secret: staticSecret("wrong-secret")},
		AuthorityBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("ClientCredentialTokenSource() error = %v", err)
	}

	_, err = src(context.Background())
	if err == nil {
		t.Fatal("mint error = nil, want a token endpoint failure")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("mint error = %v, want errors.As to *APIError", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("APIError.Status = %d, want 400", apiErr.Status)
	}
	if strings.Contains(err.Error(), "wrong-secret") {
		t.Error("error text leaked the client secret")
	}
}

func TestClientCredentialTokenSource_Validation(t *testing.T) {
	t.Parallel()

	cred := SecretCredential{Secret: staticSecret("x")}
	cases := []struct {
		name string
		cfg  ClientCredentialConfig
	}{
		{"no tenant", ClientCredentialConfig{ClientID: "app", Credential: cred}},
		{"no client id", ClientCredentialConfig{TenantID: "t", Credential: cred}},
		{"no credential", ClientCredentialConfig{TenantID: "t", ClientID: "app"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ClientCredentialTokenSource(tc.cfg); err == nil {
				t.Errorf("ClientCredentialTokenSource(%+v) error = nil, want a validation error", tc.cfg)
			}
		})
	}
}

func TestClientCredentialTokenSource_DrivesGraphWriteWithMintedToken(t *testing.T) {
	t.Parallel()

	// Prove the whole production seam offline: the client-cred source mints a
	// token, and a Client built on it carries that exact bearer on the Graph
	// create — the blueprint acting as its own client, no delegated token.
	sts := &tokenEndpoint{body: `{"access_token":"minted-blueprint-token"}`}
	stsSrv := sts.start(t)
	src, err := ClientCredentialTokenSource(ClientCredentialConfig{
		TenantID:         "tenant-01",
		ClientID:         "blueprint-appid-01",
		Credential:       SecretCredential{Secret: staticSecret("s3cr3t")},
		AuthorityBaseURL: stsSrv.URL,
	})
	if err != nil {
		t.Fatalf("ClientCredentialTokenSource() error = %v", err)
	}

	g := &writeGraph{createStatus: http.StatusCreated, createBody: `{"id":"identity-new-01","displayName":"mandat-dev"}`}
	graphSrv := g.start(t)
	c, err := New(Config{GraphBaseURL: graphSrv.URL, TokenSource: src})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := c.CreateAgentIdentity(context.Background(), "bp-01", "mandat-dev", []string{"sponsor-01"}); err != nil {
		t.Fatalf("CreateAgentIdentity() error = %v", err)
	}

	var post *capturedReq
	for _, r := range g.recorded() {
		if r.method == http.MethodPost {
			r := r
			post = &r
		}
	}
	if post == nil {
		t.Fatal("no POST reached the Graph server")
	}
	if post.authz != "Bearer minted-blueprint-token" {
		t.Errorf("write authz = %q, want the client-cred minted token", post.authz)
	}
}

func TestSecretFromEnv(t *testing.T) {
	const name = "MANDAT_TEST_BLUEPRINT_SECRET"
	t.Setenv(name, "env-secret-value")
	got, err := SecretFromEnv(name)(context.Background())
	if err != nil {
		t.Fatalf("SecretFromEnv error = %v", err)
	}
	if got != "env-secret-value" {
		t.Errorf("secret = %q, want env-secret-value", got)
	}

	if _, err := SecretFromEnv("MANDAT_TEST_UNSET_SECRET")(context.Background()); err == nil {
		t.Error("SecretFromEnv on an unset var error = nil, want an error")
	}
}

func TestSecretFromFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("  file-secret-value\n"), 0o600); err != nil {
		t.Fatalf("write temp secret: %v", err)
	}
	got, err := SecretFromFile(path)(context.Background())
	if err != nil {
		t.Fatalf("SecretFromFile error = %v", err)
	}
	if got != "file-secret-value" {
		t.Errorf("secret = %q, want file-secret-value (trimmed)", got)
	}

	if _, err := SecretFromFile(filepath.Join(t.TempDir(), "missing"))(context.Background()); err == nil {
		t.Error("SecretFromFile on a missing file error = nil, want an error")
	}
}
