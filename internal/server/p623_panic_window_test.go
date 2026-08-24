package server

// Task #614 (F-2): handleRerunMessage panic window fix.
//
// Before this fix, handleRerunMessage set releaseOnBailout = false BEFORE
// calling RunWithReservedOwnership. If a panic occurred anywhere between that
// line and runOwned's deferred abandonOwnershipWithHandoff installing (e.g.
// in hub.Broadcast, in the coordinator wrapper, or in SessionAgent.
// RunWithReservedOwnership's pre-handoff body), the handler's bail-out defer
// was disarmed and nobody released the mailbox — it stayed mbOwned until
// restart.
//
// The fix adds an onHandoff callback that is invoked exactly at the handoff
// point in agent-level RunWithReservedOwnership. The handler passes
// func() { releaseOnBailout = false } as onHandoff, so the defer stays armed
// during the entire pre-handoff window (including Broadcast, coordinator work,
// and agent pre-handoff body) and is only disarmed when onHandoff fires,
// which is immediately before runOwned's defer takes over.
//
// This test adds a test-only seam rerunPreHandoffSeam that fires right where
// step 6 begins (before Broadcast). The test sets the seam to panic, drives
// handleRerunMessage via the existing server test harness, recovers, and then
// asserts the session is not busy (IsSessionBusy false or a fresh
// ReserveExclusive succeeds).

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

// TestHandleRerunMessage_PanicBeforeHandoffReleasesReservation verifies that
// a panic occurring before the onHandoff callback fires results in the
// reservation being released by the handler's defer (not stuck mbOwned).
//
// Sequence:
//  1. Set rerunPreHandoffSeam to panic.
//  2. Drive handleRerunMessage via the test harness.
//  3. The handler panics at the seam (before Broadcast, i.e. before
//     RunWithReservedOwnership is called, so onHandoff never fires).
//  4. runRecovered in hub.go recovers the panic.
//  5. The handler's defer (releaseOnBailout still true) releases via
//     ReleaseExclusive.
//  6. Assert the session is not busy (fresh ReserveExclusive succeeds).
//
// Revert-check: restore the old pattern (set releaseOnBailout = false before
// the call and pass nil onHandoff). The session stays busy and the test fails.
func TestHandleRerunMessage_PanicBeforeHandoffReleasesReservation(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-panic-window")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "panic test"}},
	})
	require.NoError(t, err)

	// Tail message to delete (the rerun target is the user message above).
	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	// Mock coordinator that tracks ownership for assertions.
	mockCoord := &panicWindowCoordinator{coordinator: a.AgentCoordinator}
	a.AgentCoordinator = mockCoord

	// Arm the seam to panic before handoff.
	reachedSeam := make(chan struct{})
	panicValue := "boom - panic before handoff"
	rerunPreHandoffSeam = func() {
		close(reachedSeam)
		panic(panicValue)
	}
	t.Cleanup(func() { rerunPreHandoffSeam = nil })

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Recover from panic like runRecovered does.
		defer func() {
			recover() // expected panic in test - continue normally
		}()
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	// Wait until the handler reaches the seam and panics.
	select {
	case <-reachedSeam:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never reached the pre-handoff seam")
	}

	// Wait for the handler goroutine to finish (recover should have
	// recovered and returned).
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never returned after panic")
	}

	// The decisive assertion: the session must NOT be busy — the handler's
	// defer should have released the reservation even though it panicked.
	require.False(t, a.AgentCoordinator.IsSessionBusy(sessionID),
		"session must not be busy after panic before handoff")

	// Verify by attempting a fresh ReserveExclusive — it should succeed.
	_, _, _, ok := a.AgentCoordinator.ReserveExclusive(ctx, sessionID)
	require.True(t, ok, "fresh ReserveExclusive must succeed after panic before handoff")
	require.True(t, mockCoord.released, "ReleaseExclusive must have been called by the defer")
}

// TestHandleRerunMessage_PanicBeforeHandoffRecreatesPrompt (task #645,
// twelfth-review N-2): the #638/#644 prompt-recreate check used to live
// inside `if err != nil`, which does not run when the function PANICS past
// step 3's delete. A panic at rerunPreHandoffSeam left the session with
// zero messages and the operator's prompt unrecoverable. The fix registers
// a defer after the commit-point delete, so the prompt is recreated even
// on panic unwind.
//
// Revert-check: make the defer a no-op (delete it) — the prompt-survival
// assertion below goes red while the reservation-release assertions above
// stay green.
func TestHandleRerunMessage_PanicBeforeHandoffRecreatesPrompt(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-panic-recreate")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	promptText := "recreate me after panic"
	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: promptText}},
	})
	require.NoError(t, err)

	rerunPreHandoffSeam = func() { panic("boom - panic before handoff") }
	t.Cleanup(func() { rerunPreHandoffSeam = nil })

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			recover() // expected panic — mirrors hub.runRecovered.
		}()
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never returned after panic")
	}

	// The decisive assertion: the operator's prompt must have been
	// recreated by the post-commit-point defer during panic unwind.
	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == promptText {
			found = true
		}
	}
	require.True(t, found,
		"original prompt must survive a pre-handoff panic (recreated by the defer); got %d messages", len(msgs))
}

