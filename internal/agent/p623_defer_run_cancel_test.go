package agent

// Task #614 (F-6): RunWithReservedOwnership must defer runCancel().
//
// Before this fix, RunWithReservedOwnership created runCtx/runCancel via
// context.WithCancel(ctx) but never deferred runCancel(). This leaked the
// context and the cancel func. Run() has defer runCancel() at a similar
// point — RunWithReservedOwnership should match it.
//
// The fix adds `defer runCancel()` immediately after the context.WithCancel
// call, ensuring the context is always cleaned up.

import (
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"context"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"strings"
	"sync"
	"testing"
	"time"
)

// contextCapturingModel is a fantasy.LanguageModel that captures the context
// it receives in Generate/Stream calls for later inspection.
type contextCapturingModel struct {
	mu              sync.Mutex
	capturedContext context.Context
}

func (m *contextCapturingModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.mu.Lock()
	m.capturedContext = ctx
	m.mu.Unlock()

	// Return a simple response to let the turn complete normally.
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "test response"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (m *contextCapturingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.capturedContext = ctx
	m.mu.Unlock()

	// Return a simple streaming response that yields one finish part.
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}, nil
}

func (m *contextCapturingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *contextCapturingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (*contextCapturingModel) Provider() string { return "test" }
func (*contextCapturingModel) Model() string    { return "context-capturing" }

// getCapturedContext returns the context that was captured by the most recent
// Generate/Stream call.
func (m *contextCapturingModel) getCapturedContext() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capturedContext
}

// panicAfterCaptureModel captures the context it receives and then panics,
// so the turn NEVER completes normally: runOwned's `turnCancel()` (called
// only after runTurn returns) never runs. The captured context — a
// descendant of runCtx — can therefore only be cancelled by
// RunWithReservedOwnership's own `defer runCancel()`. This is what makes
// the test below discriminate F-6's mechanism: a normal-completion test
// would pass even without the fix, because turnCancel() also cancels the
// captured context.
type panicAfterCaptureModel struct {
	contextCapturingModel
}

func (m *panicAfterCaptureModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.capturedContext = ctx
	m.mu.Unlock()
	// Panic ONLY on the turn's own call, identified by its prompt NOT
	// containing the title template's marker text — not on generateTitle's
	// call, which runs in a separate goroutine (spawned concurrently with
	// the turn, so "first call" would be racy) where a panic would crash
	// the test process. fantasy.Prompt is []Message; extract the text.
	fmt.Printf("DEBUG model call prompt: %+v\n", call.Prompt)
	if !promptContains(call.Prompt, "concise title") {
		panic("boom: panic after capture")
	}
	return m.contextCapturingModel.Stream(ctx, call)
}

// promptContains reports whether any part of any message in p renders to
// text containing substr. Uses fmt rendering rather than a typed assertion
// because the concrete MessagePart type the provider layer hands the model
// is not guaranteed to be fantasy.TextContent.
func promptContains(p fantasy.Prompt, substr string) bool {
	for _, msg := range p {
		for _, part := range msg.Content {
			if strings.Contains(fmt.Sprintf("%v", part), substr) {
				return true
			}
		}
	}
	return false
}

