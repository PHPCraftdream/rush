package app

// C9-3 (ox round 9, ASYNC-02): the scope-close cancellation of the session's
// loop schedules ran at most once per run. A turn that follows the first close
// -- the unfinished-todos reminder (A18) or the reviewer pass -- can create a
// loop schedule of its own, and the run then exited with that schedule still
// active: every host of the data dir kept waking the session on a timer.

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// REVERT CHECK: restoring the loopSchedulesCancelled once-per-run flag in
// cancelLoopSchedulesAtClose leaves the schedule the reminder turn created
// active -- this test FAILED (state "active", want "cancelled"; no envelope
// warning).
func TestRunNonInteractive_LoopCreatedByTheReminderTurnIsCancelledAtTheFinalClose(t *testing.T) {
	const todosPending = `{"todos":[{"content":"write it","status":"pending","active_form":"Writing it"}]}`
	const todosDone = `{"todos":[{"content":"write it","status":"completed","active_form":"Writing it"}]}`
	application, sessionID, res, stderr, requests, runErr := wakeE2E(t,
		// Turn 1 (then the scripted "scheduled" answer): leaves an open todo.
		[]string{admissionSSEToolCall("t", "call-todos", "todos", todosPending)},
		60*time.Second,
		func(n int) []string {
			switch n {
			case 3: // the reminder turn: schedules a loop and closes the todo
				return []string{
					admissionSSEToolCall("l", "call-loop", "loop", `{"every_seconds":300,"message":"tick"}`),
					sseToolCallIndex("d", "call-done", "todos", todosDone, 1),
					admissionSSEStop("r", "tool_calls"),
				}
			case 4: // its answer after the tool results
				return []string{admissionSSEText("f", "finished"), admissionSSEStop("f", "stop")}
			}
			return nil
		}, nil)
	require.NoError(t, runErr)

	require.EqualValues(t, 4, requests, "first turn, its answer, the reminder turn, its answer")
	require.Equal(t, "finished", res.FinalText)
	require.Equal(t, "cancelled", wakeScheduleState(t, application, wakeScheduleID(t, application, sessionID)),
		"the loop the reminder turn created must not outlive the run")
	require.Equal(t, 1, strings.Count(stderr.String(), "loop schedule(s) cancelled at run end"))
	var warned bool
	for _, w := range res.Warnings {
		warned = warned || strings.Contains(w, "loop schedule(s) cancelled")
	}
	require.True(t, warned, "the cancellation is an envelope warning, got %v", res.Warnings)
}
