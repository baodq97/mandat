package runner

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/baodq97/mandat/internal/task"
)

// buildArgv assembles the ADR-0006 headless invocation. The flag set and the
// stream-json event shape are an external contract pinned in ADR-0006 and
// referenced, not restated (ADR-0003); flags outside the pipeline's needs are not
// wired (ADR-0004). A fresh run pins --session-id; a resume swaps in --resume for
// the same worktree (ADR-0006 §Session continuity). The work item is not an argv
// flag: -p with no positional prompt makes claude read the user message from
// stdin, so the runner streams TaskPrompt there (see Supervisor.run) rather than
// escaping a possibly long acceptance body onto the command line.
func (s *Supervisor) buildArgv(req Request, sessionID string, resume bool) ([]string, error) {
	settings, err := denySettingsJSON(req.DenyToolHookCommand)
	if err != nil {
		return nil, err
	}
	argv := []string{
		s.cfg.ClaudePath,
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--model", string(req.Role.ModelTier),
		"--permission-mode", "dontAsk",
		"--add-dir", req.Worktree.Dir,
		"--append-system-prompt-file", req.Role.Playbook,
		"--settings", settings,
		"--bare",
	}
	if resume {
		argv = append(argv, "--resume", sessionID)
	} else {
		argv = append(argv, "--session-id", sessionID)
	}
	return append(argv, "--max-budget-usd", strconv.FormatFloat(s.cfg.MaxBudgetUSD, 'f', 2, 64)), nil
}

// TaskPrompt renders the TaskContract into the headless user message: the one
// work item this run acts on — title, acceptance, the remit paths in scope, and
// the tracker ref. It is only the work item. The role's standing instructions
// (commit, push, write the ResultContract) are the playbook, delivered as the
// system prompt via --append-system-prompt-file. That split is the point: the
// system prompt is the fixed role config every task reuses, the user message the
// one thing that varies per run — which is also why this render is a pure
// function of the contract alone (ADR-0006 role/runner seam).
func TaskPrompt(tc task.TaskContract) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Work item %s: %s\n\n", tc.ID, tc.Title)
	fmt.Fprintf(&b, "Acceptance criteria:\n%s\n\n", tc.Acceptance)
	b.WriteString("Remit — the only paths in scope for this task:\n")
	for _, p := range tc.Remit.Paths {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	fmt.Fprintf(&b, "\nTracker: %s %s (%s)\n", tc.TrackerRef.System, tc.TrackerRef.WorkItemID, tc.TrackerRef.URL)
	return b.String()
}

// claudeSettings is the subset of the Claude Code settings schema the runner
// injects inline through --settings: a PreToolUse command hook, the mechanical
// deny layer (ADR-0006), matching the shape of this repo's own .claude/settings.json.
type claudeSettings struct {
	Hooks claudeHooks `json:"hooks"`
}

type claudeHooks struct {
	PreToolUse []claudeHookMatcher `json:"PreToolUse"`
}

type claudeHookMatcher struct {
	Matcher string              `json:"matcher"`
	Hooks   []claudeHookCommand `json:"hooks"`
}

type claudeHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// denySettingsJSON wraps the caller's deny command in the PreToolUse settings
// shape. The runner owns only the JSON structure the --settings flag carries; the
// concrete command is the caller's (a mandat subcommand), the same split as the
// git credential helper. The Write|Edit matcher mirrors the repo's own hook.
func denySettingsJSON(command string) (string, error) {
	s := claudeSettings{
		Hooks: claudeHooks{
			PreToolUse: []claudeHookMatcher{{
				Matcher: "Write|Edit",
				Hooks:   []claudeHookCommand{{Type: "command", Command: command}},
			}},
		},
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("runner: encode settings hook: %w", err)
	}
	return string(b), nil
}
