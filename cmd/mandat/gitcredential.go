package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/baodq97/mandat/internal/config"
	"github.com/baodq97/mandat/internal/identity"
)

// gitCredentialCmd answers git's credential-helper protocol. git re-invokes the
// binary as `mandat git-credential <op>` (RFC-0001 §Identity injection), appending
// the operation (get/store/erase) as the final argument; only get produces output.
// The delegated agent-user token is minted per get and written to stdout as the
// git password, never persisted — the mechanism S-credential-delivery proved
// invariant-preserving on git 2.43.
func gitCredentialCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("git-credential", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	roleName := fs.String("role", "dev", "the RoleAgent whose delegated token backs a get")
	username := fs.String("username", "", "the username to emit (defaults to the request's)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	op := fs.Arg(0)
	if op == "" {
		fmt.Fprintln(stderr, "mandat git-credential: missing operation (get|store|erase)")
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "mandat git-credential: %v\n", err)
		return 1
	}

	broker, err := buildBroker(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "mandat git-credential: %v\n", err)
		return 1
	}

	return gitCredential(context.Background(), broker, op, *roleName, *username, stdin, stdout, stderr)
}

// gitCredential is the protocol core, split from the config/flag plumbing so it is
// unit-tested against a fake identity.TokenSource with no live mint. It delegates the
// whole protocol (reading git's request, minting, writing the response) to
// identity.CredentialHelper, which keeps the token off disk by construction.
func gitCredential(ctx context.Context, src identity.TokenSource, op, role, username string, stdin io.Reader, stdout, stderr io.Writer) int {
	err := identity.CredentialHelper(ctx, src, identity.CredentialRequest{
		Op:       op,
		Role:     role,
		Username: username,
		In:       stdin,
		Out:      stdout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "mandat git-credential: %v\n", err)
		return 1
	}
	return 0
}

// buildBroker constructs the delegated-token broker from config. Shared by serve,
// doctor, and the credential helper so all three mint through one path (ADR-0005).
func buildBroker(cfg *config.Config) (*identity.Broker, error) {
	var cred identity.ClientCredential
	switch cfg.Auth.Mode {
	case config.AuthArcManagedIdentity:
		cred = identity.NewManagedIdentityCredential()
	default:
		// The client-certificate pilot path has no cert-credential constructor yet
		// (identity offers secret and managed-identity); until it lands, the dev pilot
		// mints leg 1 from a client secret in MANDAT_CLIENT_SECRET, or the file named
		// by MANDAT_CLIENT_SECRET_FILE when this process is itself the spawned child's
		// git-credential helper (see resolveClientSecret).
		secret, err := resolveClientSecret()
		if err != nil {
			return nil, err
		}
		cred = identity.NewSecretCredential(secret)
	}
	return identity.NewBroker(cfg, cred, identity.AzureDevOpsResource), nil
}

// resolveClientSecret returns the pilot client-secret from MANDAT_CLIENT_SECRET,
// falling back to the file named by MANDAT_CLIENT_SECRET_FILE (whitespace
// trimmed). The file fallback exists so the spawned agent's git-credential helper
// can mint from the child env, which by AC-15 carries the file PATH but never the
// secret value; production's managed-identity mode uses neither. A read failure or
// an unset pair is returned as an error naming the cause, so the mint fails fast
// with a clear reason rather than an opaque AADSTS error downstream.
func resolveClientSecret() (string, error) {
	if v := os.Getenv("MANDAT_CLIENT_SECRET"); v != "" {
		return v, nil
	}
	if f := os.Getenv("MANDAT_CLIENT_SECRET_FILE"); f != "" {
		b, err := os.ReadFile(f) //nolint:gosec // MANDAT_CLIENT_SECRET_FILE is operator-set config (pilot), not untrusted input; prod uses managed-identity and never reads a secret file
		if err != nil {
			return "", fmt.Errorf("read MANDAT_CLIENT_SECRET_FILE %q: %w", f, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("neither MANDAT_CLIENT_SECRET nor MANDAT_CLIENT_SECRET_FILE is set")
}
