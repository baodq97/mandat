package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baodq97/mandat/internal/config"
	"github.com/baodq97/mandat/internal/journal"
	"github.com/baodq97/mandat/internal/orchestrator"
	"github.com/baodq97/mandat/internal/result"
	"github.com/baodq97/mandat/internal/role"
	"github.com/baodq97/mandat/internal/task"
	"github.com/baodq97/mandat/internal/workspace"
)

// fakeTaskID is the task the fake claude writes ResultContracts for; the tests'
// TaskContract carries the same id so result.Parse's task_id check passes.
const fakeTaskID = "ado-baodo0220-42"

// The ResultContract bodies the fake claude writes, one per scenario. They are
// package consts so both the fake (which writes them from the re-exec'd child)
// and the assertions (which compare the journaled raw bytes) share one source.
const (
	completedResult     = `{"schema_version":1,"task_id":"ado-baodo0220-42","status":"completed","artifacts":[{"repo":"mandat","branch":"mandat/ado-baodo0220-42","pr_url":"https://dev.azure.com/baodo0220/mandat-dogfood/_git/mandat/pullrequest/7"}]}`
	needsHumanResult    = `{"schema_version":1,"task_id":"ado-baodo0220-42","status":"needs_human","reason":"acceptance criteria ambiguous: which auth mode is expected?"}`
	failedResult        = `{"schema_version":1,"task_id":"ado-baodo0220-42","status":"failed","reason":"build failed: toolchain missing"}`
	invalidSchemaResult = `{"schema_version":1,"task_id":"ado-baodo0220-42","status":"completed"}`
	malformedResult     = `this is not json {`
)

// TestHelperProcess is the §9 fake-claude double. When the runner spawns it (the
// supervisor's claude path is this test binary, re-exec'd by fakeClaudeSpawner),
// GO_WANT_HELPER_PROCESS is set and it acts as a scripted `claude`: it emits
// stream-json telemetry to stdout and then, per MANDAT_FAKE_SCENARIO, writes (or
// withholds) a ResultContract file. Under a normal `go test` run the guard is
// unset and it returns immediately. It is not a real test; the assertions live in
// the tests below.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	args := helperArgs()
	session := flagValue(args, "--session-id")
	if session == "" {
		session = flagValue(args, "--resume")
	}

	// Every scenario emits the SAME success stream: a system/init echoing the
	// pinned session id, then a terminal result claiming success. The only thing
	// that varies is the ResultContract file, so any outcome difference proves the
	// file — not stdout — is the contract (ADR-0006).
	emitSuccessStream(session)

	switch os.Getenv("MANDAT_FAKE_SCENARIO") {
	case "completed":
		writeResultFile(completedResult)
	case "needs_human":
		writeResultFile(needsHumanResult)
	case "failed":
		writeResultFile(failedResult)
	case "invalid_schema":
		writeResultFile(invalidSchemaResult)
	case "malformed":
		writeResultFile(malformedResult)
	case "empty":
		writeResultFile("")
	case "no_file":
		// Write NO file. stdout above already claimed success, and this extra
		// prose line doubles down; a runner that trusted the stream would report
		// result_ok. The file is missing, so the outcome must be result_invalid.
		fmt.Fprintln(os.Stdout, `{"type":"assistant","message":{"content":"All done — draft PR opened at pullrequest/7."}}`)
	case "crash_no_file":
		fmt.Fprintln(os.Stderr, "fatal: agent crashed")
		os.Exit(1)
	}
	os.Exit(0)
}

func emitSuccessStream(session string) {
	fmt.Fprintf(os.Stdout, `{"type":"system","subtype":"init","session_id":%q,"model":"claude-sonnet","tools":[]}`+"\n", session)
	fmt.Fprintf(os.Stdout, `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"num_turns":3,"total_cost_usd":0.4212,"usage":{"input_tokens":1200,"output_tokens":340,"cache_read_input_tokens":800},"session_id":%q}`+"\n", session)
}