// panicWindowCoordinator wraps a real coordinator and tracks whether
// ReleaseExclusive was called.
type panicWindowCoordinator struct {
	mu          sync.Mutex
	coordinator agent.Coordinator
	released    bool
}

func (p *panicWindowCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return p.coordinator.Run(ctx, sessionID, prompt, attachments...)
}

func (p *panicWindowCoordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return p.coordinator.RunWithOverrides(ctx, sessionID, prompt, smart, fast, attachments...)
}

func (p *panicWindowCoordinator) Cancel(sessionID string) {
	p.coordinator.Cancel(sessionID)
}

func (p *panicWindowCoordinator) CancelAll() (stillBusy bool) {
	return p.coordinator.CancelAll()
}

func (p *panicWindowCoordinator) IsSessionBusy(sessionID string) bool {
	return p.coordinator.IsSessionBusy(sessionID)
}

func (p *panicWindowCoordinator) IsBusy() bool {
	return p.coordinator.IsBusy()
}

func (p *panicWindowCoordinator) QueuedPrompts(sessionID string) int {
	return p.coordinator.QueuedPrompts(sessionID)
}

func (p *panicWindowCoordinator) QueuedPromptsList(sessionID string) []string {
	return p.coordinator.QueuedPromptsList(sessionID)
}

func (p *panicWindowCoordinator) ClearQueue(sessionID string) {
	p.coordinator.ClearQueue(sessionID)
}

func (p *panicWindowCoordinator) InterruptAndSend(ctx context.Context, sessionID, prompt string, smart, fast *agent.ModelOverride, attachments ...message.Attachment) error {
	return p.coordinator.InterruptAndSend(ctx, sessionID, prompt, smart, fast, attachments...)
}

func (p *panicWindowCoordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	return p.coordinator.InjectMessage(ctx, sessionID, prompt, attachments...)
}

func (p *panicWindowCoordinator) Summarize(ctx context.Context, sessionID string, snapshot *agent.SummarizeSnapshot) error {
	return p.coordinator.Summarize(ctx, sessionID, snapshot)
}

func (p *panicWindowCoordinator) SummarizeQueued(sessionID string) bool {
	return p.coordinator.SummarizeQueued(sessionID)
}

func (p *panicWindowCoordinator) TakeSummarizeQueue(sessionID string) (*agent.SummarizeSnapshot, bool) {
	return p.coordinator.TakeSummarizeQueue(sessionID)
}

func (p *panicWindowCoordinator) CancelQueuedSummarize(sessionID string) {
	p.coordinator.CancelQueuedSummarize(sessionID)
}

func (p *panicWindowCoordinator) Model() agent.Model {
	return p.coordinator.Model()
}

func (p *panicWindowCoordinator) UpdateModels(ctx context.Context) error {
	return p.coordinator.UpdateModels(ctx)
}

func (p *panicWindowCoordinator) GetSystemPrompt() string {
	return p.coordinator.GetSystemPrompt()
}

func (p *panicWindowCoordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	return p.coordinator.BuildSystemPrompt(ctx)
}

func (p *panicWindowCoordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	return p.coordinator.BuildSystemPromptForSession(ctx, sessionID)
}

func (p *panicWindowCoordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return p.coordinator.UpdateSessionSystemPrompt(ctx, sessionID, prompt)
}

func (p *panicWindowCoordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
	p.coordinator.SetAgentTimeoutOptions(extendsOnProgress, hardCap)
}

func (p *panicWindowCoordinator) SetRunLimits(maxCost float64, maxTokens int64) {
	p.coordinator.SetRunLimits(maxCost, maxTokens)
}

func (p *panicWindowCoordinator) SetActiveModelRole(role config.SelectedModelType) {
	p.coordinator.SetActiveModelRole(role)
}

func (p *panicWindowCoordinator) SetAllowPeakHours(allow bool) {
	p.coordinator.SetAllowPeakHours(allow)
}

func (p *panicWindowCoordinator) SetPersistentMode(persistent bool) {
	p.coordinator.SetPersistentMode(persistent)
}

func (p *panicWindowCoordinator) ResetAutoResumeCounter(sessionID string) {
	p.coordinator.ResetAutoResumeCounter(sessionID)
}

func (p *panicWindowCoordinator) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	return p.coordinator.RebuildSessionAgentCall(ctx, data)
}

func (p *panicWindowCoordinator) RunSessionAgentCall(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	return p.coordinator.RunSessionAgentCall(ctx, call)
}

func (p *panicWindowCoordinator) ReserveExclusive(ctx context.Context, sessionID string) (context.Context, uint64, context.CancelFunc, bool) {
	return p.coordinator.ReserveExclusive(ctx, sessionID)
}

func (p *panicWindowCoordinator) ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.released = true
	p.coordinator.ReleaseExclusive(sessionID, epoch, cancel)
}

func (p *panicWindowCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return p.coordinator.RunWithReservedOwnership(ctx, sessionID, prompt, epoch, cancel, onHandoff, smart, fast, attachments...)
}
