// Supervision silence tests (supervision_silence.go + the silence wiring in
// supervision.go). Revert-check map, per test, the single production line it
// catches:
//  1. silenceLine's flagged suffix + guidance prefix ("Verify liveness...")
//  2. silenceFlagged's age/2 silence threshold (recent output -> no flag)
//  3. silenceFlagged's silenceFlagMinAge floor (young job -> no flag)
//  4. jobLastOutputAt's known=false -> "silence unknown", never a flag
//  5. handleSupervisionDeadline's flaggedOnce escalation block + recordProgress's clear
//  6. buildSupervisionSummaryAt's paused oldestFlaggedJob line
//  7. jobLastOutputAt's lastOutputReader interface (run_command-style buffer)
//
// No wall-clock sleeps: job ages are backdated via startedAt and
// buildSupervisionSummaryAt(now, ...) takes the clock; the one real Write
// (test 7) is followed by picking now = time.Now().Add(40m).
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// silenceOutputBuffer is a test-local run_command-style output buffer: it
// implements tools.LiveOutputBuffer plus the optional LastWriteAt() interface
// jobLastOutputAt asserts (tools' real runCommandOutputBuffer constructor is
// unexported, so the package-level test mirrors its contract instead). A nil
// lastWrite means "no output ever" (LastWriteAt reports false).
type silenceOutputBuffer struct {
	mu        sync.Mutex
	data      string
	lastWrite time.Time
}

func (b *silenceOutputBuffer) Read(cursor int64) (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := int64(len(b.data))
	if cursor < 0 || cursor > total {
		cursor = 0
	}
	return b.data[cursor:], total
}

func (b *silenceOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data
}

// Write implements io.Writer, recording the last non-empty write time exactly
// like tools' runCommandOutputBuffer does.
func (b *silenceOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) > 0 {
		b.lastWrite = time.Now()
	}
	b.data += string(p)
	return len(p), nil
}

// LastWriteAt is the optional interface production asserts via lastOutputReader.
func (b *silenceOutputBuffer) LastWriteAt() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastWrite.IsZero() {
		return time.Time{}, false
	}
	return b.lastWrite, true
}

var _ tools.LiveOutputBuffer = (*silenceOutputBuffer)(nil)

// isolateRushDirs points RUSH_GLOBAL_DATA/RUSH_GLOBAL_CONFIG at throwaway
// dirs so store/config fixtures never read (or write) the developer's real
// global state.
func isolateRushDirs(t *testing.T) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
}

// startSilentJob registers an acknowledged bash job with a backdated
// startedAt, an optional output buffer, and an optional shellID — the full
// shape silenceLine/silenceFlaggedJobs read.
func startSilentJob(t *testing.T, l *workLedger, owner, id, input string, buf tools.LiveOutputBuffer, shellID string, startedAt time.Time) {
	t.Helper()
	_, _, err := l.Start(owner, id, input, tools.BashToolName, "", true, false, nil, func() {})
	require.NoError(t, err)
	job := jobOf(l, owner, id)
	l.mu.Lock()
	job.startedAt = startedAt
	job.outputBuf = buf
	job.shellID = shellID
	l.mu.Unlock()
	l.acknowledged(job)
}

// guidanceOf returns the text after the body's final blank line: the
// guidance block buildSupervisionSummaryAt appends.
func guidanceOf(text string) string {
	if i := strings.LastIndex(text, "\n\n"); i >= 0 {
		return text[i+2:]
	}
	return text
}

// TestSilence_AgedJobFlagged: a 60m-old bash job whose last output was 40m
// ago is flagged. Catches deleting silenceLine's " — SILENT, possibly stuck"
// suffix or the flagged guidance prefix in buildSupervisionSummaryAt.
func TestSilence_AgedJobFlagged(t *testing.T) {
	isolateRushDirs(t)
	l, _ := newSupervisionTestLedger(t)
	base := time.Now().Add(-60 * time.Minute)
	buf := &silenceOutputBuffer{lastWrite: base.Add(20 * time.Minute)} // last output 40m before now
	startSilentJob(t, l, "flag-root", "call-flag", `{"command":"sleep 3600","description":"long build"}`, buf, "", base)

	text, flagged := l.buildSupervisionSummaryAt(base.Add(60*time.Minute), "flag-root", 1, time.Minute, false)
	require.Len(t, flagged, 1)
	require.Equal(t, "call-flag", flagged[0].ToolCallID)
	require.Contains(t, text, "- call-flag (bash): long build — running 1h0m0s, silent 40m0s")
	require.Contains(t, text, "SILENT, possibly stuck")
	require.True(t, strings.HasPrefix(guidanceOf(text), "Verify liveness with job_output"),
		"a flagged job's guidance must lead with the liveness check, got: %s", guidanceOf(text))
}

