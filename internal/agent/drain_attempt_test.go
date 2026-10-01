// Drain attempt accounting (docs/reviews/2026-09-30-async-phase4-round2-
// attempts-design.md sec.1.4/5): one accounting point in the turn loop that
// ran the leg, from the DB's own verdict. Real SQLite, real *sessionAgent,
// httptest providers; faults are SQLite triggers.
package agent

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// emptyReplyResponse serves ONE step that finishes cleanly (stop) with no
// content at all: no error, one request, and nothing to record as a reaction.
func emptyReplyResponse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":0,"total_tokens":5}}`)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if fl != nil {
		fl.Flush()
	}
}

// stalledStreamResponse sends one chunk and then goes silent until the client
// gives up.
func stalledStreamResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	sseChunk(w, fl, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

func unauthorizedResponse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprint(w, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`)
}

// A1: a fresh notice is still delivery='pending' before the Drain's own pull.
// The snapshot is taken AFTER the pull, so the very first attempt counts.
//
// Revert-check: taking the snapshot before the pull (empty for a pending row)
// leaves wake_attempts 0 and this test red.
func TestDrainAttempt_FreshPendingNoticeFirstAttemptCounted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-first-counted", attemptFixtureOpts{noIdle: true, handler: emptyReplyResponse})
	f.seedDebt(ctx, "call-1", false)

	_, _ = f.drainRun(ctx)

	require.EqualValues(t, 1, f.requests.Load())
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.WakeAttempts, "the first attempt on a freshly pulled notice is counted")
	require.EqualValues(t, 0, row.Reacted)
	require.True(t, f.inRecheckSet(), "an unreacted attempt goes to the re-check set")
	require.Zero(t, f.markers(ctx))
}

// A3: a watchdog stall surfaces as context.Canceled but is a real, paid
// attempt: it is counted, not exempted.
//
// Revert-check: exempting every Canceled error regardless of att.stalled
// (drainAttemptExempt) leaves wake_attempts 0 and this test red.
func TestDrainAttempt_StallIsCountedNotExempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-stall", attemptFixtureOpts{
		noIdle: true, handler: stalledStreamResponse,
		streamIdle: 200 * time.Millisecond, streamTick: 20 * time.Millisecond,
	})
	f.seedDebt(ctx, "call-1", false)

	_, err := f.drainRun(ctx)
	require.Error(t, err)

	require.EqualValues(t, 1, f.row(ctx, "call-1").WakeAttempts, "a stall is counted")
	require.True(t, f.inRecheckSet())
}

// A5: the reaction write silently fails (fantasy swallows OnStepFinish's
// error): the rows stay unreacted, every attempt is counted and the debt is
// closed by failure at K=3 with exactly one marker.
//
// Revert-check: removing IncrementWakeAttempts from accountDrainAttempt
// leaves the counter at 0, the debt never closes and this test red.
func TestDrainAttempt_SwallowedReactionWrite_SettlesAtK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-swallowed", attemptFixtureOpts{noIdle: true})
	f.seedDebt(ctx, "call-1", false)
	f.blockReactions(ctx)

	for i := 1; i <= 2; i++ {
		_, _ = f.drainRun(ctx)
		f.ledger.resetDrainGate(f.sessID) // the retry pause elapses
		row := f.row(ctx, "call-1")
		require.EqualValues(t, i, row.WakeAttempts, "attempt %d is counted", i)
		require.EqualValues(t, 0, row.Reacted, "attempt %d must not close the debt yet", i)
	}
	_, _ = f.drainRun(ctx)

	row := f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.Reacted, "the third counted attempt closes the debt")
	require.EqualValues(t, 1, row.ReactedFailed)
	require.Equal(t, 1, f.markers(ctx), "exactly one marker")
	require.EqualValues(t, 3, f.requests.Load())
}

// A8: a terminal provider failure (401) closes the debt on the FIRST attempt.
//
// Revert-check: dropping the drainFailureTerminal branch (count only) leaves
// the debt open after one attempt and this test red.
func TestDrainAttempt_401SettlesFirstAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-401", attemptFixtureOpts{noIdle: true, handler: unauthorizedResponse})
	f.seedDebt(ctx, "call-1", false)

	_, err := f.drainRun(ctx)
	require.Error(t, err)

	row := f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.Reacted)
	require.EqualValues(t, 1, row.ReactedFailed)
	require.Equal(t, 1, f.markers(ctx))
}

// A9a: a process shutdown mid-turn is not evidence about the debt.
//
// Revert-check: making drainAttemptExempt always false leaves wake_attempts 1
// and this test red.
func TestDrainAttempt_ShutdownIsExempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-shutdown", attemptFixtureOpts{noIdle: true, handler: stalledStreamResponse})
	f.coord.currentAgent = &mockSessionAgent{}
	f.seedDebt(ctx, "call-1", false)

	done := make(chan error, 1)
	go func() { _, err := f.drainRun(ctx); done <- err }()
	require.Eventually(t, func() bool { return f.requests.Load() == 1 }, 10*time.Second, 10*time.Millisecond)
	f.coord.CancelAll()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the Drain never returned after CancelAll")
	}

	row := f.row(ctx, "call-1")
	require.EqualValues(t, 0, row.WakeAttempts, "a shutdown is not counted")
	require.EqualValues(t, 0, row.Reacted)
	require.Zero(t, f.markers(ctx))
}

// A9b: a bare Cancel of the live turn with nothing queued (`sessions cancel`)
// is not evidence about the debt either. Stop and an interrupt that hand the
// session a human message are pinned by drain_handover_test.go.
//
// Revert-check: as A9a.
func TestDrainAttempt_CancelTurnIsExempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-cancel", attemptFixtureOpts{noIdle: true, handler: stalledStreamResponse})
	f.seedDebt(ctx, "call-1", false)

	done := make(chan error, 1)
	go func() { _, err := f.drainRun(ctx); done <- err }()
	require.Eventually(t, func() bool { return f.requests.Load() == 1 }, 10*time.Second, 10*time.Millisecond)
	f.sa.Cancel(f.sessID)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the Drain never returned after Cancel")
	}

	row := f.row(ctx, "call-1")
	require.EqualValues(t, 0, row.WakeAttempts)
	require.EqualValues(t, 0, row.Reacted)
	require.Zero(t, f.markers(ctx))
}

// A10: a user turn queued behind a Drain and failing with a terminal
// provider error is not charged to the Drain: the Drain leg is accounted on
// its own outcome (a swallowed reaction write: counted once, no settle).
//
// Revert-check: accounting the Run's final error against the Drain's
// snapshot (the old launcher accounting) settles the debt by failure here and
// this test goes red.
func TestDrainAttempt_QueuedUserTurnFailureNotCharged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-queued-user", attemptFixtureOpts{noIdle: true})
	f.seedDebt(ctx, "call-1", false)
	f.blockReactions(ctx)

	queued := make(chan struct{})
	var mu sync.Mutex
	n := 0
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		if cur == 1 {
			<-queued // hold the Drain until the user call is queued behind it
			textFinishResponse(w, "reacted")
			return
		}
		unauthorizedResponse(w, r)
	})

	done := make(chan error, 1)
	go func() { _, err := f.drainRun(ctx); done <- err }()
	require.Eventually(t, func() bool { return f.requests.Load() == 1 }, 10*time.Second, 10*time.Millisecond)
	res, err := f.sa.Run(ctx, SessionAgentCall{SessionID: f.sessID, Prompt: "hello"})
	require.NoError(t, err)
	require.Nil(t, res, "the user call queues behind the running Drain")
	close(queued)
	select {
	case err := <-done:
		require.Error(t, err, "the queued user turn's 401 is the Run's final error")
	case <-time.After(20 * time.Second):
		t.Fatal("the Drain never returned")
	}

	row := f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.WakeAttempts, "the Drain leg itself is counted once")
	require.EqualValues(t, 0, row.Reacted, "the user turn's failure must not settle the Drain's debt")
	require.EqualValues(t, 0, row.ReactedFailed)
	require.Zero(t, f.markers(ctx))
}

// The marker names the snapshot's own tool_call_ids (an archived reused id
// shows the id the model used).
//
// Revert-check: writing the marker without the ids leaves "call-1" out and
// this test red.
func TestSettleDrainDebt_MarkerNamesSnapshotToolCallIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-marker-ids", attemptFixtureOpts{noIdle: true})
	f.seedDebt(ctx, "call-1", true)
	f.seedDebt(ctx, "call-2", true)
	snap, err := f.store.CaptureDebtSnapshot(ctx, f.sessID)
	require.NoError(t, err)
	require.Len(t, snap.Jobs, 2)

	require.NoError(t, f.coord.settleDrainDebt(ctx, f.sessID, snap, "boom"))

	notices, err := f.store.ListSessionNotices(ctx, f.sessID)
	require.NoError(t, err)
	var text string
	for _, no := range notices {
		if no.Kind == session.NoticeKindWakeFailed {
			text = no.Text
		}
	}
	require.Contains(t, text, "call-1")
	require.Contains(t, text, "call-2")
	require.Contains(t, text, "boom")
}

// A3' (P1-1 of the R-ARB-2 review): a no-turn Drain whose commit was refused
// over visible debt (paced/stuck gate, hold, suspension, chain, cap, foreign
// driver, unreadable input) is no evidence about the gate: the pacing and
// both streaks survive untouched, and the session re-enters the recheck set
// exactly when the refusing verdict was worth a tick (the old default
// branch).
//
// Revert-check: mapping a refused commit with a non-empty snapshot onto
// "no debt: gate reset" (the pre-P1-1 translation) opens the gate, zeroes
// the streaks and drops the recheck entry -- every assertion goes red.
func TestDrainAttempt_CommitRefusalKeepsTheGateAndRechecks(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-commit-refused", attemptFixtureOpts{noIdle: true})
	f.seedDebt(ctx, "call-1", true) // visible debt: the commit was refused over it
	snap, err := f.store.CaptureDebtSnapshot(ctx, f.sessID)
	require.NoError(t, err)
	require.False(t, snap.Empty())

	require.False(t, f.ledger.paceDrainGate(f.sessID, 0, time.Hour, false, pacePaidUnreacted))

	att := &drainAttempt{
		sessionID: f.sessID, snapshot: snap, outcome: drainNoTurn,
		commitNo: drainVerdict{kind: drainPaced, recheck: true, reason: "retry pause after an unreacted attempt"},
	}
	f.coord.accountDrainAttempt(ctx, att, nil)

	g := f.coord.arb.snapshot(f.sessID).Gate
	require.True(t, g.Paced, "the gate stays shut")
	require.True(t, g.RetryAt.After(time.Now()), "the pause is not lifted")
	require.Equal(t, 1, g.PaidStreak, "the streak is not reset")
	require.True(t, f.inRecheckSet(), "recheck per commitNo.recheck")

	// A refusal that is not worth a tick adds no recheck entry.
	f.coord.recheckMu.Lock()
	delete(f.coord.recheckSet, f.sessID)
	f.coord.recheckMu.Unlock()
	att = &drainAttempt{
		sessionID: f.sessID, snapshot: snap, outcome: drainNoTurn,
		commitNo: drainVerdict{kind: drainDeferred, reason: "automatic turns suspended"},
	}
	f.coord.accountDrainAttempt(ctx, att, nil)
	require.False(t, f.inRecheckSet())
	g = f.coord.arb.snapshot(f.sessID).Gate
	require.True(t, g.Paced, "the gate is still untouched")
}
