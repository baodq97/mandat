package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baodq97/mandat/internal/config"
)

const (
	testTenant     = "00000000-0000-0000-0000-000000000000"
	testBlueprint  = "blueprint-app-id"
	testAgentID    = "agent-identity-id"
	testAgentUser  = "agent-user-object-id"
	testResource   = "ado-resource-app-id"
	testSecret     = "dev-only-secret"
	testTokenRoute = "/" + testTenant + "/oauth2/v2.0/token"
)

func testConfig() *config.Config {
	return &config.Config{
		Entra: config.EntraConfig{
			Tenant:       testTenant,
			Blueprint:    testBlueprint,
			IdentityMode: config.IdentityAgentUserPair,
		},
		Roles: map[string]config.RoleConfig{
			"dev": {
				AgentIdentityID: testAgentID,
				AgentUserID:     testAgentUser,
				AutonomyCeiling: config.CeilingDraftPR,
				Playbook:        "dev.md",
			},
		},
	}
}

// makeJWT builds an unsigned JWT carrying exp so tokenExpiry can read it; the
// signature segment is a placeholder the broker never inspects.
func makeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// writeToken is invoked from the httptest handler goroutine, so it reports an
// encode failure with Errorf, never Fatalf (FailNow must run on the test
// goroutine).
func writeToken(t *testing.T, w http.ResponseWriter, access string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   3600,
	}); err != nil {
		t.Errorf("encode token response: %v", err)
	}
}

// legName classifies a token request by its distinguishing fields so the fake
// endpoint can return the right per-leg token: leg 1 alone carries fmi_path, leg
// 3 alone uses the user_fic grant, leg 2 is the remaining client_credentials
// call.
func legName(form url.Values) string {
	switch {
	case form.Get("fmi_path") != "":
		return "leg1"
	case form.Get("grant_type") == "user_fic":
		return "leg3"
	case form.Get("grant_type") == "client_credentials":
		return "leg2"
	default:
		return "unknown"
	}
}

// recorder captures the forms the broker posts. The httptest handler runs on its
// own goroutine, so the mutex keeps -race quiet even though the three legs are
// sequential.
type recorder struct {
	mu    sync.Mutex
	forms map[string]url.Values
	mints int
}

func (r *recorder) record(leg string, form url.Values) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forms[leg] = form
	if leg == "leg3" {
		r.mints++
	}
}

func (r *recorder) snapshot() (map[string]url.Values, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]url.Values, len(r.forms))
	for k, v := range r.forms {
		out[k] = v
	}
	return out, r.mints
}

// fakeEndpoint stands in for login.microsoftonline.com: it validates the route,
// records each leg's form, and returns a distinct canned JWT per leg so the test
// can assert that leg N+1 threads leg N's token forward.
func fakeEndpoint(t *testing.T, rec *recorder, t1, t2, t3 string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testTokenRoute {
			t.Errorf("token request path = %q, want %q", r.URL.Path, testTokenRoute)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		leg := legName(r.PostForm)
		rec.record(leg, r.PostForm)
		switch leg {
		case "leg1":
			writeToken(t, w, t1)
		case "leg2":
			writeToken(t, w, t2)
		case "leg3":
			writeToken(t, w, t3)
		default:
			t.Errorf("unclassifiable token request: %v", r.PostForm)
			http.Error(w, "unknown leg", http.StatusBadRequest)
		}
	}))
}

func TestBroker_ThreeLegChain(t *testing.T) {
	t.Parallel()

	// Distinct exp per leg (not just distinct calls): exp truncates to whole Unix
	// seconds, so three calls issued back-to-back at time.Hour would round to the
	// same second and produce byte-identical JWTs, which would make the
	// leg-chaining assertions below vacuous (they could not tell a correctly
	// forwarded token from a wrongly forwarded one). Offsetting by a full hour per
	// leg makes t1, t2, t3 distinct by construction, independent of wall-clock
	// timing.
	now := time.Now()
	t1 := makeJWT(t, now.Add(1*time.Hour))
	t2 := makeJWT(t, now.Add(2*time.Hour))
	t3 := makeJWT(t, now.Add(3*time.Hour))

	rec := &recorder{forms: map[string]url.Values{}}
	srv := fakeEndpoint(t, rec, t1, t2, t3)
	defer srv.Close()

	b := NewBroker(testConfig(), NewSecretCredential(testSecret), testResource)
	b.endpoint = srv.URL
	b.client = srv.Client()

	got, err := b.Token(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Token() error = %v, want nil", err)
	}
	if got != t3 {
		t.Fatalf("Token() = %q, want the leg-3 delegated token", got)
	}

	forms, _ := rec.snapshot()
	if len(forms) != 3 {
		t.Fatalf("posted %d distinct legs, want 3", len(forms))
	}

	// Each leg's form is asserted whole with reflect.DeepEqual, so a missing field
	// or a stray one (e.g. the client secret leaking into a later leg) fails the
	// test, not just the fields the assertion happens to name.

	// Leg 1: the blueprint acts for the agent identity, authenticated by the dev
	// client secret, with fmi_path naming the agent identity.
	wantLeg1 := newForm(
		"client_id", testBlueprint,
		"grant_type", "client_credentials",
		"scope", exchangeScope,
		"fmi_path", testAgentID,
		"client_secret", testSecret,
	)
	if !reflect.DeepEqual(forms["leg1"], wantLeg1) {
		t.Errorf("leg 1 form = %v, want %v", forms["leg1"], wantLeg1)
	}

	// Leg 2: the agent-identity FIC exchange, presenting leg 1's token as its
	// client assertion.
	wantLeg2 := newForm(
		"client_id", testAgentID,
		"grant_type", "client_credentials",
		"client_assertion_type", clientAssertionType,
		"client_assertion", t1,
		"scope", exchangeScope,
	)
	if !reflect.DeepEqual(forms["leg2"], wantLeg2) {
		t.Errorf("leg 2 form = %v, want %v", forms["leg2"], wantLeg2)
	}

	// Leg 3: the delegated agent-user token, carrying leg 1 as the client
	// assertion and leg 2 as the user federated credential, scoped to the ADO
	// resource. client_assertion_type is required here too (live Entra: AADSTS900144
	// if absent) — the same jwt-bearer constant leg 2 uses.
	wantLeg3 := newForm(
		"client_id", testAgentID,
		"grant_type", "user_fic",
		"client_assertion_type", clientAssertionType,
		"client_assertion", t1,
		"user_id", testAgentUser,
		"user_federated_identity_credential", t2,
		"scope", testResource+"/.default",
	)
	if !reflect.DeepEqual(forms["leg3"], wantLeg3) {
		t.Errorf("leg 3 form = %v, want %v", forms["leg3"], wantLeg3)
	}

	// The chaining invariant, called out explicitly: each leg threads the
	// previous leg's token forward.
	if forms["leg2"].Get("client_assertion") != t1 {
		t.Errorf("leg 2 client_assertion does not carry leg 1's token")
	}
	if forms["leg3"].Get("user_federated_identity_credential") != t2 {
		t.Errorf("leg 3 user_federated_identity_credential does not carry leg 2's token")
	}
}

