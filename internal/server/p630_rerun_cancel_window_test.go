package server

// Task #630: bounding rerun cancellation to phase boundaries.
//
// Commit d4e64288 made the exclusive reservation hold interruptible:
// ReserveExclusive returns holdCtx and handleRerunMessage checks
// holdCtx.Err() between phases. But the tail-deletion loop ran its Delete
// calls under holdCtx itself, so a Cancel landing BETWEEN two tail
// deletions cancelled the remaining Delete calls and the post-loop check
// bailed with "cancelled" — leaving the session's history PARTIALLY
// truncated with no rerun in exchange. Before d4e64288 a Cancel at that
// moment did nothing; after it, the user could lose part of their
// transcript and get nothing back.
//
// The fix: the pre-loop phase check honours a Cancel BEFORE the loop (tail
// untouched), and the loop plus step 3's target delete run under
// context.WithoutCancel(holdCtx) so they complete once entered. The
// invariant: the tail is either untouched or fully deleted — never half.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Compile-time guarantee that the fake satisfies agent.Coordinator.
var _ agent.Coordinator = (*cancellableHoldCoordinator)(nil)

// cancellableHoldCoordinator wraps mailboxLikeCoordinator so that the
// holdCtx handed to handleRerunMessage is genuinely cancellable and
// Cancel(sessionID) actually cancels it — mirroring the real coordinator,
// where Cancel reaches the reservation's holdCancel via the mailbox
// (sessionAgent.Cancel -> mb.current.cancel, populated by beginCompact for
// a ReserveExclusive hold). The p614 fake returned the caller's ctx
// unchanged with a no-op cancel, which cannot express "user pressed Cancel
// mid-rerun". All shared-field access uses the embedded mutex so the
// -race detector sees one lock protecting owned/epoch/holdCancel.
type cancellableHoldCoordinator struct {
	mailboxLikeCoordinator
	holdCancel context.CancelFunc
}

func (m *cancellableHoldCoordinator) ReserveExclusive(ctx context.Context, sessionID string) (context.Context, uint64, context.CancelFunc, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owned {
		return nil, 0, nil, false
	}
	m.owned = true
	m.epochSeq++
	m.epoch = m.epochSeq
	holdCtx, cancel := context.WithCancel(ctx)
	m.holdCancel = cancel
	return holdCtx, m.epoch, cancel, true
}

func (m *cancellableHoldCoordinator) Cancel(sessionID string) {
	m.mu.Lock()
	cancel := m.holdCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *cancellableHoldCoordinator) ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holdCancel = nil
	if m.epoch != epoch {
		return
	}
	m.owned = false
}

// TestHandleRerunMessage_CancelBetweenTailDeletesLeavesTailAllOrNothing is
// THE mechanism test for task #630: a Cancel landing precisely between the
// first and second tail deletions must not leave the tail half-deleted.
//
// Sequence:
//  1. handleRerunMessage runs on a goroutine with two finished tail
//     messages and claims the reservation.
//  2. rerunTailDeleteSeam fires at i==1 — i.e. strictly AFTER the first
//     tail Delete succeeded and strictly BEFORE the second — and the test
//     cancels the hold there, exactly like a user pressing Cancel in that
//     window.
//  3. Assertions: the reply is an error containing "cancelled" (no rerun
//     runs — cancellation is still honoured), and BOTH tail messages are
//     gone (the loop completed once entered).
//
// Revert-check: change the loop's Delete/ForceDelete back to holdCtx (i.e.
// undo the context.WithoutCancel) and this test fails: the second tail
// message survives while the first is gone — the half-truncated state.
func TestHandleRerunMessage_CancelBetweenTailDeletesLeavesTailAllOrNothing(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-630-cancel-mid-tail")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	var tailIDs []string
	for _, text := range []string{"old reply 1", "old reply 2"} {
		m, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: text}},
		})
		require.NoError(t, err)
		m.AddFinish(message.FinishReasonEndTurn, "", "")
		require.NoError(t, a.Messages.Update(ctx, m))
		tailIDs = append(tailIDs, m.ID)
	}

	mockCoord := &cancellableHoldCoordinator{}
	a.AgentCoordinator = mockCoord

	// Cancel precisely between tail delete #0 (done) and tail delete #1
	// (not yet attempted): the seam fires at the top of iteration 1.
	cancelled := make(chan struct{})
	rerunTailDeleteSeam = func(i int) {
		if i == 1 {
			mockCoord.Cancel(sessionID)
			close(cancelled)
		}
	}
	t.Cleanup(func() { rerunTailDeleteSeam = nil })

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rerun handler never returned — something blocked unboundedly")
	}

	select {
	case <-cancelled:
	default:
		t.Fatal("seam never fired at i==1; the test did not exercise the between-deletes window")
	}

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type,
		"a mid-loop Cancel must still abort the rerun — the handler must not proceed to the replacement turn")
	require.Contains(t, env.Error, "cancelled")

	// The decisive assertion: the tail is FULLY deleted, never half. Both
	// tail messages must be gone even though the hold was cancelled between
	// their deletions.
	for idx, id := range tailIDs {
		_, getErr := a.Messages.Get(ctx, id)
		require.Error(t, getErr,
			"tail message %d must be deleted: the loop must finish once entered, even if Cancel lands mid-loop (got half-truncated tail)", idx)
	}

	// The target user message must NOT have been deleted: the post-loop
	// phase check bailed before step 3.
	stillTarget, getErr := a.Messages.Get(ctx, userMsg.ID)
	require.NoError(t, getErr, "target user message must survive when Cancel is honoured at the post-loop phase boundary")
	require.Equal(t, "rerun me", stillTarget.Content().Text)
}