// TestSilence_RecentOutputNotFlagged: 60m old but output 1m ago is not
// flagged. Catches silenceFlagged's silence >= max(20m, age/2) threshold.
func TestSilence_RecentOutputNotFlagged(t *testing.T) {
	isolateRushDirs(t)
	l, _ := newSupervisionTestLedger(t)
	base := time.Now().Add(-60 * time.Minute)
	buf := &silenceOutputBuffer{lastWrite: base.Add(59 * time.Minute)} // last output 1m before now
	startSilentJob(t, l, "fresh-root", "call-fresh", `{"command":"tail -f log"}`, buf, "", base)

	text, flagged := l.buildSupervisionSummaryAt(base.Add(60*time.Minute), "fresh-root", 1, time.Minute, false)
	require.Empty(t, flagged)
	require.NotContains(t, text, "SILENT")
	require.NotContains(t, text, "Verify liveness")
	require.Contains(t, guidanceOf(text), "Keep waiting")
}

// TestSilence_YoungJobNotFlagged: 25m old and silent the whole 25m is below
// the 30m age floor. Catches silenceFlaggedMinAge.
func TestSilence_YoungJobNotFlagged(t *testing.T) {
	isolateRushDirs(t)
	l, _ := newSupervisionTestLedger(t)
	base := time.Now().Add(-25 * time.Minute)
	buf := &silenceOutputBuffer{lastWrite: base} // silent for the whole 25m
	startSilentJob(t, l, "young-root", "call-young", `{"command":"sleep 1500"}`, buf, "", base)

	text, flagged := l.buildSupervisionSummaryAt(base.Add(25*time.Minute), "young-root", 1, time.Minute, false)
	require.Empty(t, flagged)
	require.NotContains(t, text, "SILENT")
	require.Contains(t, guidanceOf(text), "Keep waiting")
}

// TestSilence_UnknownSilenceNeverFlagged: a bash job with no output buffer
// and a shellID with no shell in the background manager has unreadable
// silence. Catches jobSilence's known=false early return (stated, never
// flagged, no liveness guidance).
func TestSilence_UnknownSilenceNeverFlagged(t *testing.T) {
	isolateRushDirs(t)
	l, _ := newSupervisionTestLedger(t)
	startSilentJob(t, l, "unknown-root", "call-unknown", `{"command":"mystery"}`, nil, "", time.Now().Add(-90*time.Minute))

	text, flagged := l.buildSupervisionSummaryAt(time.Now().Add(30*time.Minute), "unknown-root", 1, time.Minute, false)
	require.Empty(t, flagged, "unknown silence must never flag, no matter the age")
	require.Contains(t, text, "- call-unknown (bash): mystery — running 2h0m0s, silence unknown")
	require.NotContains(t, text, "SILENT")
	require.NotContains(t, guidanceOf(text), "Verify liveness")
}

