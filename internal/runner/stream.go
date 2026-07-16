package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// maxStreamLine bounds one stream-json line. Assistant messages can be large, so
// the scanner buffer is generous; a line past it stops the best-effort telemetry
// scan without touching the outcome, which comes from the file, not this stream.
const maxStreamLine = 16 << 20

// streamTailFile is the rolling post-mortem record of the child's stream-json,
// written beside the ResultContract so a run that dies without one still leaves
// evidence of why (live escape: WI 28 died twice with zero post-mortem data). It
// is telemetry, exactly like Telemetry itself (ADR-0006): never consulted for the
// outcome, only read by a human after the fact.
const streamTailFile = "run-stream.tail.jsonl"

// maxStreamTailBytes and maxStreamTailEvents bound the tail so it can never grow
// unbounded: the last 200 events, further capped at 64KB.
const (
	maxStreamTailBytes  = 64 << 10
	maxStreamTailEvents = 200
)

// writeStreamTail persists a bounded tail of the child's raw stream-json to
// controlDir/streamTailFile. It is best-effort by design (the caller only logs a
// failure): a post-mortem aid must never be able to fail the run it is trying to
// explain.
func writeStreamTail(controlDir string, stdout []byte) error {
	if err := os.MkdirAll(controlDir, 0o700); err != nil {
		return fmt.Errorf("runner: create control dir %s: %w", controlDir, err)
	}
	path := filepath.Join(controlDir, streamTailFile)
	if err := os.WriteFile(path, streamTail(stdout), 0o600); err != nil {
		return fmt.Errorf("runner: write stream tail %s: %w", path, err)
	}
	return nil
}

// streamTail returns the last maxStreamTailEvents lines of stdout, further
// trimmed from the head to fit maxStreamTailBytes. It always keeps at least the
// last event when stdout carried any, even if that single line alone exceeds the
// byte cap, so a rolling tail never silently goes empty.
func streamTail(stdout []byte) []byte {
	var events [][]byte
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		events = append(events, line)
	}
	if len(events) == 0 {
		return nil
	}
	if len(events) > maxStreamTailEvents {
		events = events[len(events)-maxStreamTailEvents:]
	}

	start := len(events) - 1
	total := len(events[start]) + 1
	for start > 0 && total+len(events[start-1])+1 <= maxStreamTailBytes {
		start--
		total += len(events[start]) + 1
	}
	events = events[start:]

	var buf bytes.Buffer
	for _, e := range events {
		buf.Write(e)
		buf.WriteByte('\n')
	}
	tail := buf.Bytes()
	if len(tail) > maxStreamTailBytes {
		tail = tail[len(tail)-maxStreamTailBytes:]
	}
	return tail
}

// streamEvent is the subset of a Claude Code stream-json line the runner reads.
// The event type discriminates: system/init carries session_id, the terminal
// result carries the run's cost and usage telemetry (ADR-0006). Every other field
// on every other event type is deliberately ignored — this stream is telemetry,
// never the task outcome.
type streamEvent struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	SessionID    string          `json:"session_id"`
	IsError      bool            `json:"is_error"`
	NumTurns     int             `json:"num_turns"`
	DurationMS   int64           `json:"duration_ms"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	Usage        json.RawMessage `json:"usage"`
}

// parseTelemetry scans the child's stdout for the two events the runner records:
// the session id from the first system/init line, and cost/usage/turns/is_error
// from the terminal result line. Parsing is best-effort by design: a
// non-JSON or unparseable line is skipped, because a corrupted telemetry line must
// never change an outcome that only the ResultContract file decides (ADR-0006).
func parseTelemetry(stdout []byte) Telemetry {
	var tel Telemetry
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" && ev.SessionID != "" && tel.ObservedSessionID == "" {
				tel.ObservedSessionID = ev.SessionID
			}
		case "result":
			tel.Subtype = ev.Subtype
			tel.IsError = ev.IsError
			tel.NumTurns = ev.NumTurns
			tel.DurationMS = ev.DurationMS
			tel.TotalCostUSD = ev.TotalCostUSD
			if len(ev.Usage) > 0 {
				tel.Usage = append([]byte(nil), ev.Usage...)
			}
		}
	}
	return tel
}