// TestHandleRerunMessage_CancelBeforeTailLoopLeavesTailUntouched covers the
// other half of the invariant: a Cancel arriving while the handler holds
// the reservation but BEFORE the loop is entered must be honoured at that
// phase boundary — tail wholly intact, target intact, error reply
// "cancelled".
//
// Revert-check: delete the pre-loop holdCtx.Err() check and this test
// fails: the loop then runs (uncancellable) and deletes the whole tail
// before the post-loop check notices the Cancel, so the tail assertions
// below find the messages gone.
func TestHandleRerunMessage_CancelBeforeTailLoopLeavesTailUntouched(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-630-cancel-pre-loop")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	mockCoord := &cancellableHoldCoordinator{}
	a.AgentCoordinator = mockCoord

	// Cancel while the handler is parked holding the reservation, strictly
	// before the tail loop.
	rerunHoldingReservationSeam = func() {
		mockCoord.Cancel(sessionID)
	}
	t.Cleanup(func() { rerunHoldingReservationSeam = nil })

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rerun handler never returned — something blocked unboundedly")
	}

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type)
	require.Contains(t, env.Error, "cancelled")

	_, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.NoError(t, getErr, "tail message must be wholly intact when Cancel is honoured before the loop")
	_, getErr = a.Messages.Get(ctx, userMsg.ID)
	require.NoError(t, getErr, "target user message must be intact when Cancel is honoured before the loop")
}

// (end of file)

// TestHandleRerunMessage_CancelDuringTargetDeleteCommitsToRerun covers the
// commit point (task #630 follow-up). Step 3 deletes the user's own target
// message; only the replacement turn's Run() recreates it. So a Cancel
// landing during step 3 must NOT be honoured with an early return — that
// would produce "target deleted, no rerun": the user's own prompt gone
// with nothing recreating it. The handler must proceed to the handoff.
//
// Revert-check: reinstate a holdCtx.Err() early-return between step 3 and
// the handoff (the pre-follow-up code shape) and this test fails: the
// reply is EventError "cancelled" while the target message is gone.
func TestHandleRerunMessage_CancelDuringTargetDeleteCommitsToRerun(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-630-cancel-during-target-delete")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	mockCoord := &cancellableHoldCoordinator{}
	a.AgentCoordinator = mockCoord

	// Record that the replacement turn actually started.
	runStarted := make(chan struct{})
	mockCoord.runSideEffect = func() { close(runStarted) }

	// Cancel precisely during step 3: after the last honoured check, after
	// the tail loop, immediately before the target delete.
	rerunPreTargetDeleteSeam = func() {
		mockCoord.Cancel(sessionID)
	}
	t.Cleanup(func() { rerunPreTargetDeleteSeam = nil })

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rerun handler never returned — something blocked unboundedly")
	}

	// The decisive assertion, both halves: the target WAS deleted (step 3
	// completed) AND the rerun proceeded (handoff reached). Never "target
	// deleted, no rerun".
	_, getErr := a.Messages.Get(ctx, userMsg.ID)
	require.Error(t, getErr,
		"step 3 must complete even when cancelled mid-delete — the target is deleted exactly once here")

	select {
	case <-runStarted:
	default:
		t.Fatal("target message was deleted but no replacement turn started — the handler returned with the user's own prompt gone and nothing recreating it")
	}

	env := decodeReply(t, client)
	require.Equal(t, EventResponse, env.Type,
		"past the commit point the handler must reply ok (rerun proceeded), not 'cancelled'")

	_, getErr = a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, getErr, "tail message must be deleted by the committed rerun")
}