func writeResultFile(content string) {
	path := os.Getenv(result.EnvVar)
	if path == "" {
		fmt.Fprintln(os.Stderr, "fake claude: no "+result.EnvVar)
		os.Exit(3)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "fake claude: mkdir:", err)
		os.Exit(3)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake claude: write:", err)
		os.Exit(3)
	}
}

func helperArgs() []string {
	for i, a := range os.Args {
		if a == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

// fakeClaudeSpawner is the direct-exec Spawner test double: instead of dropping to
// a per-role OS user via systemd (needs root, absent on CI), it re-execs this test
// binary as the fake claude (TestHelperProcess), passing the supervisor's argv
// after `--`. It records the spec so a test can assert the ADR-0006 flag set and
// the curated child env, and runs an optional beforeSpawn probe to prove ordering.
type fakeClaudeSpawner struct {
	scenario    string
	got         workspace.SpawnSpec
	beforeSpawn func(workspace.SpawnSpec)
}

func (f *fakeClaudeSpawner) Spawn(ctx context.Context, spec workspace.SpawnSpec) error {
	f.got = spec
	if f.beforeSpawn != nil {
		f.beforeSpawn(spec)
	}
	args := append([]string{"-test.run=TestHelperProcess", "--"}, spec.Argv...)
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(append([]string(nil), spec.Env...),
		"GO_WANT_HELPER_PROCESS=1",
		"MANDAT_FAKE_SCENARIO="+f.scenario,
	)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	return cmd.Run()
}

func openStore(t *testing.T) *journal.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mandat.db")
	s, err := journal.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("journal.Open(%q) error = %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close() error = %v", err)
		}
	})
	return s
}

func newRequest(t *testing.T, tier config.ModelTier) Request {
	t.Helper()
	return Request{
		Task: &task.TaskContract{ID: fakeTaskID},
		Role: role.Role{
			Name: "dev",
			Mandate: role.MandateRef{
				AgentIdentityID: "11111111-1111-1111-1111-111111111111",
				AgentUserID:     "agent-user-dev-01@baotest.onmicrosoft.com",
			},
			Playbook:        "/etc/mandat/playbooks/dev.md",
			AutonomyCeiling: config.CeilingDraftPR,
			ModelTier:       tier,
		},
		Worktree:            &workspace.Workspace{Dir: t.TempDir(), Branch: "mandat/" + fakeTaskID},
		RoleUser:            "mandat-dev",
		Home:                t.TempDir(),
		ConfigDir:           t.TempDir(),
		DenyToolHookCommand: `mandat remit-guard --worktree "$CLAUDE_PROJECT_DIR"`,
	}
}

func flagValue(argv []string, name string) string {
	for i := range len(argv) - 1 {
		if argv[i] == name {
			return argv[i+1]
		}
	}
	return ""
}

