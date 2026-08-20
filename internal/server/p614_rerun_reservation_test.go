package server

// Task #614 (F5 of docs/reviews/2026-08-20_07-28-07-readonly-release-review-a70d1b73.md).
//
// handleRerunMessage used to cancel the old turn, poll IsSessionBusy until
// idle, and then delete the message tail based ONLY on that snapshot -- no
// reservation, no lock, nothing that actually prevented a concurrent
// Send/Rerun from becoming the new owner and writing a fresh streaming
// assistant message into the session WHILE the tail-delete loop was running.
// If that race landed, Delete would return ErrMessageStillStreaming for the
// new message, and the orphan-rescue branch (added for the legitimate
// crashed/killed-turn case) would force-delete it anyway, because the rescue
// only re-checks IsSessionBusy -- which the new turn had already flipped back
// to true, but the tail loop had no way to notice mid-iteration.
//
// This file proves the fix: handleRerunMessage now claims exclusive
// ownership (AgentCoordinator.ReserveExclusive) BEFORE touching history, and
// a concurrent reservation attempt while that hold is live must fail closed.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// mailboxLikeCoordinator is a minimal agent.Coordinator fake whose
// ReserveExclusive/ReleaseExclusive are backed by a real mutex-guarded
// boolean, so this test exercises a genuinely atomic
// "check-and-claim-or-fail" primitive -- not just a canned return value --
// the same shape guarantee the real mailbox.beginCompact-based
// implementation provides. runSideEffect, when set, is invoked synchronously
// inside RunWithReservedOwnership so a test can observe exactly when
// (relative to the reservation window) the replacement turn ran.
type mailboxLikeCoordinator struct {
	mu          sync.Mutex
	owned       bool
	epochSeq    uint64
	epoch       uint64
	reserveHits int // counts every ReserveExclusive call, successful or not.

	runSideEffect func()
}

