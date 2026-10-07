// Silence lines and guidance escalation for supervision check-ins: a
// command job whose output source reports no writes for long enough is
// flagged as possibly stuck (task items 2+3).
package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// Silence flag policy: a job counts as possibly stuck when it has run at
// least 30 minutes AND been silent for at least half its runtime, floor 20m.
const (
	silenceFlagMinAge    = 30 * time.Minute
	silenceFlagMinSilent = 20 * time.Minute
	silenceLabelMax      = 120
)

// lastOutputReader is the optional interface run_command's output buffer
// already exposes (tools.runCommandOutputBuffer.LastWriteAt); asserted, not
// added to LiveOutputBuffer.
type lastOutputReader interface {
	LastWriteAt() (time.Time, bool)
}

// flaggedJob is one command job this tick flagged as silent: the id needed
// for the escalation line, and the two durations it names.
type flaggedJob struct {
	ToolCallID       string
	StartedAt        time.Time
	Running, Silence time.Duration
}

// jobLastOutputAt reads a snapshot job's last-output time OUTSIDE l.mu:
// the run_command buffer via the optional interface, else a bash shell via
// the background manager. known=false means "silence unknown" -- never a
// flag.
func (l *workLedger) jobLastOutputAt(rootSessionID string, j jobSnapshot) (last time.Time, wrote bool, known bool) {
	if r, ok := j.outputBuf.(lastOutputReader); ok && r != nil {
		last, wrote = r.LastWriteAt()
		return last, wrote, true
	}
	if j.toolName == tools.BashToolName && j.shellID != "" && l.coord != nil && l.coord.background != nil {
		if sh, ok := l.coord.background.GetOwned(rootSessionID, j.shellID); ok {
			last, wrote = sh.LastOutputAt()
			return last, wrote, true
		}
	}
	return time.Time{}, false, false
}

// jobSilence returns how long the job has been silent and whether the
// silence is readable at all. "No output ever" (a live source that reports
// false) counts as silent for the job's whole age.
func (l *workLedger) jobSilence(now time.Time, rootSessionID string, j jobSnapshot) (time.Duration, bool) {
	last, wrote, known := l.jobLastOutputAt(rootSessionID, j)
	if !known {
		return 0, false
	}
	if !wrote {
		return j.elapsed, true
	}
	return now.Sub(last), true
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

// silenceLine renders one command job's check-in line. Unknown silence is
// stated, never flagged.
func (l *workLedger) silenceLine(now time.Time, rootSessionID string, j jobSnapshot) (string, bool) {
	label := silenceLabel(j.input, j.toolName)
	silence, known := l.jobSilence(now, rootSessionID, j)
	if !known {
		return fmt.Sprintf("- %s (%s): %s — running %s, silence unknown",
			j.toolCallID, j.toolName, label, j.elapsed.Round(time.Second)), false
	}
	flagged := silenceFlagged(j.elapsed, silence)
	line := fmt.Sprintf("- %s (%s): %s — running %s, silent %s",
		j.toolCallID, j.toolName, label, j.elapsed.Round(time.Second), silence.Round(time.Second))
	if flagged {
		line += " — SILENT, possibly stuck"
	}
	return line, flagged
}

// silenceFlaggedJobs collects the flagged set for buildSupervisionSummaryAt.
func (l *workLedger) silenceFlaggedJobs(now time.Time, rootSessionID string, snaps []jobSnapshot) []flaggedJob {
	var flagged []flaggedJob
	for _, j := range snaps {
		if j.childSession != "" {
			continue
		}
		if silence, known := l.jobSilence(now, rootSessionID, j); known && silenceFlagged(j.elapsed, silence) {
			flagged = append(flagged, flaggedJob{ToolCallID: j.toolCallID, StartedAt: j.startedAt, Running: j.elapsed, Silence: silence})
		}
	}
	return flagged
}