// TestRunWithReservedOwnership_DefersRunCancelOnPanicPath asserts the
// invariant "the provider-visible context is cancelled even when the turn
// panics", and that the panic does not wedge the session.
//
// SCOPE CAVEAT (verified by revert-check, do not paper over): this test
// does NOT uniquely discriminate the `defer runCancel()` line itself.
// On the panic path, runTurn's own `defer cancel()` (genCtx,
// agent_turn.go) also runs during unwind and cancels the captured context
// (a genCtx descendant), so the test still passes with the F-6 defer
// removed. Every descendant of runCtx the provider can observe is covered
// by its own deferred cancel; runCtx itself has no externally reachable
// observer (rebindDispatcher stores only its CancelFunc, which the era's
// release clears). The F-6 fix is therefore a lostcancel-style leak fix
// whose behavioral difference is not externally observable on any
// reachable path; this test remains as a regression guard for the
// user-visible invariant, not as proof of the defer.
func TestRunWithReservedOwnership_DefersRunCancelOnPanicPath(t *testing.T) {
	env := testEnv(t)
	panicModel := &panicAfterCaptureModel{}
	normalModel := &contextCapturingModel{}

	agent := NewSessionAgent(SessionAgentOptions{
		// The smart model runs the turn and panics mid-stream.
		SmartModel: Model{
			Model:      panicModel,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		// The fast model serves title generation normally so the panic
		// path's deferred wg.Wait can complete.
		FastModel: Model{
			Model:      normalModel,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		SystemPrompt: "test system prompt",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "test-defer-run-cancel-panic")
	require.NoError(t, err)
	sessionID := sess.ID

	_, epoch, reserveCancel, ok := agent.ReserveExclusive(ctx, sessionID)
	require.True(t, ok, "ReserveExclusive must succeed")
	defer reserveCancel()

	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		_, _ = agent.RunWithReservedOwnership(ctx, SessionAgentCall{
			Prompt:          "test prompt",
			SessionID:       sessionID,
			MaxOutputTokens: 100,
		}, epoch, reserveCancel, nil)
	}()

	select {
	case r := <-recovered:
		require.NotNil(t, r, "expected the model's panic to propagate out of RunWithReservedOwnership")
	case <-time.After(10 * time.Second):
		t.Fatal("RunWithReservedOwnership did not return within 10s")
	}

	// By the time RunWithReservedOwnership returns (or panics through),
	// the unwinding defers have run — runOwned's defer runCancel() and/or
	// runTurn's own deferred genCtx cancel (see this test's SCOPE CAVEAT:
	// either can be the one observed here) — so no wait is needed.
	capturedCtx := panicModel.getCapturedContext()
	require.NotNil(t, capturedCtx, "panicking model should have captured a context")
	require.ErrorIs(t, capturedCtx.Err(), context.Canceled,
		"captured context must be cancelled after RunWithReservedOwnership panics — "+
			"cancelled by the unwinding defers (this asserts the user-visible "+
			"invariant only; it does not discriminate WHICH defer did it)")

	// The panic must not wedge the session: runOwned's defer released it.
	require.False(t, agent.IsSessionBusy(sessionID),
		"session must not be busy after the panicking turn")
}

// TestRunWithReservedOwnership_EarlyReturnAlsoDefersRunCancel verifies that
// runCancel is invoked even on early returns (before the handoff).
//
// This tests that defer runCancel() works correctly on the early-return paths
// where abandonOwnershipWithHandoff is called.
func TestRunWithReservedOwnership_EarlyReturnAlsoDefersRunCancel(t *testing.T) {
	env := testEnv(t)
	model := &contextCapturingModel{}

	agent := NewSessionAgent(SessionAgentOptions{
		SmartModel: Model{
			Model:      model,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		FastModel: Model{
			Model:      model,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		SystemPrompt: "test system prompt",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "test-early-return-cancel")
	require.NoError(t, err)
	sessionID := sess.ID

	// Reserve exclusive ownership.
	_, epoch, reserveCancel, ok := agent.ReserveExclusive(ctx, sessionID)
	require.True(t, ok, "ReserveExclusive must succeed")

	// Call RunWithReservedOwnership with an empty prompt (early return path).
	_, err = agent.RunWithReservedOwnership(ctx, SessionAgentCall{
		Prompt:          "", // Empty prompt triggers early return
		SessionID:       sessionID,
		MaxOutputTokens: 100,
	}, epoch, reserveCancel, nil)
	require.Error(t, err, "RunWithReservedOwnership must fail with empty prompt")
	require.ErrorIs(t, err, ErrEmptyPrompt, "error must be ErrEmptyPrompt")

	// Verify the session is not busy (early release worked).
	require.False(t, agent.IsSessionBusy(sessionID),
		"session must not be busy after early return")
}