func (m *mailboxLikeCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (m *mailboxLikeCoordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (m *mailboxLikeCoordinator) Cancel(sessionID string) {}

func (m *mailboxLikeCoordinator) CancelAll() (stillBusy bool) { return false }

func (m *mailboxLikeCoordinator) IsSessionBusy(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owned
}

func (m *mailboxLikeCoordinator) IsBusy() bool { return m.IsSessionBusy("") }

func (m *mailboxLikeCoordinator) QueuedPrompts(sessionID string) int          { return 0 }
func (m *mailboxLikeCoordinator) QueuedPromptsList(sessionID string) []string { return nil }
func (m *mailboxLikeCoordinator) ClearQueue(sessionID string)                 {}

func (m *mailboxLikeCoordinator) InterruptAndSend(ctx context.Context, sessionID, prompt string, smart, fast *agent.ModelOverride, attachments ...message.Attachment) error {
	return nil
}

func (m *mailboxLikeCoordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	return message.Message{}, nil
}

func (m *mailboxLikeCoordinator) Summarize(context.Context, string, *agent.SummarizeSnapshot) error {
	return nil
}
func (m *mailboxLikeCoordinator) SummarizeQueued(sessionID string) bool { return false }
func (m *mailboxLikeCoordinator) TakeSummarizeQueue(sessionID string) (*agent.SummarizeSnapshot, bool) {
	return nil, false
}
func (m *mailboxLikeCoordinator) CancelQueuedSummarize(sessionID string) {}

func (m *mailboxLikeCoordinator) Model() agent.Model                     { return agent.Model{} }
func (m *mailboxLikeCoordinator) UpdateModels(ctx context.Context) error { return nil }
func (m *mailboxLikeCoordinator) GetSystemPrompt() string                { return "" }
func (m *mailboxLikeCoordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	return "", nil
}
func (m *mailboxLikeCoordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}
func (m *mailboxLikeCoordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return nil
}
func (m *mailboxLikeCoordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
}
func (m *mailboxLikeCoordinator) SetRunLimits(maxCost float64, maxTokens int64)    {}
func (m *mailboxLikeCoordinator) SetActiveModelRole(role config.SelectedModelType) {}
func (m *mailboxLikeCoordinator) SetAllowPeakHours(allow bool)                     {}
func (m *mailboxLikeCoordinator) SetPersistentMode(persistent bool)                {}
func (m *mailboxLikeCoordinator) ResetAutoResumeCounter(sessionID string)          {}

func (m *mailboxLikeCoordinator) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	return agent.SessionAgentCall{}, nil
}
func (m *mailboxLikeCoordinator) RunSessionAgentCall(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

// ReserveExclusive is the atomic check-and-claim this whole test exists to
// exercise: it holds m.mu for the ENTIRE check-then-set, exactly mirroring
// mailbox.beginCompact's single critical section.
func (m *mailboxLikeCoordinator) ReserveExclusive(ctx context.Context, sessionID string) (epoch uint64, cancel context.CancelFunc, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserveHits++
	if m.owned {
		return 0, nil, false
	}
	m.owned = true
	m.epochSeq++
	m.epoch = m.epochSeq
	return m.epoch, func() {}, true
}

func (m *mailboxLikeCoordinator) ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.epoch != epoch {
		return
	}
	m.owned = false
}

func (m *mailboxLikeCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if m.runSideEffect != nil {
		m.runSideEffect()
	}
	// Continuing the SAME reservation into a turn: release it now that the
	// (fake) turn is "done", mirroring runOwned's own abandonOwnershipWithHandoff.
	m.ReleaseExclusive(sessionID, epoch, cancel)
	return nil, nil
}

// TestHandleRerunMessage_ConcurrentNewTurnCannotRaceReservation is the
// regression test for task #614 / F5: a rerun that is paused (via the
// rerunPostIdlePollSeam test seam) strictly AFTER observing the session idle
// but strictly BEFORE claiming exclusive ownership must not let a concurrent
// "new turn" reservation attempt land in that window and then get bulldozed.
//
// Sequence:
//  1. handleRerunMessage runs on a goroutine; it observes idle, then blocks
//     at rerunPostIdlePollSeam.
//  2. The test simulates a concurrent new Send/Rerun landing in exactly that
//     window by calling ReserveExclusive directly and, if it wins, holding
//     ownership and marking the target/tail messages as "protected" by NOT
//     releasing until after asserting they are untouched.
//  3. The rerun goroutine is unblocked. Its own ReserveExclusive call MUST
//     fail (the concurrent caller already owns the session), so
//     handleRerunMessage must reply with an error and must NOT have deleted
//     the target or tail messages.
//
// Revert-check: comment out the ReserveExclusive call (and its failure
// branch) in handlers_agent.go, restoring the old "just delete" behavior,
// and this test fails: the target and tail messages are gone even though the
// simulated concurrent turn still owned the session throughout.
func TestHandleRerunMessage_ConcurrentNewTurnCannotRaceReservation(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	// newAttachmentsTestApp (not the lighter newMessageEditTestApp) is
	// required here: handleRerunMessage's step 5 calls a.Sessions.Get, and
	// newMessageEditTestApp only populates a.Messages.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-rerun-reservation")
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

	mockCoord := &mailboxLikeCoordinator{}
	a.AgentCoordinator = mockCoord

	// Arm the seam: block the rerun handler right after it observes idle,
	// signal readiness, and wait for the test to release it.
	reachedSeam := make(chan struct{})
	releaseSeam := make(chan struct{})
	rerunPostIdlePollSeam = func() {
		close(reachedSeam)
		<-releaseSeam
	}
	t.Cleanup(func() { rerunPostIdlePollSeam = nil })

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

	// Wait until the rerun goroutine is parked at the seam (idle observed,
	// ownership not yet claimed).
	select {
	case <-reachedSeam:
	case <-time.After(5 * time.Second):
		t.Fatal("rerun handler never reached the post-idle-poll seam")
	}

	// Simulate a concurrent new turn winning the reservation race in exactly
	// this window.
	epoch, cancel, ok := mockCoord.ReserveExclusive(ctx, sessionID)
	require.True(t, ok, "the concurrent caller must win the reservation while the rerun handler is parked pre-reservation")

	// Now let the rerun handler proceed. Its own ReserveExclusive call must
	// observe the session busy and fail closed.
	close(releaseSeam)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rerun handler never returned after being unblocked")
	}

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type,
		"rerun must fail closed when a concurrent caller already holds the reservation")
	require.Contains(t, env.Error, "busy",
		"the error should explain the session became busy again, not report success")

	// The decisive assertion: neither the target user message nor the tail
	// assistant message was touched while the concurrent "new turn" held
	// ownership.
	stillTarget, getErr := a.Messages.Get(ctx, userMsg.ID)
	require.NoError(t, getErr, "target user message must NOT have been deleted")
	require.Equal(t, "rerun me", stillTarget.Content().Text)

	stillTail, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.NoError(t, getErr, "tail assistant message must NOT have been deleted")
	require.Equal(t, "old reply", stillTail.Content().Text)

	// Exactly one ReserveExclusive call came from the rerun handler itself
	// (the concurrent one above was made directly by the test, not through
	// the handler), and it must have been rejected.
	require.GreaterOrEqual(t, mockCoord.reserveHits, 2,
		"expected both the test's concurrent reservation and the handler's own attempt")

	// Release the simulated concurrent turn's hold now that assertions are done.
	mockCoord.ReleaseExclusive(sessionID, epoch, cancel)
}