// newForm builds a url.Values from alternating key, value pairs so the expected
// leg bodies compare equal to the posted PostForm without hand-aligned literals.
func newForm(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestBroker_TokenCaching(t *testing.T) {
	t.Parallel()

	clock := &stepClock{t: time.Unix(1_700_000_000, 0).UTC()}
	// Canned leg-3 token expires one hour after the clock's start; with the
	// 5-minute refresh skew it is served from cache until start+55m.
	t3 := makeJWT(t, clock.now().Add(time.Hour))

	rec := &recorder{forms: map[string]url.Values{}}
	srv := fakeEndpoint(t, rec, makeJWT(t, clock.now().Add(time.Hour)), makeJWT(t, clock.now().Add(time.Hour)), t3)
	defer srv.Close()

	b := NewBroker(testConfig(), NewSecretCredential(testSecret), testResource)
	b.endpoint = srv.URL
	b.client = srv.Client()
	b.now = clock.now

	first, err := b.Token(context.Background(), "dev")
	if err != nil {
		t.Fatalf("first Token() error = %v", err)
	}
	if _, mints := rec.snapshot(); mints != 1 {
		t.Fatalf("after first Token, mints = %d, want 1", mints)
	}

	// Well before expiry: served from cache, no new mint.
	clock.advance(30 * time.Minute)
	second, err := b.Token(context.Background(), "dev")
	if err != nil {
		t.Fatalf("second Token() error = %v", err)
	}
	if second != first {
		t.Errorf("cached Token() = %q, want the first token %q", second, first)
	}
	if _, mints := rec.snapshot(); mints != 1 {
		t.Fatalf("after cached Token, mints = %d, want 1 (no re-mint)", mints)
	}

	// Past the refresh skew (start+56m > start+55m): re-mint.
	clock.advance(26 * time.Minute)
	if _, err := b.Token(context.Background(), "dev"); err != nil {
		t.Fatalf("third Token() error = %v", err)
	}
	if _, mints := rec.snapshot(); mints != 2 {
		t.Fatalf("after expiry Token, mints = %d, want 2 (re-mint)", mints)
	}
}

func TestBroker_LegErrorNeverLeaksSecret(t *testing.T) {
	t.Parallel()

	// Leg 1 fails with an AAD-style error; the error surfaced must carry the
	// diagnostic code but never the client secret from the request form.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
	}))
	defer srv.Close()

	b := NewBroker(testConfig(), NewSecretCredential(testSecret), testResource)
	b.endpoint = srv.URL
	b.client = srv.Client()

	_, err := b.Token(context.Background(), "dev")
	if err == nil {
		t.Fatal("Token() error = nil, want a leg-1 failure")
	}
	if !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("error %q does not carry the AAD diagnostic code", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("error leaks the client secret: %q", err)
	}
}

func TestBroker_UnknownRole(t *testing.T) {
	t.Parallel()

	b := NewBroker(testConfig(), NewSecretCredential(testSecret), testResource)
	if _, err := b.Token(context.Background(), "reviewer"); err == nil {
		t.Fatal("Token() for an unconfigured role error = nil, want a role-not-found error")
	}
}

func TestTokenExpiry(t *testing.T) {
	t.Parallel()

	want := time.Unix(1_700_003_600, 0).UTC()
	got, err := tokenExpiry(makeJWT(t, want))
	if err != nil {
		t.Fatalf("tokenExpiry() error = %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("tokenExpiry() = %v, want %v", got, want)
	}

	if _, err := tokenExpiry("not-a-jwt"); err == nil {
		t.Error("tokenExpiry() on a non-JWT error = nil, want an error")
	}
}
