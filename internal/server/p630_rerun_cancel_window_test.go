package server

// Task #630, reshaped by the atomic-Rerun design (docs/reviews/2026-09-29-
// async-phase4-round1-rerun-design.md): a Rerun commits exactly once.
//
// The tail, the target and the async-job bookkeeping are removed by ONE
// transaction (session.TruncateForRerun), so a Cancel has exactly one place
// to be honoured -- the last cancel point right before it, where nothing has
// changed yet -- and from the commit on the handler is committed: it stops
// the voided jobs and hands off into the replacement turn whatever the
// hold's cancellation state. There is no "tail half-deleted" state and no
// "target deleted separately, later" window any more.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Compile-time guarantee that the fake satisfies agent.Coordinator.
var _ agent.Coordinator = (*cancellableHoldCoordinator)(nil)

// cancellableHoldCoordinator wraps mailboxLikeCoordinator so that the
// holdCtx handed to handleRerunMessage is genuinely cancellable and
// Cancel/CancelTurn actually cancel it — mirroring the real coordinator,
// where a cancel reaches the reservation's holdCancel via the mailbox
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
	m.mailboxLikeCoordinator.Cancel(sessionID)
	m.cancelHold()
}

func (m *cancellableHoldCoordinator) cancelHold() {
	m.mu.Lock()
	cancel := m.holdCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *cancellableHoldCoordinator) CancelTurn(sessionID string) {
	m.mailboxLikeCoordinator.CancelTurn(sessionID)
	m.cancelHold()
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

// rerunHandlerFx is a real App/DB session with a target user message, a tail
// assistant message carrying async tool call "call-rerun", and the durable
// job row announced by a "started" tool-result message in the tail.
type rerunHandlerFx struct {
	a         *appPkg.App
	sessionID string
	target    message.Message
	tail      message.Message
	started   message.Message
	store     *session.AsyncJobStore
}

const rerunCallID = "call-rerun"

func newRerunHandlerFx(t *testing.T, title string) *rerunHandlerFx {
	t.Helper()
	return newRerunHandlerFxIn(t, t.TempDir(), title)
}

// newRerunHandlerFxIn is newRerunHandlerFx over an app whose working
// directory the caller picks, so a test can run the rerun handler inside a
// real git checkout (a non-empty workspace root; #1142 step C).
func newRerunHandlerFxIn(t *testing.T, workingDir, title string) *rerunHandlerFx {
	t.Helper()
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, workingDir, t.TempDir())
	ctx := t.Context()
	sess, err := a.Sessions.Create(ctx, title)
	require.NoError(t, err)
	f := &rerunHandlerFx{a: a, sessionID: sess.ID}

	f.target, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)
	f.tail, err = a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "old reply"},
			message.ToolCall{ID: rerunCallID, Name: "bash", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)
	f.tail.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, f.tail))

	f.store = session.NewAsyncJobStore(a.DB(), t.TempDir(), os.Getpid(), "rerun-handler-test")
	t.Cleanup(func() { _ = f.store.Close(context.Background()) })
	_, err = f.store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: rerunCallID, Kind: session.JobKindCommand, Input: "sleep 100", ToolName: "bash"})
	require.NoError(t, err)
	f.started, err = f.store.AnnounceStarted(ctx, a.Messages, sess.ID, rerunCallID, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: rerunCallID, Name: "bash", Content: "started"}},
	})
	require.NoError(t, err)
	return f
}

// run drives handleRerunMessage to completion and returns its single reply.
func (f *rerunHandlerFx) run(t *testing.T) WSMessage {
	t.Helper()
	client := newClient(newHub(), nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: f.target.ID})
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleRerunMessage(t.Context(), f.a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rerun handler never returned — something blocked unboundedly")
	}
	return decodeReply(t, client)
}

func (f *rerunHandlerFx) exists(t *testing.T, id string) bool {
	t.Helper()
	_, err := f.a.Messages.Get(t.Context(), id)
	return err == nil
}