func hasFlag(argv []string, name string) bool {
	for _, a := range argv {
		if a == name {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

func assertFlagValue(t *testing.T, argv []string, name, want string) {
	t.Helper()
	if got := flagValue(argv, name); got != want {
		t.Errorf("argv flag %s = %q, want %q", name, got, want)
	}
}

// TestSupervisor_Run_HappyPath is scenario (a): a valid completed ResultContract
// yields result_ok, the pinned session id is journaled before the spawn and flows
// intact to the argv, the init event, and runs.session_id, and the runs row
// records the stdout telemetry.
func TestSupervisor_Run_HappyPath(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	req := newRequest(t, config.ModelSonnet)

	// beforeSpawn runs at the moment the child starts: the run_spawn journal row
	// must already be present, proving the session id was recorded BEFORE the
	// spawn (ADR-0006, RFC-0001 AC-13).
	var journaledSession string
	spawner := &fakeClaudeSpawner{scenario: "completed", beforeSpawn: func(spec workspace.SpawnSpec) {
		events, err := store.Events(ctx, fakeTaskID)
		if err != nil {
			t.Errorf("Events() at spawn error = %v", err)
			return
		}
		for _, e := range events {
			if e.Act == "run_spawn" {
				var d map[string]string
				if err := json.Unmarshal(e.Detail, &d); err != nil {
					t.Errorf("unmarshal run_spawn detail: %v", err)
				}
				journaledSession = d["session_id"]
			}
		}
		if journaledSession == "" {
			t.Error("no run_spawn journal row at spawn: session id was not recorded before the child started")
		}
		if argv := flagValue(spec.Argv, "--session-id"); argv != journaledSession {
			t.Errorf("argv --session-id %q != journaled session %q", argv, journaledSession)
		}
	}}

	sup := New(store, spawner, Config{ClaudePath: os.Args[0], MaxBudgetUSD: 5.00})
	out, err := sup.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if out.Event != orchestrator.EventResultOK {
		t.Errorf("event = %q, want %q", out.Event, orchestrator.EventResultOK)
	}
	if !out.Valid || out.Result == nil || out.Result.Status != result.StatusCompleted {
		t.Fatalf("outcome valid=%v result=%+v, want a valid completed contract", out.Valid, out.Result)
	}
	if len(out.Result.Artifacts) != 1 || out.Result.Artifacts[0].PRURL == "" {
		t.Errorf("artifacts = %+v, want one with a pr_url", out.Result.Artifacts)
	}

	// The session id is one value across the whole chain.
	if out.SessionID != journaledSession {
		t.Errorf("outcome session %q != journaled %q", out.SessionID, journaledSession)
	}
	if !out.Telemetry.SessionMatch || out.Telemetry.ObservedSessionID != out.SessionID {
		t.Errorf("init session %q did not match pinned %q (match=%v)", out.Telemetry.ObservedSessionID, out.SessionID, out.Telemetry.SessionMatch)
	}

	run, err := store.LoadRun(ctx, out.RunID)
	if err != nil {
		t.Fatalf("LoadRun(%q) error = %v", out.RunID, err)
	}
	if run.SessionID != out.SessionID {
		t.Errorf("runs.session_id = %q, want %q", run.SessionID, out.SessionID)
	}
	if run.ActingIdentity != req.Role.Mandate.AgentUserID {
		t.Errorf("runs.acting_identity = %q, want the agent user %q", run.ActingIdentity, req.Role.Mandate.AgentUserID)
	}
	if run.Model != string(config.ModelSonnet) {
		t.Errorf("runs.model = %q, want %q", run.Model, config.ModelSonnet)
	}
	if run.TotalCostUSD <= 0 {
		t.Errorf("runs.total_cost_usd = %v, want the stream's cost", run.TotalCostUSD)
	}
	if run.NumTurns != 3 {
		t.Errorf("runs.num_turns = %d, want 3", run.NumTurns)
	}
	if !strings.Contains(string(run.Usage), "input_tokens") {
		t.Errorf("runs.usage = %q, want the stream's usage object", run.Usage)
	}
	if run.ExitCode != 0 {
		t.Errorf("runs.exit_code = %d, want 0", run.ExitCode)
	}

	// The runner spawned the ADR-0006 flag set under the per-role OS user in the
	// worktree (AC-6.1/AC-6.4).
	if spawner.got.RoleUser != req.RoleUser {
		t.Errorf("spawn RoleUser = %q, want %q", spawner.got.RoleUser, req.RoleUser)
	}
	if spawner.got.Dir != req.Worktree.Dir {
		t.Errorf("spawn Dir = %q, want the worktree %q", spawner.got.Dir, req.Worktree.Dir)
	}
	if spawner.got.Argv[0] != os.Args[0] {
		t.Errorf("argv[0] = %q, want the claude path %q", spawner.got.Argv[0], os.Args[0])
	}
	for _, f := range []string{"-p", "--verbose", "--bare"} {
		if !hasFlag(spawner.got.Argv, f) {
			t.Errorf("argv missing %q", f)
		}
	}
	assertFlagValue(t, spawner.got.Argv, "--output-format", "stream-json")
	assertFlagValue(t, spawner.got.Argv, "--permission-mode", "dontAsk")
	assertFlagValue(t, spawner.got.Argv, "--add-dir", req.Worktree.Dir)
	assertFlagValue(t, spawner.got.Argv, "--model", "sonnet")
	assertFlagValue(t, spawner.got.Argv, "--append-system-prompt-file", req.Role.Playbook)
	assertFlagValue(t, spawner.got.Argv, "--max-budget-usd", "5.00")
	assertFlagValue(t, spawner.got.Argv, "--session-id", out.SessionID)

	// The raw ResultContract bytes and valid=1 landed in results.
	results, err := store.Results(ctx, out.RunID)
	if err != nil {
		t.Fatalf("Results() error = %v", err)
	}
	if len(results) != 1 || !results[0].Valid || string(results[0].Raw) != completedResult {
		t.Errorf("results = %+v, want one valid row carrying the exact bytes", results)
	}
}

// TestSupervisor_Run_InvalidContract is scenario (b): a present-but-unusable
// ResultContract routes to result_invalid, and the raw bytes plus valid=0 are
// journaled (RFC-0001 AC-21). Covers the schema-invalid, malformed-JSON, and
// empty-file variants.
func TestSupervisor_Run_InvalidContract(t *testing.T) {
	cases := []struct {
		scenario string
		wantRaw  string
	}{
		{"invalid_schema", invalidSchemaResult},
		{"malformed", malformedResult},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.scenario, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t)
			req := newRequest(t, config.ModelSonnet)
			spawner := &fakeClaudeSpawner{scenario: tc.scenario}
			sup := New(store, spawner, Config{ClaudePath: os.Args[0], MaxBudgetUSD: 5})

			out, err := sup.Run(ctx, req)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if out.Event != orchestrator.EventResultInvalid {
				t.Errorf("event = %q, want %q", out.Event, orchestrator.EventResultInvalid)
			}
			if out.Valid || out.Result != nil {
				t.Errorf("outcome valid=%v result=%+v, want invalid/nil", out.Valid, out.Result)
			}

			results, err := store.Results(ctx, out.RunID)
			if err != nil {
				t.Fatalf("Results() error = %v", err)
			}
			if len(results) != 1 || results[0].Valid {
				t.Fatalf("results = %+v, want one row with valid=0", results)
			}
			if string(results[0].Raw) != tc.wantRaw {
				t.Errorf("results raw = %q, want %q", results[0].Raw, tc.wantRaw)
			}
		})
	}
}

