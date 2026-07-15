package identity

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// TokenSource mints a delegated token for a role. *Broker satisfies it; the
// credential helper depends on this narrow interface so its protocol handling is
// unit-tested against a fake source with no live mint.
type TokenSource interface {
	Token(ctx context.Context, role string) (string, error)
}

// CredentialRequest is one git credential-helper invocation: the operation git
// passed as argv (get, store, or erase), the mandat role whose delegated token
// backs a get, the username to emit, and the request/response streams (git's
// stdin and stdout). Bundled like workspace.SpawnSpec so the cmd wiring passes
// one value.
type CredentialRequest struct {
	Op       string
	Role     string
	Username string
	In       io.Reader
	Out      io.Writer
}

// CredentialHelper answers git's credential-helper protocol for one operation
// (git re-invokes the mandat binary as `mandat git-credential <op>`, RFC-0001
// §Identity injection: the binary re-invoked as a git credential helper). Only
// get produces output: it reads the request attributes from req.In, mints a
// fresh delegated token for req.Role via src, and writes username and password
// (the token) to req.Out so git sends Authorization: Basic with the token as the
// password — the mechanism S-credential-delivery proved invariant-preserving on
// git 2.43. store and erase are deliberate no-ops: the token is minted per get
// and never handed to git to cache, so there is nothing to persist or forget.
//
// The token reaches git only through req.Out and never touches a file: this
// function opens none (the package imports no filesystem API), which is the AC-15
// / AC-8.3 no-persist invariant made structural.
func CredentialHelper(ctx context.Context, src TokenSource, req CredentialRequest) error {
	switch req.Op {
	case "get":
		return credentialGet(ctx, src, req)
	case "store", "erase":
		return nil
	default:
		return fmt.Errorf("identity: unknown git credential operation %q", req.Op)
	}
}

func credentialGet(ctx context.Context, src TokenSource, req CredentialRequest) error {
	attrs, err := readRequest(req.In)
	if err != nil {
		return err
	}

	token, err := src.Token(ctx, req.Role)
	if err != nil {
		return err
	}

	username := req.Username
	if username == "" {
		username = attrs["username"]
	}

	// Basic-password delivery (S-credential-delivery): git emits Authorization:
	// Basic base64(username:password) and ADO accepts the delegated agent-user
	// token as the password. The username is not load-bearing for ADO token auth
	// but is emitted (the agent-user UPN in production) for a truthful credential
	// record. The token has no newline or NUL, so it is a valid attribute value.
	var buf bytes.Buffer
	if username != "" {
		fmt.Fprintf(&buf, "username=%s\n", username)
	}
	fmt.Fprintf(&buf, "password=%s\n", token)
	if _, err := req.Out.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("identity: write credential response: %w", err)
	}
	return nil
}

// readRequest parses the git credential-helper request: key=value lines
// terminated by a blank line or EOF (git-credential(1) input format). The parsed
// attributes are advisory here — the broker mints the same delegated token for
// the one ADO resource it is configured for regardless of the requested host —
// but draining stdin follows the protocol and lets a get echo the requested
// username when the caller does not override it.
func readRequest(in io.Reader) (map[string]string, error) {
	attrs := make(map[string]string)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), maxResponseBytes)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("identity: malformed credential request line %q", line)
		}
		attrs[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("identity: read credential request: %w", err)
	}
	return attrs, nil
}