func (f *rerunHandlerFx) jobDelivery(t *testing.T) string {
	t.Helper()
	row, err := f.store.Get(t.Context(), f.sessionID, rerunCallID)
	require.NoError(t, err)
	return row.Delivery
}

func (m *mailboxLikeCoordinator) counters() (cancelTurn, stop, stopRerun int, voided []session.VoidedAsyncJob, owned bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelTurnCalls, m.stopCalls, m.stopRerunCalls, append([]session.VoidedAsyncJob(nil), m.stopRerunVoided...), m.owned
}

// TestHandleRerunMessage_CancelAtLastPoint_NothingChanged: a Cancel landing
// while the handler holds the reservation but BEFORE the truncation
// transaction is honoured there and changes NOTHING: the tail, the target
// and the job row are intact and no job is stopped.
//
// Revert-check: run the truncation transaction before the last cancel check
// -- the tail is gone and the job voided when the handler reports "cancelled".
func TestHandleRerunMessage_CancelAtLastPoint_NothingChanged(t *testing.T) {
	f := newRerunHandlerFx(t, "cancel-at-last-point")
	mockCoord := &cancellableHoldCoordinator{}
	f.a.AgentCoordinator = mockCoord
	rerunHoldingReservationSeam = func() { mockCoord.Cancel(f.sessionID) }
	t.Cleanup(func() { rerunHoldingReservationSeam = nil })

	env := f.run(t)

	require.Equal(t, EventError, env.Type)
	require.Contains(t, env.Error, "cancelled")
	require.True(t, f.exists(t, f.target.ID), "target must be intact when Cancel is honoured at the last point")
	require.True(t, f.exists(t, f.tail.ID), "tail must be intact")
	require.True(t, f.exists(t, f.started.ID), "the job's announce message must be intact")
	require.Equal(t, "none", f.jobDelivery(t), "the job must not be voided by a cancelled rerun")
	_, _, stopRerun, _, owned := mockCoord.counters()
	require.Zero(t, stopRerun, "no job may be stopped by a cancelled rerun")
	require.False(t, owned, "the reservation must be released")
}

// TestHandleRerunMessage_CancelAfterCommit_Proceeds: a Cancel arriving after
// the truncation committed (the user's own message is gone) must NOT abort
// the handler: it stops the voided jobs and hands off into the replacement
// turn. Never "target deleted, no rerun".
//
// Revert-check: reinstate a holdCtx.Err() early-return between the commit
// and the handoff -- the reply is EventError "cancelled" and no run starts.
func TestHandleRerunMessage_CancelAfterCommit_Proceeds(t *testing.T) {
	f := newRerunHandlerFx(t, "cancel-after-commit")
	mockCoord := &cancellableHoldCoordinator{}
	f.a.AgentCoordinator = mockCoord
	runStarted := make(chan struct{})
	mockCoord.runSideEffect = func() { close(runStarted) }
	rerunPostTruncateSeam = func() { mockCoord.Cancel(f.sessionID) }
	t.Cleanup(func() { rerunPostTruncateSeam = nil })

	env := f.run(t)

	require.Equal(t, EventResponse, env.Type, "past the commit point the handler must reply ok, not 'cancelled'")
	select {
	case <-runStarted:
	default:
		t.Fatal("the truncation committed but no replacement turn started")
	}
	require.False(t, f.exists(t, f.target.ID), "the target is deleted by the committed rerun")
	require.False(t, f.exists(t, f.tail.ID))
	require.Equal(t, "void", f.jobDelivery(t))
	mockCoord.waitStopRerun(t, 1)
	_, _, stopRerun, voided, _ := mockCoord.counters()
	require.Equal(t, 1, stopRerun, "the voided job must be stopped after the commit")
	require.Len(t, voided, 1)
	require.Equal(t, rerunCallID, voided[0].ToolCallID)
}