// TestSupervisor_Run_NeedsHuman is scenario (c): a valid needs_human
// ResultContract routes to result_needs_human and surfaces the reason.
func TestSupervisor_Run_NeedsHuman(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	req := newRequest(t, config.ModelSonnet)
	spawner := &fakeClaudeSpawner{scenario: "needs_human"}
	sup := New(store, spawner, Config{ClaudePath: os.Args[0], MaxBudgetUSD: 5})

	out, err := sup.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.Event != orchestrator.EventResultNeedsHuman {
		t.Errorf("event = %q, want %q", out.Event, orchestrator.EventResultNeedsHuman)
	}
	if !out.Valid || out.Result == nil || out.Result.Status != result.StatusNeedsHuman {
		t.Fatalf("outcome = %+v, want a valid needs_human contract", out.Result)
	}
	if out.Result.Reason == "" {
		t.Error("needs_human contract surfaced no reason")
	}

	results, err := store.Results(ctx, out.RunID)
	if err != nil {
		t.Fatalf("Results() error = %v", err)
	}
	if len(results) != 1 || !results[0].Valid {
		t.Errorf("results = %+v, want one valid row", results)
	}
}

// TestSupervisor_Run_StdoutSuccessButNoFile is scenario (d), the load-bearing
// proof: the fake prints the SAME success stream as the happy path but writes no
// file. The runner captures that success telemetry yet derives result_invalid,
// because the outcome comes only from the file (ADR-0006). The single difference
// between this and the happy path is the file's presence.
func TestSupervisor_Run_StdoutSuccessButNoFile(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	req := newRequest(t, config.ModelSonnet)
	spawner := &fakeClaudeSpawner{scenario: "no_file"}
	sup := New(store, spawner, Config{ClaudePath: os.Args[0], MaxBudgetUSD: 5})

	out, err := sup.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.Event != orchestrator.EventResultOK && out.Event != orchestrator.EventResultInvalid {
		t.Fatalf("unexpected event %q", out.Event)
	}
	if out.Event == orchestrator.EventResultOK {
		t.Fatal("event = result_ok from a stdout success with NO file: the runner trusted the stream, not the file")
	}
	if out.Event != orchestrator.EventResultInvalid || out.Valid || out.Result != nil {
		t.Errorf("outcome event=%q valid=%v result=%+v, want result_invalid", out.Event, out.Valid, out.Result)
	}

	// The success telemetry WAS read from stdout — it just is not the outcome.
	if out.Telemetry.Subtype != "success" || out.Telemetry.IsError {
		t.Errorf("telemetry subtype=%q is_error=%v, want the stream's success telemetry", out.Telemetry.Subtype, out.Telemetry.IsError)
	}
	if out.Telemetry.ObservedSessionID != out.SessionID {
		t.Errorf("telemetry session %q, want the init echo %q", out.Telemetry.ObservedSessionID, out.SessionID)
	}

	results, err := store.Results(ctx, out.RunID)
	if err != nil {
		t.Fatalf("Results() error = %v", err)
	}
	if len(results) != 1 || results[0].Valid || len(results[0].Raw) != 0 {
		t.Errorf("results = %+v, want one row with valid=0 and empty raw (no file was written)", results)
	}
}

