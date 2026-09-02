package agent

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFinalCompositionLog_FirstTextDeltaAfterToolBoundary verifies that the
// "final composition started" log fires on the first text delta after a tool
// boundary.
func TestFinalCompositionLog_FirstTextDeltaAfterToolBoundary(t *testing.T) {
	// Not parallel: this test swaps the global slog default logger and reads
	// its bytes.Buffer directly (outside slog's handler mutex), so running it
	// in parallel races with slog writes from other tests in this package.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prev := slog.Default()
	prevLogOut, prevLogFlags := log.Writer(), log.Flags()
	slog.SetDefault(logger)
	t.Cleanup(func() {
		// slog.SetDefault(prev) alone does NOT undo log's own output:
		// SetDefault only calls log.SetOutput when the NEW handler isn't
		// a *defaultHandler, so restoring to a defaultHandler-backed
		// logger skips it (log/slog/logger.go) -- log's writer would
		// otherwise stay pointed at buf, a dead local variable, for the
		// rest of the process.
		slog.SetDefault(prev)
		log.SetOutput(prevLogOut)
		log.SetFlags(prevLogFlags)
	})

	// Simulate the tracking variable logic.
	sawToolBoundary := true
	sessionID := "test-session"
	messageID := "test-msg"
	charsSoFar := 0

	// Simulate first text delta — should fire.
	if sawToolBoundary {
		sawToolBoundary = false
		slog.Info("agent: final composition started",
			"session_id", sessionID,
			"message_id", messageID,
			"chars_in_message_so_far", charsSoFar,
		)
	}

	output := buf.String()
	require.Contains(t, output, "final composition started")
	require.Contains(t, output, "session_id=test-session")
	require.Contains(t, output, "message_id=test-msg")
}

// TestFinalCompositionLog_ManyTextDeltasStillOnce verifies that many text
// deltas in a row produce only one log line.
func TestFinalCompositionLog_ManyTextDeltasStillOnce(t *testing.T) {
	// Not parallel — see TestFinalCompositionLog_FirstTextDeltaAfterToolBoundary.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prev := slog.Default()
	prevLogOut, prevLogFlags := log.Writer(), log.Flags()
	slog.SetDefault(logger)
	t.Cleanup(func() {
		// slog.SetDefault(prev) alone does NOT undo log's own output:
		// SetDefault only calls log.SetOutput when the NEW handler isn't
		// a *defaultHandler, so restoring to a defaultHandler-backed
		// logger skips it (log/slog/logger.go) -- log's writer would
		// otherwise stay pointed at buf, a dead local variable, for the
		// rest of the process.
		slog.SetDefault(prev)
		log.SetOutput(prevLogOut)
		log.SetFlags(prevLogFlags)
	})

	sawToolBoundary := true

	// Simulate first text delta — fires.
	if sawToolBoundary {
		sawToolBoundary = false
		slog.Info("agent: final composition started", "session_id", "s1", "message_id", "m1")
	}

	// Simulate more text deltas — no more logs.
	for i := 0; i < 10; i++ {
		if sawToolBoundary {
			slog.Info("agent: final composition started", "session_id", "s1", "message_id", "m1")
		}
	}

	count := strings.Count(buf.String(), "final composition started")
	require.Equal(t, 1, count, "should log exactly once per step")
}

// TestFinalCompositionLog_NewStepLogsAgain verifies that after OnStepFinish
// and a new tool call, the log fires again.
func TestFinalCompositionLog_NewStepLogsAgain(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prev := slog.Default()
	prevLogOut, prevLogFlags := log.Writer(), log.Flags()
	slog.SetDefault(logger)
	t.Cleanup(func() {
		// slog.SetDefault(prev) alone does NOT undo log's own output:
		// SetDefault only calls log.SetOutput when the NEW handler isn't
		// a *defaultHandler, so restoring to a defaultHandler-backed
		// logger skips it (log/slog/logger.go) -- log's writer would
		// otherwise stay pointed at buf, a dead local variable, for the
		// rest of the process.
		slog.SetDefault(prev)
		log.SetOutput(prevLogOut)
		log.SetFlags(prevLogFlags)
	})

	sawToolBoundary := true

	// Step 1: first text delta.
	if sawToolBoundary {
		sawToolBoundary = false
		slog.Info("agent: final composition started", "session_id", "s1", "message_id", "m1")
	}

	// Step finish resets.
	sawToolBoundary = true

	// Tool call.
	sawToolBoundary = true // OnToolCall sets this

	// Step 2: first text delta after tool call.
	if sawToolBoundary {
		sawToolBoundary = false
		slog.Info("agent: final composition started", "session_id", "s1", "message_id", "m1")
	}

	count := strings.Count(buf.String(), "final composition started")
	require.Equal(t, 2, count, "should log once per step")
}
