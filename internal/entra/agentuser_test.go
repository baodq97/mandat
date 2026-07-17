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
	"time"
)

// userGraph serves the agent-user list (GET) and create (POST). The create
// returns 400 for its first lagPosts calls — the create→use propagation lag,
// where the just-created identity is not yet visible — then 201, so the
// retry-backoff is exercised without any wall-clock wait.
type userGraph struct {
	mu       sync.Mutex
	requests []capturedReq
	posts    int

	lagPosts int // number of leading POSTs that 400 before the 201
	forbid   bool
}

func (g *userGraph) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.requests = append(g.requests, capturedReq{method: r.Method, path: r.URL.Path, authz: r.Header.Get("Authorization"), body: string(raw)})
		isPost := r.Method == http.MethodPost
		if isPost {
			g.posts++
		}
		n := g.posts
		g.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case isPost && strings.HasSuffix(r.URL.Path, "/users/microsoft.graph.agentUser"):
			switch {
			case g.forbid:
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"code":"Authorization_RequestDenied"}}`))
			case n <= g.lagPosts:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":"Request_BadRequest","message":"IdentityParent does not exist or one of its queried reference-property objects are not present."}}`))
			default:
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"user-new-01","displayName":"mandat-dev-user","userPrincipalName":"mandat-dev@baotest.onmicrosoft.com","identityParentId":"identity-dev-01"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *userGraph) recorded() []capturedReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]capturedReq, len(g.requests))
	copy(out, g.requests)
	return out
}

// countingSleep is a no-op Sleep seam that records how many backoffs ran, so a
// test asserts the retry happened without waiting real seconds.
func countingSleep(calls *int) func(context.Context, time.Duration) error {
	return func(context.Context, time.Duration) error { *calls++; return nil }
}

func devUserSpec() AgentUserSpec {
	return AgentUserSpec{
		IdentityID:        "identity-dev-01",
		DisplayName:       "mandat-dev-user",
		MailNickname:      "mandat-dev",
		UserPrincipalName: "mandat-dev@baotest.onmicrosoft.com",
	}
}

func TestCreateAgentUser_RetriesOverPropagationLag(t *testing.T) {
	t.Parallel()

	g := &userGraph{lagPosts: 2} // 400, 400, then 201
	srv := g.start(t)
	c := newClient(t, srv, &fakeTokenSource{token: testToken})

	sleeps := 0
	user, err := c.CreateAgentUser(context.Background(), devUserSpec(), RetryPolicy{Sleep: countingSleep(&sleeps)})
	if err != nil {
		t.Fatalf("CreateAgentUser() error = %v, want nil after the lag clears", err)
	}
	if user.ID != "user-new-01" {
		t.Errorf("user.ID = %q, want user-new-01", user.ID)
	}

	posts := 0
	for _, r := range g.recorded() {
		if r.method == http.MethodPost {
			posts++
		}
	}
	if posts != 3 {
		t.Errorf("POST attempts = %d, want 3 (two lag 400s then the 201)", posts)
	}
	if sleeps != 2 {
		t.Errorf("backoff sleeps = %d, want 2 (before attempts 2 and 3)", sleeps)
	}
}

func TestCreateAgentUser_Forbidden_FailsFastNoRetry(t *testing.T) {
	t.Parallel()

	g := &userGraph{forbid: true}
	srv := g.start(t)
	c := newClient(t, srv, &fakeTokenSource{token: testToken})

	sleeps := 0
	_, err := c.CreateAgentUser(context.Background(), devUserSpec(), RetryPolicy{Sleep: countingSleep(&sleeps)})
	if err == nil {
		t.Fatal("CreateAgentUser() error = nil, want a 403 privilege error")
	}
	var privErr *PrivilegeError
	if !errors.As(err, &privErr) {
		t.Fatalf("CreateAgentUser() error = %v, want errors.As to *PrivilegeError", err)
	}
	if sleeps != 0 {
		t.Errorf("backoff sleeps = %d, want 0 (a 403 is a real permission gap, never retried)", sleeps)
	}

	posts := 0
	for _, r := range g.recorded() {
		if r.method == http.MethodPost {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("POST attempts = %d, want 1 (fail fast, no retry on 403)", posts)
	}
}

func TestCreateAgentUser_ExhaustsAttempts_SurfacesLastError(t *testing.T) {
	t.Parallel()

	g := &userGraph{lagPosts: 100} // never clears within the budget
	srv := g.start(t)
	c := newClient(t, srv, &fakeTokenSource{token: testToken})

	sleeps := 0
	_, err := c.CreateAgentUser(context.Background(), devUserSpec(), RetryPolicy{Attempts: 3, Sleep: countingSleep(&sleeps)})
	if err == nil {
		t.Fatal("CreateAgentUser() error = nil, want failure after the attempt budget")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error = %v, want it to name the exhausted attempt budget", err)
	}
	posts := 0
	for _, r := range g.recorded() {
		if r.method == http.MethodPost {
			posts++
		}
	}
	if posts != 3 {
		t.Errorf("POST attempts = %d, want 3 (the full budget)", posts)
	}
}
