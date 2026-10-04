// The in-turn progress guard's own visibility (#1149, step 3): the envelope
// warning buildRunResult adds for a turn the guard stopped, and the single
// stderr line handleMessageEvent prints for it. Without them a guard-stopped
// turn looks like an ordinary end_turn to the orchestrator.

package app

import (
	"bytes"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check: dropping the IsInTurnGuardStop branch in buildRunResult leaves
// the warnings slice empty and this test red on its length; dropping the
// handleMessageEvent branch leaves the stderr buffer empty there.
func TestInTurnGuardStop_VisibleInEnvelopeAndStderr(t *testing.T) {
	const details = "The in-turn progress guard stopped this turn: 5 consecutive steps with no progress, " +
		"1 wait call(s) and 0 already-read window(s). End the turn with a short status and NO tool call."

	res := buildRunResult("s1", "the answer", "", "end_turn", nil, false, map[string]int{"bash": 5},
		100, 0.01, time.Second, agent.InTurnGuardStopTitle, details, 0, "", "", nil, "")
	require.Equal(t, "end_turn", res.ExitReason, "a guard stop is not an error")
	require.Len(t, res.Warnings, 1, "exactly one warning for the guard stop")
	require.Contains(t, res.Warnings[0], "in-turn progress guard")
	require.Contains(t, res.Warnings[0], "5 consecutive steps with no progress")

	// loop_detection's finish carries its own title and must not be mistaken
	// for the guard's: no warning is added for it.
	loop := buildRunResult("s1", "the answer", "", "end_turn", nil, false, map[string]int{"view": 5},
		100, 0.01, time.Second, "Stopped: loop detected", "3 identical calls", 0, "", "", nil, "")
	require.Empty(t, loop.Warnings)
}

func TestHandleMessageEvent_GuardStopPrintsOneStderrLine(t *testing.T) {
	msg := func(id, finishMessage string) pubsub.Event[message.Message] {
		return pubsub.Event[message.Message]{Payload: message.Message{
			ID: id, SessionID: "session", Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "stopped"},
				message.Finish{Reason: message.FinishReasonEndTurn, Message: finishMessage, Details: "d"},
			},
		}}
	}
	newLoop := func(id string) (*executeRunLoop, *bytes.Buffer) {
		stderr := &bytes.Buffer{}
		loop := &executeRunLoop{
			sess: session.Session{ID: "session"}, mode: RunModeTerse, stopSpinner: func() {},
			stdout: &bytes.Buffer{}, stderr: stderr,
			seenToolCalls: map[string]bool{}, toolCallCounts: map[string]int{}, messageReadBytes: map[string]int{},
		}
		recorder := agent.NewCallResultRecorder()
		recorder.Capture()(id)
		loop.eventOwner = recorder
		return loop, stderr
	}

	guardLoop, guardStderr := newLoop("guard-msg")
	require.NoError(t, guardLoop.handleMessageEvent(msg("guard-msg", agent.InTurnGuardStopTitle)))
	require.Equal(t, 1, bytes.Count(guardStderr.Bytes(), []byte("in-turn progress guard")),
		"one stderr line for the turn the guard stopped")
	require.Contains(t, guardStderr.String(), agent.InTurnGuardStopTitle)
	require.Equal(t, agent.InTurnGuardStopTitle, guardLoop.finalErrTitle,
		"the finish title still reaches the envelope")

	plainLoop, plainStderr := newLoop("plain-msg")
	require.NoError(t, plainLoop.handleMessageEvent(msg("plain-msg", "done")))
	require.Empty(t, plainStderr.String(), "an ordinary finish prints nothing")
}
