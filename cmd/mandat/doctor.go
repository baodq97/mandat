package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/baodq97/mandat/internal/config"
	"github.com/baodq97/mandat/internal/journal"
	"github.com/baodq97/mandat/internal/role"
)

// claudeVersionFloor is the CLI version mandat requires before first dispatch
// (ADR-0006: below it a truncated final result line corrupts the telemetry mandat
// records). doctor asserts it (RFC-0001 §Runner harness, AC-9.4).
const claudeVersionFloor = "2.1.208"

// checkResult is one doctor preflight outcome. required marks a check whose failure
// blocks (non-zero exit); a non-required check that fails warns without blocking.
// detail carries the recorded value (a version, a writable path) or the failure reason.
type checkResult struct {
	name     string
	required bool
	ok       bool
	detail   string
}

// doctor runs the preflight checks and prints a PASS/FAIL/WARN table, exiting
// non-zero if any required check fails before first dispatch (RFC-0001 §4.10). Each
// check maps to a spike: the CLI version floor to the runner-harness spike, the git
// version record to S-credential-delivery, SQLite to the journal plane, and tracker
// reachability to the S1/S3 identity chain.
func doctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	dbPath := fs.String("db", journal.DefaultPath, "path to the SQLite journal file")
	roleName := fs.String("role", "dev", "the RoleAgent to check tracker reachability for")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "mandat doctor: %v\n", err)
		return 1
	}

	return runChecks(ctx, buildChecks(cfg, *dbPath, *roleName), stdout)
}

// buildChecks assembles the ordered doctor preflight set. Both doctor and
// mandat init's closing preflight (init.go) build it here, so there is one
// validator set, not two: a green init runs the identical checks doctor does
// against the config it just wrote (US-0013 AC-13.7).
func buildChecks(cfg *config.Config, dbPath, roleName string) []func(context.Context) checkResult {
	return []func(context.Context) checkResult{
		claudeVersionCheck,
		gitVersionCheck,
		func(c context.Context) checkResult { return sqliteCheck(c, dbPath) },
		trackerCheckFor(cfg, roleName),
		func(context.Context) checkResult { return reviewerIdentityCheck(cfg) },
		func(context.Context) checkResult { return diskCheck(dbPath) },
	}
}

// runChecks executes each check, renders the aligned table, and returns the exit
// code: 1 when any required check failed, 0 otherwise. Separated from doctor so the
// table and exit logic are unit-tested against synthetic checks with no live
// environment.
func runChecks(ctx context.Context, checks []func(context.Context) checkResult, out io.Writer) int {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATUS\tDETAIL")
	exit := 0
	for _, fn := range checks {
		r := fn(ctx)
		status := "PASS"
		if !r.ok {
			if r.required {
				status = "FAIL"
				exit = 1
			} else {
				status = "WARN"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.name, status, r.detail)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(out, "mandat doctor: render table: %v\n", err)
	}
	return exit
}

// claudeVersionCheck asserts the Claude Code CLI is present and at least
// claudeVersionFloor (ADR-0006, AC-9.4). Below the floor is a required failure.
func claudeVersionCheck(ctx context.Context) checkResult {
	const name = "claude CLI"
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		return checkResult{name: name, required: true, detail: "claude not found on PATH: " + err.Error()}
	}
	got, err := parseVersion(string(out))
	if err != nil {
		return checkResult{name: name, required: true, detail: "could not parse `claude --version`: " + strings.TrimSpace(string(out))}
	}
	if !versionAtLeast(got, claudeVersionFloor) {
		return checkResult{name: name, required: true, detail: fmt.Sprintf("claude %s is below the required %s (ADR-0006)", got.raw, claudeVersionFloor)}
	}
	return checkResult{name: name, required: true, ok: true, detail: fmt.Sprintf("claude %s (>= %s)", got.raw, claudeVersionFloor)}
}

// gitVersionCheck records the installed git version. S-credential-delivery resolved
// to a Basic-auth password helper proven on git 2.43 (RFC-0001 §Open questions), so
// doctor records the version and does not gate on a floor the mechanism does not
// require (AC-9.5: the floor stays open until a mechanism names a higher minimum).
func gitVersionCheck(ctx context.Context) checkResult {
	const name = "git"
	out, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return checkResult{name: name, required: true, detail: "git not found on PATH: " + err.Error()}
	}
	got, err := parseVersion(string(out))
	if err != nil {
		return checkResult{name: name, required: true, ok: true, detail: "git present (" + strings.TrimSpace(string(out)) + ")"}
	}
	return checkResult{name: name, required: true, ok: true, detail: fmt.Sprintf("git %s (recorded; S-credential-delivery needs no higher floor)", got.raw)}
}

// sqliteCheck opens the journal at the configured path (running the idempotent
// migration) and closes it, confirming the pure-Go driver can persist there (D4).
func sqliteCheck(ctx context.Context, dbPath string) checkResult {
	const name = "sqlite journal"
	store, err := journal.Open(ctx, dbPath)
	if err != nil {
		return checkResult{name: name, required: true, detail: "cannot open " + dbPath + ": " + err.Error()}
	}
	_ = store.Close()
	return checkResult{name: name, required: true, ok: true, detail: "opened " + dbPath + " (WAL, migrations applied)"}
}

