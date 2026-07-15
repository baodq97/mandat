package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
)

// maxStreamLine bounds one stream-json line. Assistant messages can be large, so
// the scanner buffer is generous; a line past it stops the best-effort telemetry
// scan without touching the outcome, which comes from the file, not this stream.
const maxStreamLine = 16 << 20

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
