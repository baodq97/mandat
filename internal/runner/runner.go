// Package runner is the subprocess-supervisor plane (ADR-0006, RFC-0001 §Runner
// harness): it spawns Claude Code headless once per task under the per-role OS
// user, records the run in the Journal, and turns the run into one
// orchestrator.Event.
//
// Two invariants from ADR-0006 shape this package and are worth stating because
// the code cannot (ADR-0003):
//
//   - stream-json is telemetry; the ResultContract file is the contract. The
//     child's stdout carries session_id and cost/usage/turns; none of it is the
//     task outcome. The outcome comes only from the schema-validated
//     .mandat/result.json the subprocess writes (result.Path). A clean exit with
//     a stdout "success" line still yields result_invalid when the file is
//     missing or schema-invalid, so a compromised or buggy agent cannot talk its
//     way past the file gate.
//   - the session id is pinned and journaled BEFORE the spawn. The supervisor
//     generates the --session-id, records it in the append-only journal, and only
//     then starts the child, so a crashed run is still addressable by its session
//     id even when the runs row never lands.
//
// The supervisor derives an orchestrator.Event and returns it; it never calls
// orchestrator.Next itself. Feeding the state machine and journaling the
// transition belong to the caller, which composes this run with the verification
// plane (gates, diff-in-remit, PR probe) before the completed path reaches
// in-review (RFC-0001 AC-20).
package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/baodq97/mandat/internal/journal"
	"github.com/baodq97/mandat/internal/orchestrator"
	"github.com/baodq97/mandat/internal/result"
	"github.com/baodq97/mandat/internal/role"
	"github.com/baodq97/mandat/internal/task"
	"github.com/baodq97/mandat/internal/workspace"
)

// Config is the supervisor's installation-scoped invocation config: the Claude
// Code binary to spawn (a fake binary under test, §9) and the per-run budget
// ceiling passed as --max-budget-usd (ADR-0006; RFC-0001's 5.00 default is an MVP
// placeholder).
type Config struct {
	ClaudePath   string
	MaxBudgetUSD float64
}

// Supervisor runs one task per call through the injected Spawner. It owns no
// mutable state between runs; the Journal is the durable record.
type Supervisor struct {
	store   *journal.Store
	spawner workspace.Spawner
	cfg     Config
}

// New builds a Supervisor. Inject workspace.DefaultSpawner in production (the
// per-role OS-user drop) and a direct-exec spawner in tests; the supervisor never
// names systemd itself, so the isolation mechanism stays behind the Spawner seam.
func New(store *journal.Store, spawner workspace.Spawner, cfg Config) *Supervisor {
	return &Supervisor{store: store, spawner: spawner, cfg: cfg}
}

// Request is one task run: the task and its resolved role, the provisioned
// worktree, and the per-role OS isolation the child drops into.
type Request struct {
	Task     *task.TaskContract
	Role     role.Role
	Worktree *workspace.Workspace

	// RoleUser, Home, and ConfigDir are the per-role OS isolation the caller
	// resolves at provision time (US-0005); config carries no OS-user field, so
	// the runner is told them rather than deriving a naming convention. Each child
	// gets its own HOME and CLAUDE_CONFIG_DIR so per-role config and session stores
	// do not collide with the parent's or each other's (ADR-0006 §Isolation).
	RoleUser  string
	Home      string
	ConfigDir string

	// GitCredentialHelper is the credential.helper value written into the
	// worktree's git config so a push authenticates as the agent user through the
	// `mandat git-credential` helper, which mints the delegated token on demand and
	// never lets it land in the worktree (S-credential-delivery). Empty skips the
	// wiring for a run that will not push. The runner writes the config; the helper
	// subcommand itself is cmd's job.
	GitCredentialHelper string

	// DenyToolHookCommand is the PreToolUse deny-hook command injected via the
	// --settings JSON (ADR-0006), the mechanical deny layer that mirrors this
	// repo's own govkit audit-write hook. Caller-provided like the credential
	// helper; the runner owns only the settings JSON shape, not the command.
	DenyToolHookCommand string

	// HarnessVersion and ConfigVersion stamp the runs and journal rows (RFC-0001
	// §Journal); optional, empty stores NULL.
	HarnessVersion string
	ConfigVersion  string
}