// TestHandleRerunMessage_HeldReservationBlocksConcurrentSend is the OTHER
// direction of the F5 race -- the one the review flagged as the direction
// that actually matters: a NEW turn starting AFTER handleRerunMessage has
// already become the owner, while its tail-delete loop is still running.
// Before this fix, that window had no reservation at all guarding it: a
// concurrent Send/Rerun could become owner mid-deletion and start writing a
// fresh streaming assistant message that the tail loop (built from an
// earlier snapshot, walking real DB state) could then race against, up to
// and including the orphan-rescue branch force-deleting it.
//
// Sequence:
//  1. handleRerunMessage runs on a goroutine; it claims the reservation
//     (ReserveExclusive succeeds) and then blocks at
//     rerunHoldingReservationSeam, strictly BEFORE the tail-delete loop.
//  2. The test simulates a concurrent Send/Rerun landing in exactly that
//     window by calling ReserveExclusive itself. It MUST fail (ok=false),
//     proving the concurrent caller cannot become owner and therefore
//     cannot create a new streaming message while this handler holds
//     ownership.
//  3. The rerun goroutine is unblocked and completes its (fake) tail
//     delete + handoff normally.
//
// Revert-check: comment out the ReserveExclusive call (and its failure
// branch) in handlers_agent.go, restoring the old "just delete" behavior,
// and this test fails: the concurrent ReserveExclusive call now succeeds
// (ok=true) because nothing claimed the mailbox before the tail-delete
// loop ran, exactly reproducing the pre-fix hole.
func TestHandleRerunMessage_HeldReservationBlocksConcurrentSend(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-rerun-holds-reservation")
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

	mockCoord := &mailboxLikeCoordinator{}
	a.AgentCoordinator = mockCoord

	// Arm the seam: block the rerun handler right after it claims the
	// reservation, before it starts deleting the tail.
	reachedSeam := make(chan struct{})
	releaseSeam := make(chan struct{})
	rerunHoldingReservationSeam = func() {
		close(reachedSeam)
		<-releaseSeam
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

	// Wait until the rerun goroutine is parked at the seam: it has already
	// claimed ReserveExclusive and is about to delete the tail.
	select {
	case <-reachedSeam:
	case <-time.After(5 * time.Second):
		t.Fatal("rerun handler never reached the holding-reservation seam")
	}

	// Simulate a concurrent new Send/Rerun landing in exactly this window.
	// It must NOT be able to become owner -- proving no new streaming
	// message could be created while this handler holds ownership.
	_, _, ok := mockCoord.ReserveExclusive(ctx, sessionID)
	require.False(t, ok,
		"a concurrent Send/Rerun must NOT be able to claim the reservation while handleRerunMessage still holds it")

	// Let the rerun handler finish its (fake) tail delete + handoff.
	close(releaseSeam)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rerun handler never returned after being unblocked")
	}

	env := decodeReply(t, client)
	require.Equal(t, EventResponse, env.Type,
		"rerun itself should complete normally once it is the sole owner throughout")

	// The tail message must be gone (a normal, uncontested tail delete --
	// the fake coordinator's RunWithReservedOwnership never touches
	// a.Messages, so any deletion here came from the handler's own step 2/3).
	_, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, getErr, "tail message should have been deleted by the uncontested rerun")

	_, getErr = a.Messages.Get(ctx, userMsg.ID)
	require.Error(t, getErr, "target user message should have been deleted by the uncontested rerun")

	// After the handler completes (RunWithReservedOwnership released via the
	// fake's own ReleaseExclusive call), the session must be free again: a
	// FOLLOW-UP reservation attempt must succeed.
	_, followUpCancel, followUpOk := mockCoord.ReserveExclusive(ctx, sessionID)
	require.True(t, followUpOk, "session must be free again once the rerun handler has fully completed")
	mockCoord.ReleaseExclusive(sessionID, mockCoord.epoch, followUpCancel)
}