// TestSupervisor_Run_ChildCrashNoFile confirms a non-zero exit with no file is
// result_invalid too, and the exit code lands in the runs row.
func TestSupervisor_Run_ChildCrashNoFile(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	req := newRequest(t, config.ModelSonnet)
	spawner := &fakeClaudeSpawner{scenario: "crash_no_file"}
	sup := New(store, spawner, Config{ClaudePath: os.Args[0], MaxBudgetUSD: 5})

	out, err := sup.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if out.Event != orchestrator.EventResultInvalid {
		t.Errorf("event = %q, want %q", out.Event, orchestrator.EventResultInvalid)
	}
	if out.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", out.ExitCode)
	}
	run, err := store.LoadRun(ctx, out.RunID)
	if err != nil {
		t.Fatalf("LoadRun() error = %v", err)
	}
	if run.ExitCode != 1 {
		t.Errorf("runs.exit_code = %d, want 1", run.ExitCode)
	}
}

// TestSupervisor_Run_ChildEnvCarriesNoParentSecret is AC-15/AC-6.5: the child env
// is an allow-list, so a secret in the parent process env never reaches the child,
// and the per-role HOME/CLAUDE_CONFIG_DIR are set and distinct from the parent's.
func TestSupervisor_Run_ChildEnvCarriesNoParentSecret(t *testing.T) {
	const secret = "super-secret-delegated-entra-token-xyz"
	t.Setenv("MANDAT_TEST_ENTRA_TOKEN", secret)

	ctx := context.Background()
	store := openStore(t)
	req := newRequest(t, config.ModelSonnet)
	spawner := &fakeClaudeSpawner{scenario: "completed"}
	sup := New(store, spawner, Config{ClaudePath: os.Args[0], MaxBudgetUSD: 5})

	if _, err := sup.Run(ctx, req); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, e := range spawner.got.Env {
		if strings.Contains(e, secret) {
			t.Errorf("child env leaked the parent secret: %q", e)
		}
		if strings.HasPrefix(e, "MANDAT_TEST_ENTRA_TOKEN=") {
			t.Errorf("child env inherited parent var %q; buildEnv must be an allow-list", e)
		}
	}
	if v, ok := envValue(spawner.got.Env, "HOME"); !ok || v != req.Home {
		t.Errorf("child HOME = %q (present=%v), want the per-role %q", v, ok, req.Home)
	}
	if v, ok := envValue(spawner.got.Env, "CLAUDE_CONFIG_DIR"); !ok || v != req.ConfigDir {
		t.Errorf("child CLAUDE_CONFIG_DIR = %q (present=%v), want the per-role %q", v, ok, req.ConfigDir)
	}
	if v, ok := envValue(spawner.got.Env, result.EnvVar); !ok || v == "" {
		t.Errorf("child %s = %q (present=%v), want the worktree result path", result.EnvVar, v, ok)
	}
	if _, ok := envValue(spawner.got.Env, "PATH"); !ok {
		t.Error("child env has no PATH; the agent needs it to find its tools")
	}
	if req.Home == os.Getenv("HOME") {
		t.Error("test setup: the per-role HOME must differ from the parent's to prove isolation")
	}
}

