package agent

// Task #614 (F-4): ReserveExclusive returns a cancellable holdCtx.
//
// Before this fix, ReserveExclusive created a derived context via
// context.WithCancel(ctx) but discarded it (assigned to `_`), only returning
// the CancelFunc. Cancel(sessionID) during the hold would invoke the cancel
// func, but the holder had no way to observe the cancellation.
//
// The fix changes ReserveExclusive to return the derived context as the first
// return value (holdCtx), so the holder can select on it and abort its held
// work when Cancel(sessionID) lands.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// mockModel is a simple mock for testing
type mockModel struct{}

func (m *mockModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "test response"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (m *mockModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	// Return a simple streaming response that yields one finish part.
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}, nil
}

func (m *mockModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mockModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (mockModel) Provider() string { return "test" }
func (mockModel) Model() string    { return "mock" }

// TestReserveExclusive_HoldCtxCancellable verifies that Cancel(sessionID)
// during a hold cancels the returned holdCtx, allowing the holder to observe
// and abort.
//
// Sequence:
//  1. Call ReserveExclusive to get holdCtx.
//  2. Call Cancel(sessionID) on the same session.
//  3. Assert that holdCtx.Done() closes within a bounded timeout.
//
// Revert-check: make ReserveExclusive return context.Background() as
// holdCtx. Done never closes and the test times out with a clear message.
func TestReserveExclusive_HoldCtxCancellable(t *testing.T) {
	env := testEnv(t)
	agent := NewSessionAgent(SessionAgentOptions{
		SmartModel:   Model{Model: &mockModel{}, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:    Model{Model: &mockModel{}, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt: "test",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	sessionID := "test-hold-ctx-cancellable"
	ctx := context.Background()

	// Reserve exclusive ownership.
	holdCtx, epoch, cancel, ok := agent.ReserveExclusive(ctx, sessionID)
	require.True(t, ok, "ReserveExclusive must succeed")
	require.NotNil(t, holdCtx, "holdCtx must not be nil")
	require.NotEqual(t, context.Background(), holdCtx, "holdCtx must not be context.Background()")
	defer cancel()

	// Verify the session is now busy.
	require.True(t, agent.IsSessionBusy(sessionID), "session must be busy after reserve")

	// Cancel the session — this should cancel holdCtx.
	agent.Cancel(sessionID)

	// Assert that holdCtx.Done() closes within a reasonable timeout.
	select {
	case <-holdCtx.Done():
		// Success: holdCtx was cancelled as expected.
		require.ErrorIs(t, holdCtx.Err(), context.Canceled,
			"holdCtx.Err() must be context.Canceled")
	case <-time.After(5 * time.Second):
		t.Fatal("REVERT-CHECK FAILED: holdCtx.Done() did not close within timeout. " +
			"This means holdCtx is not being cancelled by Cancel(sessionID). " +
			"Expected failure text with the bug present: 'timed out waiting for holdCtx.Done()'")
	}

	// Release the reservation (even though the era is already cancelled,
	// ReleaseExclusive is safe to call).
	agent.ReleaseExclusive(sessionID, epoch, cancel)

	// Verify the session is no longer busy.
	require.False(t, agent.IsSessionBusy(sessionID), "session must not be busy after release")
}

// TestReserveExclusive_HoldCtxUsedInHeldWork verifies that a holder can
// use holdCtx for its held work and abort when Cancel(sessionID) lands.
//
// This is a more realistic test than the simple Done() check above: it
// simulates handleRerunMessage's pattern of using holdCtx for message
// operations and bailing out when cancelled.
func TestReserveExclusive_HoldCtxUsedInHeldWork(t *testing.T) {
	env := testEnv(t)
	agent := NewSessionAgent(SessionAgentOptions{
		SmartModel:   Model{Model: &mockModel{}, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:    Model{Model: &mockModel{}, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt: "test",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "test-held-work")
	require.NoError(t, err)
	sessionID := sess.ID

	// Create some test messages.
	_, err = env.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "message 1"}},
	})
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "message 2"}},
	})
	require.NoError(t, err)

	// Reserve exclusive ownership.
	holdCtx, epoch, cancel, ok := agent.ReserveExclusive(ctx, sessionID)
	require.True(t, ok, "ReserveExclusive must succeed")
	defer cancel()

	// Simulate held work that uses holdCtx and checks for cancellation.
	// This mirrors handleRerunMessage's pattern.
	var cancelled atomic.Bool
	var wg sync.WaitGroup
	startedWorking := make(chan struct{})
	proceedWithWork := make(chan struct{})
	wg.Add(1)

	go func() {
		defer wg.Done()

		// Signal that we've started
		close(startedWorking)

		// Wait for the main thread to tell us to proceed with work
		<-proceedWithWork

		// Simulate work that checks holdCtx after each operation.
		select {
		case <-holdCtx.Done():
			cancelled.Store(true)
			return
		default:
		}

		// Do some work (list messages using holdCtx).
		_, err := env.messages.List(holdCtx, sessionID)
		if err != nil && holdCtx.Err() != nil {
			cancelled.Store(true)
			return
		}

		// Check again.
		select {
		case <-holdCtx.Done():
			cancelled.Store(true)
			return
		default:
		}
	}()

	// Wait for the goroutine to start.
	<-startedWorking

	// Cancel is synchronous (it invokes the mailbox's stored CancelFunc
	// directly), so no propagation wait is needed before releasing the
	// worker goroutine.
	agent.Cancel(sessionID)

	// Tell the goroutine to proceed with work (holdCtx is now cancelled).
	close(proceedWithWork)

	// Wait for the goroutine to finish.
	wg.Wait()

	// Assert that the held work observed the cancellation.
	require.True(t, cancelled.Load(), "held work must have observed cancellation via holdCtx")

	// Clean up.
	agent.ReleaseExclusive(sessionID, epoch, cancel)
}
