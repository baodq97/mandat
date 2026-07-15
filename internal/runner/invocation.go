package runner

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// buildArgv assembles the ADR-0006 headless invocation. The flag set and the
// stream-json event shape are an external contract pinned in ADR-0006 and
// referenced, not restated (ADR-0003); flags outside the pipeline's needs are not
// wired (ADR-0004). A fresh run pins --session-id; a resume swaps in --resume for
// the same worktree (ADR-0006 §Session continuity). The task prompt / TaskContract
// delivery to the child is not part of this pinned flag set and is left to the
// role/playbook plane, so the runner does not invent a prompt flag.
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