// trackerCheckFor returns the tracker-reachability check: a delegated-token
// acquisition through the identity broker (S1/S3 chain) followed by a WIQL poll
// against ADO. Building the broker and adapter can fail before the network probe,
// so those errors are captured into the returned check rather than crashing doctor.
func trackerCheckFor(cfg *config.Config, roleName string) func(context.Context) checkResult {
	const name = "tracker reachability"
	r, rerr := role.Resolve(cfg, roleName)
	if rerr != nil {
		return func(context.Context) checkResult {
			return checkResult{name: name, required: true, detail: "resolve role: " + rerr.Error()}
		}
	}
	broker, berr := buildBroker(cfg)
	if berr != nil {
		return func(context.Context) checkResult {
			return checkResult{name: name, required: true, detail: "build broker: " + berr.Error()}
		}
	}
	adapter, aerr := newRoleAdapter(cfg, broker, roleName, r.Mandate.AgentUserName)
	if aerr != nil {
		return func(context.Context) checkResult {
			return checkResult{name: name, required: true, detail: "build adapter: " + aerr.Error()}
		}
	}
	return func(ctx context.Context) checkResult {
		if _, err := broker.Token(ctx, roleName); err != nil {
			return checkResult{name: name, required: true, detail: "token acquisition failed: " + err.Error()}
		}
		if _, err := adapter.Poll(ctx); err != nil {
			return checkResult{name: name, required: true, detail: "WIQL poll failed: " + err.Error()}
		}
		return checkResult{name: name, required: true, ok: true, detail: "delegated token acquired and WIQL poll reachable"}
	}
}

// reviewerIdentityCheck confirms a Reviewer role is configured with a UPN
// distinct from every other role's (writer != scorer, RFC-0001 AC-27), so a
// missing or colliding reviewer identity surfaces here rather than only at
// verify time, task by task. A missing reviewer role only warns: it is a
// valid (if degraded) config, not a blocking misconfiguration.
func reviewerIdentityCheck(cfg *config.Config) checkResult {
	const name = "reviewer identity"
	rc, ok := cfg.Roles["reviewer"]
	if !ok || rc.AgentUserName == "" {
		return checkResult{name: name, required: false, detail: "no reviewer role configured: verification will hold every task at the probe (RFC-0001 AC-27)"}
	}
	for other, orc := range cfg.Roles {
		if other == "reviewer" || orc.AgentUserName == "" {
			continue
		}
		if orc.AgentUserName == rc.AgentUserName {
			return checkResult{name: name, required: true, detail: fmt.Sprintf("reviewer and %s share agent_user_name %q (writer must differ from scorer, RFC-0001 AC-27)", other, rc.AgentUserName)}
		}
	}
	return checkResult{name: name, required: true, ok: true, detail: "reviewer " + rc.AgentUserName + " is distinct from every other role"}
}

// diskCheck confirms the directory holding the DB and worktrees is present and
// writable, catching a missing mount, a read-only filesystem, or a full disk (a probe
// write returns ENOSPC) before first dispatch. A precise free-bytes headroom
// threshold via statfs is deferred: it needs a signed-to-unsigned block-size
// conversion the static-build/lint gate treats as suspect, and presence+writability
// catches the operational failures the skeleton needs to guard.
func diskCheck(dbPath string) checkResult {
	const name = "disk"
	dir := filepath.Dir(dbPath)
	info, err := os.Stat(dir)
	if err != nil {
		return checkResult{name: name, required: true, detail: dir + " is not present: " + err.Error()}
	}
	if !info.IsDir() {
		return checkResult{name: name, required: true, detail: dir + " is not a directory"}
	}
	f, err := os.CreateTemp(dir, ".mandat-doctor-probe-*")
	if err != nil {
		return checkResult{name: name, required: true, detail: dir + " is not writable: " + err.Error()}
	}
	probe := f.Name()
	_ = f.Close()
	_ = os.Remove(probe)
	return checkResult{name: name, required: true, ok: true, detail: dir + " is present and writable"}
}

// semver is a dotted numeric version, kept as its parsed components plus the raw
// text matched, so comparisons are numeric (2.1.208 < 2.1.210) and the table can
// echo exactly what the tool reported.
type semver struct {
	raw   string
	parts []int
}

var versionPattern = regexp.MustCompile(`\d+(?:\.\d+)+`)

// parseVersion extracts the first dotted numeric version from a tool's --version
// line (e.g. "2.1.210 (Claude Code)", "git version 2.43.0").
func parseVersion(s string) (semver, error) {
	m := versionPattern.FindString(s)
	if m == "" {
		return semver{}, fmt.Errorf("no dotted version in %q", s)
	}
	fields := strings.Split(m, ".")
	parts := make([]int, len(fields))
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return semver{}, fmt.Errorf("version %q has a non-numeric component %q", m, f)
		}
		parts[i] = n
	}
	return semver{raw: m, parts: parts}, nil
}

// versionAtLeast reports whether got is greater than or equal to the floor version
// string, comparing component by component with missing trailing components as zero.
func versionAtLeast(got semver, floor string) bool {
	f, err := parseVersion(floor)
	if err != nil {
		return false
	}
	return compareVersion(got.parts, f.parts) >= 0
}

func compareVersion(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
