package main

import (
	"context"
	"strings"
	"testing"
)

// fakeTokenSource is the identity.TokenSource double: a fixed token, so the
// credential-helper wiring is proven without a live mint.
type fakeTokenSource struct {
	token string
}

func (f fakeTokenSource) Token(_ context.Context, _ string) (string, error) {
	return f.token, nil
}

// TestGitCredential_GetEmitsPassword proves the get path mints for the requested role
// and writes the delegated token as the git password (S-credential-delivery Basic
// auth), with the emitted username the caller supplied.
func TestGitCredential_GetEmitsPassword(t *testing.T) {
	t.Parallel()
	var out, errBuf strings.Builder
	in := strings.NewReader("protocol=https\nhost=dev.azure.com\n\n")

	code := gitCredential(context.Background(), fakeTokenSource{token: "delegated-xyz"}, "get", "dev", "mandat-dev", in, &out, &errBuf)
	if code != 0 {
		t.Fatalf("gitCredential(get) = %d, stderr = %q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "password=delegated-xyz") {
		t.Errorf("out = %q, want a password line carrying the delegated token", out.String())
	}
	if !strings.Contains(out.String(), "username=mandat-dev") {
		t.Errorf("out = %q, want the emitted username line", out.String())
	}
}

// TestGitCredential_StoreIsNoOp proves store produces no output: the token is minted
// per get and never handed to git to cache, so there is nothing to persist.
func TestGitCredential_StoreIsNoOp(t *testing.T) {
	t.Parallel()
	var out, errBuf strings.Builder
	code := gitCredential(context.Background(), fakeTokenSource{token: "x"}, "store", "dev", "", strings.NewReader(""), &out, &errBuf)
	if code != 0 || out.Len() != 0 {
		t.Errorf("gitCredential(store) = %d, out = %q, want 0 and no output", code, out.String())
	}
}

// TestGitCredential_UnknownOpFails proves an unrecognized operation is a non-zero,
// diagnosed exit rather than a silent success.
func TestGitCredential_UnknownOpFails(t *testing.T) {
	t.Parallel()
	var out, errBuf strings.Builder
	code := gitCredential(context.Background(), fakeTokenSource{token: "x"}, "bogus", "dev", "", strings.NewReader(""), &out, &errBuf)
	if code != 1 {
		t.Errorf("gitCredential(bogus) = %d, want 1", code)
	}
}
