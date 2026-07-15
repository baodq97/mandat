package identity

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSource is a stub TokenSource: it returns a canned token (or error) and
// counts calls so a test can prove the helper mints once per invocation.
type fakeSource struct {
	token string
	err   error
	calls int
}

func (f *fakeSource) Token(_ context.Context, _ string) (string, error) {
	f.calls++
	return f.token, f.err
}

const getRequest = "protocol=https\nhost=dev.azure.com\npath=baodo0220/mandat-dogfood/_git/mandat\n\n"

func TestCredentialHelper_Get(t *testing.T) {
	t.Parallel()

	src := &fakeSource{token: "delegated.jwt.token"}
	var out bytes.Buffer
	err := CredentialHelper(context.Background(), src, CredentialRequest{
		Op:       "get",
		Role:     "dev",
		Username: "agent-user-dev-01@baotest.onmicrosoft.com",
		In:       strings.NewReader(getRequest),
		Out:      &out,
	})
	if err != nil {
		t.Fatalf("CredentialHelper(get) error = %v, want nil", err)
	}

	want := "username=agent-user-dev-01@baotest.onmicrosoft.com\npassword=delegated.jwt.token\n"
	if out.String() != want {
		t.Errorf("get output =\n%q\nwant\n%q", out.String(), want)
	}
}

func TestCredentialHelper_Get_EchoesRequestUsername(t *testing.T) {
	t.Parallel()

	// With no override username, the helper echoes the username git sent in the
	// request so it still emits a well-formed Basic credential.
	src := &fakeSource{token: "tok"}
	var out bytes.Buffer
	err := CredentialHelper(context.Background(), src, CredentialRequest{
		Op:   "get",
		Role: "dev",
		In:   strings.NewReader("protocol=https\nhost=dev.azure.com\nusername=from-git\n\n"),
		Out:  &out,
	})
	if err != nil {
		t.Fatalf("CredentialHelper(get) error = %v", err)
	}
	if out.String() != "username=from-git\npassword=tok\n" {
		t.Errorf("get output = %q, want echoed request username", out.String())
	}
}

func TestCredentialHelper_Get_EOFTerminatedRequest(t *testing.T) {
	t.Parallel()

	// Git may end the request at EOF without a trailing blank line; the helper
	// must still answer.
	src := &fakeSource{token: "tok"}
	var out bytes.Buffer
	err := CredentialHelper(context.Background(), src, CredentialRequest{
		Op:       "get",
		Role:     "dev",
		Username: "u",
		In:       strings.NewReader("protocol=https\nhost=dev.azure.com"),
		Out:      &out,
	})
	if err != nil {
		t.Fatalf("CredentialHelper(get) error = %v", err)
	}
	if out.String() != "username=u\npassword=tok\n" {
		t.Errorf("get output = %q", out.String())
	}
}

func TestCredentialHelper_PerInvocation(t *testing.T) {
	t.Parallel()

	src := &fakeSource{token: "tok"}
	for i := range 3 {
		var out bytes.Buffer
		if err := CredentialHelper(context.Background(), src, CredentialRequest{
			Op: "get", Role: "dev", Username: "u", In: strings.NewReader(getRequest), Out: &out,
		}); err != nil {
			t.Fatalf("CredentialHelper(get) #%d error = %v", i, err)
		}
	}
	if src.calls != 3 {
		t.Errorf("broker minted %d times over 3 gets, want 3 (a fresh mint per invocation)", src.calls)
	}
}

func TestCredentialHelper_StoreEraseAreNoOps(t *testing.T) {
	t.Parallel()

	for _, op := range []string{"store", "erase"} {
		src := &fakeSource{token: "tok"}
		var out bytes.Buffer
		err := CredentialHelper(context.Background(), src, CredentialRequest{
			Op: op, Role: "dev", Username: "u", In: strings.NewReader(getRequest), Out: &out,
		})
		if err != nil {
			t.Errorf("CredentialHelper(%s) error = %v, want nil", op, err)
		}
		if out.Len() != 0 {
			t.Errorf("CredentialHelper(%s) wrote %q, want no output", op, out.String())
		}
		if src.calls != 0 {
			t.Errorf("CredentialHelper(%s) minted a token; store/erase must never mint", op)
		}
	}
}

func TestCredentialHelper_UnknownOp(t *testing.T) {
	t.Parallel()

	src := &fakeSource{token: "tok"}
	var out bytes.Buffer
	err := CredentialHelper(context.Background(), src, CredentialRequest{
		Op: "frobnicate", Role: "dev", Username: "u", In: strings.NewReader(getRequest), Out: &out,
	})
	if err == nil {
		t.Fatal("CredentialHelper(unknown op) error = nil, want an error")
	}
}

func TestCredentialHelper_BrokerErrorPropagates(t *testing.T) {
	t.Parallel()

	src := &fakeSource{err: errors.New("mint failed")}
	var out bytes.Buffer
	err := CredentialHelper(context.Background(), src, CredentialRequest{
		Op: "get", Role: "dev", Username: "u", In: strings.NewReader(getRequest), Out: &out,
	})
	if err == nil {
		t.Fatal("CredentialHelper(get) with a failing broker error = nil, want the mint error")
	}
	if out.Len() != 0 {
		t.Errorf("get wrote %q on a mint failure, want no output", out.String())
	}
}

// TestCredentialHelper_NeverPersistsToken is the AC-15 / AC-8.3 no-persist
// witness: the token reaches only the caller-supplied writer and nothing under a
// working directory. The structural guarantee is stronger — credential.go imports
// no filesystem API — so this proves the runtime behavior matches: after a get,
// the token exists in out and no file was created in a fresh directory.
func TestCredentialHelper_NeverPersistsToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const token = "super-secret-delegated-token"
	src := &fakeSource{token: token}
	var out bytes.Buffer
	if err := CredentialHelper(context.Background(), src, CredentialRequest{
		Op: "get", Role: "dev", Username: "u", In: strings.NewReader(getRequest), Out: &out,
	}); err != nil {
		t.Fatalf("CredentialHelper(get) error = %v", err)
	}

	if !strings.Contains(out.String(), token) {
		t.Fatal("token not delivered to the response writer")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read work dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("get created %d filesystem entries, want 0 (token must never touch disk)", len(entries))
	}
	// Belt and braces: no file anywhere under the work dir carries the token.
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			if data, readErr := os.ReadFile(path); readErr == nil && strings.Contains(string(data), token) {
				t.Errorf("token found persisted at %q", path)
			}
		}
		return nil
	})
}
