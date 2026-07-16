package main

import (
	"context"
	"os"
	"path/filepath"
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

// TestResolveClientSecret proves the pilot fallback chain (env, then the
// MANDAT_CLIENT_SECRET_FILE path, trimmed) and that a read failure or an unset
// pair surfaces as an error naming the cause, rather than an empty secret that
// only fails later as an opaque Entra error. Subtests use t.Setenv, so no
// t.Parallel here (Go forbids Setenv under a parallel ancestor).
func TestResolveClientSecret(t *testing.T) {
	t.Run("env wins even when a file is also set", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("file-secret"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		t.Setenv("MANDAT_CLIENT_SECRET", "s1")
		t.Setenv("MANDAT_CLIENT_SECRET_FILE", path)
		got, err := resolveClientSecret()
		if err != nil {
			t.Fatalf("resolveClientSecret() error = %v, want nil", err)
		}
		if got != "s1" {
			t.Errorf("resolveClientSecret() = %q, want %q", got, "s1")
		}
	})

	t.Run("file fallback is trimmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("  s2\n"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		t.Setenv("MANDAT_CLIENT_SECRET", "")
		t.Setenv("MANDAT_CLIENT_SECRET_FILE", path)
		got, err := resolveClientSecret()
		if err != nil {
			t.Fatalf("resolveClientSecret() error = %v, want nil", err)
		}
		if got != "s2" {
			t.Errorf("resolveClientSecret() = %q, want %q", got, "s2")
		}
	})

	t.Run("empty-but-readable file errors naming the path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		t.Setenv("MANDAT_CLIENT_SECRET", "")
		t.Setenv("MANDAT_CLIENT_SECRET_FILE", path)
		got, err := resolveClientSecret()
		if err == nil {
			t.Fatalf("resolveClientSecret() error = nil, want an error naming %q (a blank file must not silently mint an empty secret)", path)
		}
		if got != "" {
			t.Errorf("resolveClientSecret() = %q, want empty on error", got)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("resolveClientSecret() error = %q, want it to name %q", err.Error(), path)
		}
	})

	t.Run("nonexistent file path errors naming the path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing")
		t.Setenv("MANDAT_CLIENT_SECRET", "")
		t.Setenv("MANDAT_CLIENT_SECRET_FILE", path)
		got, err := resolveClientSecret()
		if err == nil {
			t.Fatalf("resolveClientSecret() error = nil, want an error naming %q", path)
		}
		if got != "" {
			t.Errorf("resolveClientSecret() = %q, want empty on error", got)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("resolveClientSecret() error = %q, want it to name %q", err.Error(), path)
		}
	})

	t.Run("both unset errors naming both variables", func(t *testing.T) {
		t.Setenv("MANDAT_CLIENT_SECRET", "")
		t.Setenv("MANDAT_CLIENT_SECRET_FILE", "")
		got, err := resolveClientSecret()
		if err == nil {
			t.Fatalf("resolveClientSecret() error = nil, want an error naming both env vars")
		}
		if got != "" {
			t.Errorf("resolveClientSecret() = %q, want empty on error", got)
		}
		if !strings.Contains(err.Error(), "MANDAT_CLIENT_SECRET") || !strings.Contains(err.Error(), "MANDAT_CLIENT_SECRET_FILE") {
			t.Errorf("resolveClientSecret() error = %q, want it to name both MANDAT_CLIENT_SECRET and MANDAT_CLIENT_SECRET_FILE", err.Error())
		}
	})
}
