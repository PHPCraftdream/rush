// Silence flag policy for supervision check-ins (task items 2+3). The
// batch world has NO output-buffer/last-activity seam on BatchCutoverFacts
// (batch execution output lives on the executor, not on any fact row), so
// the ledger-era per-job silence readers are gone with the workLedger. What
// survives is the pure policy: the age/silence thresholds and the flagged
// carrier, for any future consumer that can prove a last-activity fact.
package agent

import (
	"encoding/json"
	"time"
)

// Silence flag policy: a job counts as possibly stuck when it has run at
// least 30 minutes AND been silent for at least half its runtime, floor 20m.
const (
	silenceFlagMinAge    = 30 * time.Minute
	silenceFlagMinSilent = 20 * time.Minute
	silenceLabelMax      = 120
)

// flaggedJob is one command job a tick flagged as silent: the id needed for
// the escalation line, and the two durations it names. Nothing in the batch
// world produces a flagged set today (no last-activity fact exists); the
// type is retained for the supervision state's shape.
type flaggedJob struct {
	ToolCallID       string
	StartedAt        time.Time
	Running, Silence time.Duration
}

// silenceFlagged is the whole flag rule, in one place for tests.
func silenceFlagged(age, silence time.Duration) bool {
	return age >= silenceFlagMinAge && silence >= max(silenceFlagMinSilent, age/2)
}

// silenceLabel renders a human label from the job's raw input JSON
// {"description","command"}: description, else the command's first line,
// else the tool name; truncated to 120 characters.
func silenceLabel(input, toolName string) string {
	var raw struct {
		Description string `json:"description"`
		Command     string `json:"command"`
	}
	label := ""
	if err := json.Unmarshal([]byte(input), &raw); err == nil {
		if raw.Description != "" {
			label = raw.Description
		} else if raw.Command != "" {
			label = raw.Command
			if i := indexByteAny(label); i >= 0 {
				label = label[:i]
			}
		}
	}
	if label == "" {
		label = toolName
	}
	if len(label) > silenceLabelMax {
		label = label[:silenceLabelMax]
	}
	return label
}

// indexByteAny returns the index of the first newline or carriage return.
func indexByteAny(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return i
		}
	}
	return -1
}
