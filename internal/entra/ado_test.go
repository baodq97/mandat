package entra

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// adoGraph serves the three Graph surfaces step 6 touches — resolve the ADO
// service principal by appId, list a principal's oauth2 grants, and create a
// grant — from canned bodies, recording each request for assertion.
type adoGraph struct {
	mu       sync.Mutex
	requests []capturedReq

	spByAppIDBody string
	grantsBody    string
	createStatus  int
}

func (g *adoGraph) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.requests = append(g.requests, capturedReq{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, authz: r.Header.Get("Authorization"), body: string(raw)})
		g.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/oauth2PermissionGrants"):
			if g.createStatus != 0 {
				w.WriteHeader(g.createStatus)
			}
		case strings.HasSuffix(r.URL.Path, "/oauth2PermissionGrants"):
			_, _ = w.Write([]byte(g.grantsBody))
		case strings.Contains(r.URL.Path, "servicePrincipals(appId="):
			_, _ = w.Write([]byte(g.spByAppIDBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *adoGraph) recorded() []capturedReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]capturedReq, len(g.requests))
	copy(out, g.requests)
	return out
}

func TestResolveServicePrincipalID(t *testing.T) {
	t.Parallel()
	g := &adoGraph{spByAppIDBody: `{"id":"ado-sp-object-01","appId":"499b84ac-1321-427f-aa17-267ca6975798"}`}
	srv := g.start(t)
	c := newClient(t, srv, &fakeTokenSource{token: testToken})

	id, err := c.ResolveServicePrincipalID(context.Background(), ADOAppID)
	if err != nil {
		t.Fatalf("ResolveServicePrincipalID() error = %v", err)
	}
	if id != "ado-sp-object-01" {
		t.Errorf("id = %q, want ado-sp-object-01", id)
	}
	// The lookup uses the OData (appId='...') key segment verbatim.
	if got := g.recorded()[0].path; !strings.Contains(got, "servicePrincipals(appId='"+ADOAppID+"')") {
		t.Errorf("request path = %q, want the (appId='...') key segment", got)
	}
}

func TestOAuth2GrantCall_BodyShape(t *testing.T) {
	t.Parallel()
	c, err := New(Config{GraphBaseURL: "https://graph.example/v1.0", TokenSource: (&fakeTokenSource{token: testToken}).source})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	call, err := c.OAuth2GrantCall(OAuth2GrantSpec{
		ClientID: "identity-sp-01", PrincipalID: "user-oid-01", ResourceID: "ado-sp-01", Scope: ADOImpersonationScope,
	})
	if err != nil {
		t.Fatalf("OAuth2GrantCall() error = %v", err)
	}
	body := string(call.Body)
	for _, want := range []string{
		`"clientId":"identity-sp-01"`,
		`"consentType":"Principal"`,
		`"principalId":"user-oid-01"`,
		`"resourceId":"ado-sp-01"`,
		`"scope":"user_impersonation"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("grant body missing %q\n%s", want, body)
		}
	}
}

func TestHasOAuth2Grant(t *testing.T) {
	t.Parallel()
	g := &adoGraph{grantsBody: `{"value":[{"resourceId":"ado-sp-01"},{"resourceId":"other-sp"}]}`}
	srv := g.start(t)
	c := newClient(t, srv, &fakeTokenSource{token: testToken})

	has, err := c.HasOAuth2Grant(context.Background(), "user-oid-01", "ado-sp-01")
	if err != nil {
		t.Fatalf("HasOAuth2Grant() error = %v", err)
	}
	if !has {
		t.Error("has = false, want true (a grant to ado-sp-01 exists)")
	}

	missing, err := c.HasOAuth2Grant(context.Background(), "user-oid-01", "not-granted-sp")
	if err != nil {
		t.Fatalf("HasOAuth2Grant() error = %v", err)
	}
	if missing {
		t.Error("has = true for an ungranted resource, want false")
	}

	// The filter query keeps $ literal and encodes the value.
	if q := g.recorded()[0].rawQuery; !strings.HasPrefix(q, "$filter=principalId") {
		t.Errorf("filter query = %q, want a $filter on principalId", q)
	}
}

func TestCreateOAuth2Grant_Forbidden_ReturnsPrivilegeError(t *testing.T) {
	t.Parallel()
	g := &adoGraph{createStatus: http.StatusForbidden}
	srv := g.start(t)
	c := newClient(t, srv, &fakeTokenSource{token: testToken})

	err := c.CreateOAuth2Grant(context.Background(), OAuth2GrantSpec{
		ClientID: "identity-sp-01", PrincipalID: "user-oid-01", ResourceID: "ado-sp-01", Scope: ADOImpersonationScope,
	})
	if err == nil {
		t.Fatal("CreateOAuth2Grant() error = nil, want a 403")
	}
	var privErr *PrivilegeError
	if !errors.As(err, &privErr) {
		t.Fatalf("error = %v, want errors.As to *PrivilegeError", err)
	}
}