// TestSupervisor_Resume_UsesResumeFlag proves the resume path swaps --session-id
// for --resume against the existing session in the same worktree, and journals it.
func TestSupervisor_Resume_UsesResumeFlag(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	req := newRequest(t, config.ModelSonnet)
	spawner := &fakeClaudeSpawner{scenario: "completed"}
	sup := New(store, spawner, Config{ClaudePath: os.Args[0], MaxBudgetUSD: 5})

	const existing = "11111111-2222-4333-8444-555555555555"
	out, err := sup.Resume(ctx, req, existing)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	assertFlagValue(t, spawner.got.Argv, "--resume", existing)
	if hasFlag(spawner.got.Argv, "--session-id") {
		t.Error("resume argv pins a fresh --session-id; it must reuse the existing session")
	}
	if out.SessionID != existing {
		t.Errorf("outcome session = %q, want the resumed %q", out.SessionID, existing)
	}

	events, err := store.Events(ctx, fakeTaskID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Act == "run_resume" {
			var d map[string]string
			_ = json.Unmarshal(e.Detail, &d)
			if d["session_id"] == existing {
				found = true
			}
		}
	}
	if !found {
		t.Error("no run_resume journal row carrying the existing session id")
	}
}

func TestBuildArgv_ADR0006FlagSet(t *testing.T) {
	t.Parallel()
	sup := New(nil, nil, Config{ClaudePath: "/opt/claude", MaxBudgetUSD: 5})

	opus := bareRequest(config.ModelOpus)
	argv, err := sup.buildArgv(opus, "sess-123", false)
	if err != nil {
		t.Fatalf("buildArgv() error = %v", err)
	}
	if argv[0] != "/opt/claude" {
		t.Errorf("argv[0] = %q, want the claude path", argv[0])
	}
	assertFlagValue(t, argv, "--model", "opus") // AC-6.2 per-role override
	assertFlagValue(t, argv, "--session-id", "sess-123")
	assertFlagValue(t, argv, "--add-dir", "/wt")
	assertFlagValue(t, argv, "--max-budget-usd", "5.00")
	for _, f := range []string{"-p", "--verbose", "--bare"} {
		if !hasFlag(argv, f) {
			t.Errorf("argv missing %q", f)
		}
	}

	// --settings carries a well-formed PreToolUse deny hook wrapping the command.
	var parsed claudeSettings
	if err := json.Unmarshal([]byte(flagValue(argv, "--settings")), &parsed); err != nil {
		t.Fatalf("--settings is not valid JSON: %v", err)
	}
	if len(parsed.Hooks.PreToolUse) != 1 || len(parsed.Hooks.PreToolUse[0].Hooks) != 1 {
		t.Fatalf("--settings = %+v, want one PreToolUse command hook", parsed)
	}
	if parsed.Hooks.PreToolUse[0].Hooks[0].Command != opus.DenyToolHookCommand {
		t.Errorf("hook command = %q, want %q", parsed.Hooks.PreToolUse[0].Hooks[0].Command, opus.DenyToolHookCommand)
	}

	// Default tier resolves to sonnet (AC-6.2 default).
	sonnet := bareRequest(config.ModelSonnet)
	argvS, _ := sup.buildArgv(sonnet, "s", false)
	assertFlagValue(t, argvS, "--model", "sonnet")

	// Resume swaps the session flag.
	argvR, _ := sup.buildArgv(opus, "sess-123", true)
	assertFlagValue(t, argvR, "--resume", "sess-123")
	if hasFlag(argvR, "--session-id") {
		t.Error("resume argv must not carry --session-id")
	}
}