// TestSilence_EscalationSecondConsecutiveTickThenCleared: drives the REAL
// handleSupervisionDeadline (the flaggedOnce block runs there; timeouts are
// injectable via supervisionState interval and the fixture's armFunc) three
// times. Catches the flaggedOnce escalation block's append and
// recordProgress's flaggedOnce = nil clear.
func TestSilence_EscalationSecondConsecutiveTickThenCleared(t *testing.T) {
	isolateRushDirs(t)
	l, coord := newSupervisionTestLedger(t)
	l.timeouts = newTimeoutService(l)
	defer l.timeouts.close()

	coord.subAgentDrivers.register("esc-root", subAgentDriver{agent: &mockSessionAgent{
		runFunc: func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		},
	}})
	base := time.Now().Add(-60 * time.Minute)
	buf := &silenceOutputBuffer{lastWrite: base} // silent for the whole hour
	startSilentJob(t, l, "esc-root", "call-esc", `{"command":"sleep 7200"}`, buf, "", base)

	// Base interval 1h: neither handleSupervisionDeadline's own re-arm nor
	// recordProgress's re-arm can fire a stray tick during the test.
	cfg := SupervisionConfig{Enabled: true, Interval: time.Hour, MaxInterval: time.Hour, MaxNoProgress: 6}
	l.supervision.nextGen = 1
	l.supervision.byRoot["esc-root"] = &supervisionState{rootSessionID: "esc-root", cfg: cfg, interval: cfg.Interval, generation: 1}

	notices := func() int {
		rows, err := l.store.ListSessionNotices(context.Background(), "esc-root")
		require.NoError(t, err)
		return len(rows)
	}
	waitNotices := func(want int) {
		t.Helper()
		require.Eventually(t, func() bool { return notices() >= want }, 5*time.Second, 5*time.Millisecond)
	}

	l.handleSupervisionDeadline("esc-root", 1) // tick 1: flagged, remembered
	waitNotices(1)
	require.NotContains(t, sessionNoticeTextAt(t, l, "esc-root", 0), "has been silent for")

	l.supervision.mu.Lock()
	gen2 := l.supervision.byRoot["esc-root"].generation
	l.supervision.mu.Unlock()
	l.handleSupervisionDeadline("esc-root", gen2) // tick 2: escalation
	waitNotices(2)
	require.Contains(t, sessionNoticeTextAt(t, l, "esc-root", 1),
		"job call-esc has been silent for", "second consecutive silent tick must escalate")

	l.recordProgress("esc-root") // clears flaggedOnce
	l.supervision.mu.Lock()
	gen3 := l.supervision.byRoot["esc-root"].generation
	l.supervision.mu.Unlock()
	l.handleSupervisionDeadline("esc-root", gen3) // tick 3: fresh memory
	waitNotices(3)
	require.NotContains(t, sessionNoticeTextAt(t, l, "esc-root", 2),
		"has been silent for", "recordProgress must clear the escalation memory")
}

// TestSilence_PausingNamesOldestFlaggedJob: pausing (lastTick=true) with a
// flagged job names it with running/silent ages and demands kill-or-justify.
// Catches buildSupervisionSummaryAt's paused oldestFlaggedJob line.
func TestSilence_PausingNamesOldestFlaggedJob(t *testing.T) {
	isolateRushDirs(t)
	l, _ := newSupervisionTestLedger(t)
	base := time.Now().Add(-60 * time.Minute)
	buf := &silenceOutputBuffer{lastWrite: base}
	startSilentJob(t, l, "pause-root", "call-pause", `{"command":"sleep 3600"}`, buf, "", base)

	text, flagged := l.buildSupervisionSummaryAt(base.Add(60*time.Minute), "pause-root", 6, time.Hour, true)
	require.Len(t, flagged, 1)
	require.Contains(t, text, "Supervision is pausing: job call-pause")
	require.Contains(t, text, "has been running 1h0m0s and silent 1h0m0s")
	require.Contains(t, text, "job_kill it now or state why it must keep running")
}

// TestSilence_RunCommandBufferLastWriteAt: a run_command job's output buffer
// is the run_command output buffer shape (LiveOutputBuffer + LastWriteAt);
// one real Write, then now is pushed 40m into the future. Catches
// jobLastOutputAt's lastOutputReader interface assertion.
func TestSilence_RunCommandBufferLastWriteAt(t *testing.T) {
	isolateRushDirs(t)
	l, _ := newSupervisionTestLedger(t)
	buf := &silenceOutputBuffer{}
	_, err := buf.Write([]byte("compiling...\n"))
	require.NoError(t, err)
	startedAt := time.Now() // age 40m when now arrives
	input, err := json.Marshal(map[string]string{"command": "make all", "description": "build"})
	require.NoError(t, err)

	_, _, err = l.Start("rc-root", "call-rc", string(input), tools.RunCommandToolName, "", true, false, nil, func() {})
	require.NoError(t, err)
	rcJob := jobOf(l, "rc-root", "call-rc")
	l.mu.Lock()
	rcJob.startedAt = startedAt
	rcJob.outputBuf = buf
	l.mu.Unlock()
	l.acknowledged(rcJob)

	text, flagged := l.buildSupervisionSummaryAt(startedAt.Add(40*time.Minute), "rc-root", 1, time.Minute, false)
	require.Len(t, flagged, 1, "the LastWriteAt interface must make the buffer's silence readable")
	require.Contains(t, text, "- call-rc (run_command): build — running 40m0s, silent 40m0s")
	require.Contains(t, text, "SILENT, possibly stuck")
}
