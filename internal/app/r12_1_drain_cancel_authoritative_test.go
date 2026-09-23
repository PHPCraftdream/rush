package app

// R12-1 (2026-09-23 round-12 audit): a durable drain hands s.eventOwner to
// a fresh recorder (the isCanceled branches in runTurnPhase's select loop)
// without touching s.callResultRec, which still names the SUPERSEDED
// original call. Before this fix, finish() treated reconcileErr==nil
// (a committed row was found for s.callResultRec's own, superseded ID) as
// proof the CURRENT owner's result was authoritative — even though
// handleMessageEvent's own Owns() filter had already rejected replaying
// that superseded row, leaving s.finalReason/s.finalText holding leftover
// state from an EARLIER, not-yet-durably-confirmed live event the drain
// published under the new owner. Combined with the R2-4 "canceled after
// commit" suppression, this fabricated a successful end_turn outcome for a
// cancellation whose real result was never confirmed by the durable queue
// (Ack not yet observed).
//
// This is the audit's own suggested "unit variant": call finish(context.
// Canceled) directly with callResultRec naming a real, committed,
// superseded row (A) and eventOwner set to a DIFFERENT, not-yet-confirmed
// recorder (B) that has already "accepted" a live end_turn event (simulated
// by pre-setting finalReason/finalText, exactly what handleMessageEvent
// would have done when it was live-processing B's own event earlier under
// eventOwner=B). No sleeps or goroutine races needed — finish() must never
// launder a superseded row's successful reconciliation into an
// authoritative result for an owner it doesn't belong to.
//
// REVERT CHECK: in finish() (app_run_reviewer.go), restore the previous
// unconditional `authoritativeTerminal = true` (removing the
// `s.eventOwner.Owns(reconciled.message.ID)` gate) and this test fails:
// finish returns (summary, nil) with ExitReason "end_turn" instead of a
// non-nil error.

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestFinishRejectsSupersededRecorderRowAsAuthoritative_R12_1(t *testing.T) {
	h := newCycle6RunApp(t)

	// A: the original, now-superseded call's own committed row -- what a
	// canceled generation A would have left behind. Its FinishReason is
	// deliberately end_turn too, so the test can tell "the fix correctly
	// refused to trust A's row for a DIFFERENT reason (e.g. its finish
	// reason)" apart from "the fix correctly refused it because eventOwner
	// no longer owns it" -- only the latter is what R12-1 is about.
	aMsg, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "A's superseded text"}},
	})
	require.NoError(t, err)
	aMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), aMsg))

	aRecorder := agent.NewCallResultRecorder()
	aRecorder.RecordConfirmed(aMsg.ID)

	// B: a fresh recorder representing the drain's not-yet-confirmed
	// owner. It does NOT own aMsg.ID.
	bRecorder := agent.NewCallResultRecorder()

	hookExitReason := new(string)
	loop := &executeRunLoop{
		app:            h.app,
		sess:           h.sess,
		ctx:            context.Background(),
		mode:           RunModeJSON,
		stdout:         io.Discard,
		stderr:         io.Discard,
		stopSpinner:    func() {},
		runStart:       time.Now().Add(-time.Second),
		hookExitReason: hookExitReason,

		baselineIDs:    map[string]struct{}{},
		baselineKnown:  true,
		toolCallCounts: map[string]int{},

		// Leftover from B's own live end_turn event, accepted earlier by
		// handleMessageEvent while eventOwner was already B -- exactly
		// what a real durable-drain replacement publishing its own
		// end_turn live event would have left in place.
		finalReason: string(message.FinishReasonEndTurn),
		finalText:   "B's live, not-yet-durably-confirmed text",

		invocationToolCalls:   map[string]int{},
		invocationToolCallIDs: map[string]struct{}{},

		callResultRec: aRecorder,
		eventOwner:    bRecorder,
	}

	result, err := loop.finish(context.Canceled)

	// The fabricated-success symptom (pre-fix) is specifically a NIL error
	// with a clean end_turn envelope -- buildRunResult's ExitReason still
	// reads s.finalReason verbatim regardless of the fix (it is cosmetic,
	// not the bug), so the real discriminator is whether the cancellation
	// actually surfaces as an error the caller can act on.
	require.Error(t, err, "a superseded recorder's own committed row must not fabricate a successful outcome while the current owner's drain is unconfirmed")
	require.ErrorIs(t, err, context.Canceled, "the cancellation must remain visible in the returned error instead of being suppressed by the R2-4 canceled-after-commit branch")
	require.NotNil(t, result, "finish still emits the envelope so stdout output is preserved even though the process exit must be non-zero")
}

// TestFinishAcceptsOwnRecorderRowAsAuthoritative is the positive control:
// when eventOwner IS callResultRec (the ordinary case, no drain handoff in
// flight), finish() must still treat a successfully reconciled row as
// authoritative -- R12-1's fix must not regress the common path.
func TestFinishAcceptsOwnRecorderRowAsAuthoritative(t *testing.T) {
	h := newCycle6RunApp(t)

	msg, err := h.app.Messages.Create(context.Background(), h.sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "own committed text"}},
	})
	require.NoError(t, err)
	msg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, h.app.Messages.Update(context.Background(), msg))

	recorder := agent.NewCallResultRecorder()
	recorder.RecordConfirmed(msg.ID)

	hookExitReason := new(string)
	loop := &executeRunLoop{
		app:            h.app,
		sess:           h.sess,
		ctx:            context.Background(),
		mode:           RunModeJSON,
		stdout:         io.Discard,
		stderr:         io.Discard,
		stopSpinner:    func() {},
		runStart:       time.Now().Add(-time.Second),
		hookExitReason: hookExitReason,

		baselineIDs:    map[string]struct{}{},
		baselineKnown:  true,
		toolCallCounts: map[string]int{},

		invocationToolCalls:   map[string]int{},
		invocationToolCallIDs: map[string]struct{}{},

		callResultRec: recorder,
		eventOwner:    recorder,
	}

	result, err := loop.finish(context.Canceled)

	require.NoError(t, err, "the ordinary no-handoff case must still resolve a committed row as authoritative")
	require.NotNil(t, result)
	require.Equal(t, "end_turn", result.ExitReason)
	require.Equal(t, "own committed text", result.FinalText)
}