func bareRequest(tier config.ModelTier) Request {
	return Request{
		Task:                &task.TaskContract{ID: fakeTaskID},
		Role:                role.Role{Name: "dev", ModelTier: tier, Playbook: "/pb/dev.md", Mandate: role.MandateRef{AgentUserID: "au@x"}},
		Worktree:            &workspace.Workspace{Dir: "/wt"},
		DenyToolHookCommand: "mandat remit-guard",
	}
}

func TestDeriveOutcome_StatusMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		content   string
		exists    bool
		wantEvent orchestrator.Event
		wantValid bool
	}{
		{"completed", completedResult, true, orchestrator.EventResultOK, true},
		{"needs_human", needsHumanResult, true, orchestrator.EventResultNeedsHuman, true},
		{"failed", failedResult, true, orchestrator.EventResultNeedsHuman, true},
		{"invalid_schema", invalidSchemaResult, true, orchestrator.EventResultInvalid, false},
		{"malformed", malformedResult, true, orchestrator.EventResultInvalid, false},
		{"empty", "", true, orchestrator.EventResultInvalid, false},
		{"missing", "", false, orchestrator.EventResultInvalid, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), result.Path)
			if tc.exists {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			event, rc, raw, valid := deriveOutcome(path)
			if event != tc.wantEvent {
				t.Errorf("event = %q, want %q", event, tc.wantEvent)
			}
			if valid != tc.wantValid {
				t.Errorf("valid = %v, want %v", valid, tc.wantValid)
			}
			if valid != (rc != nil) {
				t.Errorf("valid=%v but rc=%v: the contract pointer must track validity", valid, rc)
			}
			if !tc.exists && raw != nil {
				t.Errorf("missing-file raw = %q, want nil", raw)
			}
		})
	}
}

// TestSupervisor_WiresGitCredentialHelper is the credential-wiring seam: the
// worktree's git config points at the `mandat git-credential` helper, so a push
// authenticates as the agent user. The config carries the helper COMMAND, never
// the delegated token (S-credential-delivery invariant).
func TestSupervisor_WiresGitCredentialHelper(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	gitInit(t, dir)

	const helper = `!/usr/local/bin/mandat git-credential --role dev`
	if err := configureGitCredential(ctx, dir, helper); err != nil {
		t.Fatalf("configureGitCredential() error = %v", err)
	}
	if got := gitConfigGet(t, dir, "credential.helper"); got != helper {
		t.Errorf("credential.helper = %q, want %q", got, helper)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	if !strings.Contains(string(cfg), "git-credential") {
		t.Errorf(".git/config does not carry the helper command:\n%s", cfg)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
}

func gitConfigGet(t *testing.T, dir, key string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "config", "--get", key)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git config --get %s: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}