func (r Request) validate() error {
	switch {
	case r.Task == nil:
		return errors.New("runner: request has no task")
	case r.Worktree == nil || r.Worktree.Dir == "":
		return errors.New("runner: request has no provisioned worktree")
	default:
		return nil
	}
}

// Telemetry is the stream-json record the supervisor reads from stdout. It is
// never the outcome (ADR-0006): the caller journals it beside the run for cost
// and audit, and can compare ObservedSessionID against the pinned session id.
// Subtype and DurationMS have no runs-table column (RFC-0001 §runs), so they ride
// the Outcome to the caller's transition-journal detail rather than the runs row.
type Telemetry struct {
	ObservedSessionID string
	SessionMatch      bool
	Subtype           string
	IsError           bool
	NumTurns          int
	DurationMS        int64
	TotalCostUSD      float64
	Usage             []byte
}

// Outcome is one run's result: the derived event plus everything the caller needs
// to feed orchestrator.Next and journal the transition. Result is the parsed
// ResultContract on the valid path and nil otherwise; RawResult is always the
// exact bytes read (empty when the file was missing), the same bytes stored in
// results.raw.
type Outcome struct {
	Event     orchestrator.Event
	RunID     string
	SessionID string
	Valid     bool
	Result    *result.ResultContract
	RawResult []byte
	Telemetry Telemetry
	ExitCode  int
}

// Run spawns the agent for a fresh session. It pins a new --session-id, journals
// it before the spawn, runs the child, and derives the event from the
// ResultContract file.
func (s *Supervisor) Run(ctx context.Context, req Request) (Outcome, error) {
	sessionID, err := newUUIDv4()
	if err != nil {
		return Outcome{}, fmt.Errorf("runner: generate session id: %w", err)
	}
	return s.run(ctx, req, sessionID, false)
}

// Resume re-enters an existing session in the same worktree with
// `claude -p --resume <session-id>` (ADR-0006 §Session continuity). The trigger
// (clearing a needs-human hold) is deferred out of the skeleton, but the code
// path exists now because the session id and worktree binding are already pinned
// in runs.session_id (RFC-0001 §Session and resume).
func (s *Supervisor) Resume(ctx context.Context, req Request, sessionID string) (Outcome, error) {
	if sessionID == "" {
		return Outcome{}, errors.New("runner: resume needs the existing session id")
	}
	return s.run(ctx, req, sessionID, true)
}