// TestRunWithReservedOwnership_ModelResolutionFailureReleasesReservation is
// the regression test for task #614 defect 1: every early return in
// coordinator.RunWithReservedOwnership / sessionAgent.RunWithReservedOwnership
// that happens BEFORE the single handoff line into runOwned must release the
// reservation it was handed, or the session wedges at "busy" forever on a
// perfectly ordinary error -- not just a shutdown race.
//
// This test drives the REAL agent.Coordinator (not a fake), using a session
// whose configured model cannot be resolved, so
// coordinator.RunWithReservedOwnership fails inside its model-resolution
// step -- exactly the branch the findings review identified as reachable on
// a routine error, not just process shutdown.
//
// Revert-check: remove the `c.currentAgent.ReleaseExclusive(...)` call from
// the model-resolution failure branch in coordinator_run.go's
// RunWithReservedOwnership, and this test fails: the follow-up
// ReserveExclusive call times out/fails because the session is permanently
// stuck at mbOwned.
func TestRunWithReservedOwnership_ModelResolutionFailureReleasesReservation(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	require.NotNil(t, a.AgentCoordinator, "test app must build a real coordinator")

	sess, err := a.Sessions.Create(t.Context(), "test-reservation-release-on-model-error")
	require.NoError(t, err)

	ctx := t.Context()

	// Claim the reservation exactly like handleRerunMessage's step 1a does.
	epoch, cancel, ok := a.AgentCoordinator.ReserveExclusive(ctx, sess.ID)
	require.True(t, ok, "precondition: reservation must succeed on an idle session")

	// Force model resolution to fail by pointing this session at a
	// nonexistent provider/model pair via an explicit override -- this
	// reaches applyModelOverrides inside RunWithReservedOwnership and fails
	// there, which is the exact branch task #614 defect 1 identified as
	// unreachable-by-shutdown but reachable-by-routine-error.
	badOverride := &agent.ModelOverride{Provider: "does-not-exist", Model: "does-not-exist"}
	_, runErr := a.AgentCoordinator.RunWithReservedOwnership(ctx, sess.ID, "hello", epoch, cancel, badOverride, nil)
	require.Error(t, runErr, "precondition: an unresolvable model override must fail RunWithReservedOwnership")

	// The decisive assertion: the session must be FREE again, not wedged at
	// mbOwned. A follow-up ReserveExclusive must succeed.
	followUpDone := make(chan bool, 1)
	go func() {
		_, followUpCancel, followUpOk := a.AgentCoordinator.ReserveExclusive(ctx, sess.ID)
		if followUpOk {
			a.AgentCoordinator.ReleaseExclusive(sess.ID, 0, followUpCancel)
		}
		followUpDone <- followUpOk
	}()

	select {
	case ok := <-followUpDone:
		require.True(t, ok, "session must be free again after RunWithReservedOwnership failed at model resolution -- it must not stay wedged at mbOwned")
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up ReserveExclusive call hung -- session is wedged busy")
	}
}