// TestHandleRerunMessage_TruncateFailure_ErrorNoStopNoRun: a failing
// truncation transaction (rolled back by construction) replies with an
// error, stops no job, starts no turn, leaves history and the job row as
// they were, and releases the reservation so a retry can proceed.
//
// Revert-check: log the error and continue into the stop/handoff -- the
// handler replies ok, stops the job and runs a turn over an untouched history.
func TestHandleRerunMessage_TruncateFailure_ErrorNoStopNoRun(t *testing.T) {
	f := newRerunHandlerFx(t, "truncate-failure")
	mockCoord := &cancellableHoldCoordinator{}
	f.a.AgentCoordinator = mockCoord
	ran := false
	mockCoord.runSideEffect = func() { ran = true }
	prev := rerunTruncate
	rerunTruncate = func(context.Context, *sql.DB, message.Service, session.RerunTruncateParams) (session.RerunTruncation, error) {
		return session.RerunTruncation{}, errors.New("database is locked")
	}
	t.Cleanup(func() { rerunTruncate = prev })

	env := f.run(t)

	require.Equal(t, EventError, env.Type)
	require.Contains(t, env.Error, "nothing was changed")
	require.Contains(t, env.Error, "database is locked")
	require.True(t, f.exists(t, f.target.ID))
	require.True(t, f.exists(t, f.tail.ID))
	require.Equal(t, "none", f.jobDelivery(t))
	_, _, stopRerun, _, owned := mockCoord.counters()
	require.Zero(t, stopRerun, "a failed truncation must stop no job")
	require.False(t, ran, "a failed truncation must not start a turn")
	require.False(t, owned, "the reservation must be released so a retry can proceed")
}

// TestHandleRerunMessage_TargetGoneAtTruncation_RefusesAndChangesNothing: the
// target vanishes between the handler's listing and the transaction (a
// concurrent rerun or delete): the real transaction refuses, the tail and
// the job stay.
//
// Revert-check: drop the target-present check in TruncateForRerun.
func TestHandleRerunMessage_TargetGoneAtTruncation_RefusesAndChangesNothing(t *testing.T) {
	f := newRerunHandlerFx(t, "target-gone")
	mockCoord := &cancellableHoldCoordinator{}
	f.a.AgentCoordinator = mockCoord
	rerunHoldingReservationSeam = func() {
		require.NoError(t, f.a.Messages.Delete(t.Context(), f.target.ID))
	}
	t.Cleanup(func() { rerunHoldingReservationSeam = nil })

	env := f.run(t)

	require.Equal(t, EventError, env.Type)
	require.Contains(t, env.Error, "target message not found")
	require.True(t, f.exists(t, f.tail.ID), "the tail must survive a refused rerun")
	require.Equal(t, "none", f.jobDelivery(t))
	_, _, stopRerun, _, _ := mockCoord.counters()
	require.Zero(t, stopRerun)
}

// TestHandleRerunMessage_UsesCancelTurnNotStop: the rerun cancels only the
// live generation (CancelTurn), never the full Stop (Cancel).
//
// Revert-check: call Cancel instead of CancelTurn in the handler.
func TestHandleRerunMessage_UsesCancelTurnNotStop(t *testing.T) {
	f := newRerunHandlerFx(t, "uses-cancel-turn")
	mockCoord := &cancellableHoldCoordinator{}
	f.a.AgentCoordinator = mockCoord

	env := f.run(t)

	require.Equal(t, EventResponse, env.Type)
	mockCoord.waitStopRerun(t, 1)
	cancelTurn, stop, stopRerun, _, _ := mockCoord.counters()
	require.GreaterOrEqual(t, cancelTurn, 1, "rerun must cancel the live generation")
	require.Zero(t, stop, "rerun must not run the full Stop")
	require.Equal(t, 1, stopRerun)
}

// waitStopRerun waits for the rerun's detached StopRerunJobs call(s): the
// handler no longer runs it inline.
func (m *mailboxLikeCoordinator) waitStopRerun(t *testing.T, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.stopRerunCalls >= want
	}, 5*time.Second, 5*time.Millisecond, "the detached stop of the voided jobs must run")
}