func (s *Supervisor) run(ctx context.Context, req Request, sessionID string, resume bool) (Outcome, error) {
	if err := req.validate(); err != nil {
		return Outcome{}, err
	}

	runID, err := newUUIDv4()
	if err != nil {
		return Outcome{}, fmt.Errorf("runner: generate run id: %w", err)
	}
	resultPath := filepath.Join(req.Worktree.Dir, result.Path)

	// req.Home and req.ConfigDir are per-task dirs (US-0012 AC-12.7) with no prior
	// provisioning step, unlike the shared per-role dirs, so the runner creates them
	// fresh here. An empty CLAUDE_CONFIG_DIR needs no seeding: auth reaches the
	// child through the allow-listed env vars in buildEnv below, not through
	// anything already on disk.
	if err := os.MkdirAll(req.Home, 0o700); err != nil {
		return Outcome{}, fmt.Errorf("runner: create home dir %s: %w", req.Home, err)
	}
	if err := os.MkdirAll(req.ConfigDir, 0o700); err != nil {
		return Outcome{}, fmt.Errorf("runner: create claude config dir %s: %w", req.ConfigDir, err)
	}

	if req.GitCredentialHelper != "" {
		if err := configureGitCredential(ctx, req.Worktree.Dir, req.GitCredentialHelper); err != nil {
			return Outcome{}, err
		}
	}

	if req.Role.Mandate.AgentUserName != "" {
		if err := configureGitAuthor(ctx, req.Worktree.Dir, req.Role.Mandate.AgentUserName); err != nil {
			return Outcome{}, err
		}
	}

	// Session id before spawn (ADR-0006). This append lands in the journal ahead
	// of the child starting; if the spawn then crashes, the run is still
	// addressable by its session id even though the runs row below never runs.
	act := "run_spawn"
	if resume {
		act = "run_resume"
	}
	sessionDetail, err := json.Marshal(map[string]string{"session_id": sessionID})
	if err != nil {
		return Outcome{}, fmt.Errorf("runner: encode session detail: %w", err)
	}
	if err := s.store.AppendEvent(ctx, journal.JournalEvent{
		TaskID:         req.Task.ID,
		RunID:          runID,
		ActingIdentity: req.Role.Mandate.AgentUserID,
		Act:            act,
		Detail:         sessionDetail,
		ConfigVersion:  req.ConfigVersion,
		HarnessVersion: req.HarnessVersion,
	}); err != nil {
		return Outcome{}, fmt.Errorf("runner: journal session id before spawn: %w", err)
	}

	argv, err := s.buildArgv(req, sessionID, resume)
	if err != nil {
		return Outcome{}, err
	}

	var stdout bytes.Buffer
	startedAt := time.Now()
	spawnErr := s.spawner.Spawn(ctx, workspace.SpawnSpec{
		RoleUser: req.RoleUser,
		Dir:      req.Worktree.Dir,
		Argv:     argv,
		Env:      buildEnv(req, resultPath),
		// The work item rides in on stdin as the user message: -p (bare, in the
		// argv above) makes claude read the prompt from stdin, keeping an
		// arbitrarily long acceptance body off the command line. The playbook is
		// the system prompt (--append-system-prompt-file); this is only the task.
		Stdin:  strings.NewReader(TaskPrompt(*req.Task)),
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	endedAt := time.Now()

	tel := parseTelemetry(stdout.Bytes())
	tel.SessionMatch = tel.ObservedSessionID == sessionID
	exitCode := spawnExitCode(spawnErr)

	// Outcome from the file, never stdout. The read and validate below are the
	// only source of the event: a missing file, unreadable file, or schema
	// violation is result_invalid regardless of what the child printed or its exit
	// status (ADR-0006, RFC-0001 AC-19, AC-21).
	event, rc, raw, valid := deriveOutcome(resultPath)

	if err := s.store.RecordRun(ctx, journal.Run{
		RunID:          runID,
		TaskID:         req.Task.ID,
		SessionID:      sessionID,
		ActingIdentity: req.Role.Mandate.AgentUserID,
		Model:          string(req.Role.ModelTier),
		StartedAt:      startedAt,
		EndedAt:        endedAt,
		TotalCostUSD:   tel.TotalCostUSD,
		Usage:          tel.Usage,
		NumTurns:       tel.NumTurns,
		IsError:        tel.IsError,
		ExitCode:       exitCode,
		HarnessVersion: req.HarnessVersion,
		ConfigVersion:  req.ConfigVersion,
	}); err != nil {
		return Outcome{}, fmt.Errorf("runner: record run: %w", err)
	}

	if err := s.store.StoreResult(ctx, journal.Result{
		RunID:      runID,
		TaskID:     req.Task.ID,
		Raw:        raw,
		Valid:      valid,
		RecordedAt: endedAt,
	}); err != nil {
		return Outcome{}, fmt.Errorf("runner: store result: %w", err)
	}

	return Outcome{
		Event:     event,
		RunID:     runID,
		SessionID: sessionID,
		Valid:     valid,
		Result:    rc,
		RawResult: raw,
		Telemetry: tel,
		ExitCode:  exitCode,
	}, nil
}

// deriveOutcome reads and validates the ResultContract file and maps it to an
// event. It is the whole outcome logic (ADR-0006 file-is-contract): a valid
// completed contract is result_ok-eligible (gates/diff/probe still run in the
// caller, RFC-0001 AC-20); a valid needs_human or failed contract holds for a
// human via result_needs_human, since no automatic edge reaches the failed state
// (RFC-0001 transition table: only human_abandon does) so a failed run routes to
// needs-human like the agent's own escalation; anything else is result_invalid.
func deriveOutcome(resultPath string) (orchestrator.Event, *result.ResultContract, []byte, bool) {
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		return orchestrator.EventResultInvalid, nil, nil, false
	}
	rc, err := result.Parse(raw)
	if err != nil {
		return orchestrator.EventResultInvalid, nil, raw, false
	}
	switch rc.Status {
	case result.StatusCompleted:
		return orchestrator.EventResultOK, rc, raw, true
	default:
		return orchestrator.EventResultNeedsHuman, rc, raw, true
	}
}

// configureGitCredential points the worktree's git at the credential helper so a
// push authenticates as the agent user. Only the helper command lands in the
// config; the delegated token is minted per operation by the helper process and
// never written to the worktree (S-credential-delivery). The skeleton runs one
// task in flight (RFC-0001 §Scope), so this local-config write needs no
// per-worktree isolation yet.
func configureGitCredential(ctx context.Context, worktreeDir, helper string) error {
	return gitConfig(ctx, worktreeDir, "credential.helper", helper)
}

// configureGitAuthor pins the worktree's commit author to the mandate's agent
// user: the playbook tells the spawned agent the author identity is already
// configured and not to touch it, so this must run before the spawn. Without
// it, `git commit` in the worktree inherits the OS pilot user's own global
// gitconfig, breaking writer=agent-user attribution at the commit level even
// though the PR creator (the push, over the agent-user token) is already
// correct. Azure DevOps attributes a commit to an identity by author EMAIL, so
// user.name and user.email both carry the agent user's UPN.
func configureGitAuthor(ctx context.Context, worktreeDir, user string) error {
	if err := gitConfig(ctx, worktreeDir, "user.name", user); err != nil {
		return err
	}
	return gitConfig(ctx, worktreeDir, "user.email", user)
}

// gitConfig writes one value to the worktree's local git config under mandat's
// hardened env (ambient global/system config stripped, prompts forbidden). It is
// the single write path the credential-helper and commit-author setup share.
func gitConfig(ctx context.Context, worktreeDir, key, value string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", worktreeDir, "config", key, value)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runner: git config %s: %w (%s)", key, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// buildEnv is the child's whole environment: an explicit allow-list, never
// os.Environ(). Inheriting the parent env would carry the blueprint secret or a
// delegated token into a process the role OS user controls, the exact breach
// AC-15 forbids. Git credentials reach git only through the on-demand helper
// (S-credential-delivery); the Entra-issued delegated token the identity broker
// holds is never a named var here and buildEnv never calls os.Environ(), so it
// cannot leak through this path either way.
//
// CLAUDE_CODE_OAUTH_TOKEN and ANTHROPIC_API_KEY are a different plane: Claude
// Code's OWN model-auth credential, not the Entra identity-plane token above.
// Without one of these (or a CLAUDE_CONFIG_DIR apiKeyHelper) the spawned claude
// cannot authenticate to run at all, so both are allow-listed straight through
// from the runner's own process env when the operator has set them there —
// still a named-var allow-list, never os.Environ(). PATH is not a secret and the
// child needs it to find its tools.
//
// MANDAT_CLIENT_SECRET_FILE is the pilot's one deliberate exception, and it is
// still AC-15-clean: it carries only the client-secret FILE PATH, never the
// secret value. When the child's own `git push` re-invokes `mandat
// git-credential` as its own subprocess, that helper reads the file on demand
// to mint the agent-user token (S-credential-delivery) — the child never sees
// MANDAT_CLIENT_SECRET itself. Honestly, the pilot runs one OS user with no
// isolation between parent and child, so the child process can read whatever
// the path points at regardless of this var; production's managed-identity
// mode carries no secret file at all, so this line is a pilot-only artifact
// that a future isolated pilot mode should revisit.
func buildEnv(req Request, resultPath string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + req.Home,
		"CLAUDE_CONFIG_DIR=" + req.ConfigDir,
		result.EnvVar + "=" + resultPath,
	}
	if v := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+v)
	}
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		env = append(env, "ANTHROPIC_API_KEY="+v)
	}
	if v := os.Getenv("MANDAT_CLIENT_SECRET_FILE"); v != "" {
		env = append(env, "MANDAT_CLIENT_SECRET_FILE="+v)
	}
	return env
}

// spawnExitCode extracts the child's exit status. A nil error is a clean exit; an
// *exec.ExitError carries the code; anything else (a start failure such as
// workspace.SetupError) could not run a child, reported as -1.
func spawnExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// newUUIDv4 generates an RFC 4122 version-4 UUID from crypto/rand. It stays on
// the stdlib rung of the dependency ladder (ADR-0002): a session id is one value,
// below the bar where a UUID library earns a direct dependency, mirroring how the
// identity package decodes a JWT exp by hand rather than pulling a JWT library.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
