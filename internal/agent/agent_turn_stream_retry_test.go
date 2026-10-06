// turnStream.onRetry is fantasy's only hook between a cut stream attempt's
// deltas and the retried attempt's deltas: fantasy re-runs the whole step on
// the SAME callbacks (agent.go's retry wraps Stream+processStepStream;
// PrepareStep and OnStepStart are not re-run), so nothing else can clear
// ts.currentAssistant. These tests pin what the hook must discard, observed
// through a real message service.
package agent

import (
	"context"
	"io"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// retryFixture builds the thinnest turnStream that drives the streaming
// callbacks and onRetry against a real message service (the same connection
// helper the tool-suggest fixture uses), so what onRetry discards is
// observed both in memory and in what is actually persisted.
type retryFixture struct {
	svc message.Service
	ts  *turnStream
}

func newRetryFixture(t *testing.T, sessionID string) *retryFixture {
	t.Helper()
	_, _, conn := newTestAsyncJobStoreWithDataDir(t)
	svc := message.NewService(db.New(conn))
	return &retryFixture{
		svc: svc,
		ts: &turnStream{
			a:               &sessionAgent{messages: svc},
			ctx:             t.Context(),
			genCtx:          t.Context(),
			call:            SessionAgentCall{SessionID: sessionID},
			bumpActivity:    func() {},
			startCheckpoint: func() {},
			notifyUI:        func() error { return nil },
		},
	}
}

// beginStep creates the step's assistant row the way prepareStep does and
// points ts.currentAssistant at it.
func (f *retryFixture) beginStep(t *testing.T) *message.Message {
	t.Helper()
	msg, err := f.svc.Create(context.Background(), f.ts.call.SessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{},
	})
	require.NoError(t, err)
	f.ts.currentAssistant = &msg
	return &msg
}

// codexCutError is the exact shape #1202 produces when a Codex body is cut
// mid-stream: a retryable ProviderError over io.ErrUnexpectedEOF.
func codexCutError() *fantasy.ProviderError {
	return &fantasy.ProviderError{
		Title:   "stream transport error",
		Message: io.ErrUnexpectedEOF.Error(),
		Cause:   io.ErrUnexpectedEOF,
	}
}

// TestStreamRetryDiscardsCutAttemptContent pins the retry duplication fix.
// Revert check: without onRetry's truncation this fails with the assistant
// text reading "partial the full answer" (the cut attempt's prefix riding
// under the retry's answer) and reasoning
// "cut-attempt thinkingmore thinkingretry thinking".
func TestStreamRetryDiscardsCutAttemptContent(t *testing.T) {
	f := newRetryFixture(t, "retry-cut")
	stepMsg := f.beginStep(t)

	require.NoError(t, f.ts.onReasoningStart("r1", fantasy.ReasoningContent{Text: "cut-attempt thinking"}))
	require.NoError(t, f.ts.onReasoningDelta("r1", "more thinking"))
	require.NoError(t, f.ts.onTextDelta("t1", "partial "))

	f.ts.onRetry(codexCutError(), 2*time.Second)

	// The truncation itself is persisted, not just applied in memory.
	persisted, err := f.svc.Get(context.Background(), stepMsg.ID)
	require.NoError(t, err)
	require.Empty(t, persisted.Content().Text)
	require.Empty(t, persisted.ReasoningContent().Thinking)

	// The retried attempt streams its own text/reasoning from the start.
	require.NoError(t, f.ts.onReasoningStart("r2", fantasy.ReasoningContent{Text: "retry thinking"}))
	require.NoError(t, f.ts.onTextDelta("t2", "the full answer"))

	require.Equal(t, "the full answer", f.ts.currentAssistant.FullText())
	require.Equal(t, "retry thinking", f.ts.currentAssistant.ReasoningContent().Thinking)
}

// TestTurnStreamDeltasAccumulateWithoutRetry is the control: with NO retry,
// text and reasoning deltas accumulate exactly as before the fix.
func TestTurnStreamDeltasAccumulateWithoutRetry(t *testing.T) {
	f := newRetryFixture(t, "retry-control")
	f.beginStep(t)

	require.NoError(t, f.ts.onReasoningStart("r1", fantasy.ReasoningContent{Text: "thinking"}))
	require.NoError(t, f.ts.onReasoningDelta("r1", " harder"))
	require.NoError(t, f.ts.onTextDelta("t1", "hello "))
	require.NoError(t, f.ts.onTextDelta("t1", "world"))

	require.Equal(t, "hello world", f.ts.currentAssistant.FullText())
	require.Equal(t, "thinking harder", f.ts.currentAssistant.ReasoningContent().Thinking)
}

// TestStreamRetrySecondStepKeepsEarlierStepContent: a retry inside the
// SECOND step must not touch the first step's already-persisted message.
func TestStreamRetrySecondStepKeepsEarlierStepContent(t *testing.T) {
	f := newRetryFixture(t, "retry-step2")

	// Step 1 composes and is persisted at its step boundary.
	step1 := f.beginStep(t)
	require.NoError(t, f.ts.onTextDelta("t1", "first step answer"))
	require.NoError(t, f.svc.Update(context.Background(), step1.Clone()))

	// Step 2 gets a fresh assistant row (prepareStep), is cut mid-body,
	// and the retried attempt answers on its own.
	step2 := f.beginStep(t)
	require.NoError(t, f.ts.onTextDelta("t2", "second step partial"))
	f.ts.onRetry(codexCutError(), 2*time.Second)
	require.NoError(t, f.ts.onTextDelta("t3", "second step final"))

	saved1, err := f.svc.Get(context.Background(), step1.ID)
	require.NoError(t, err)
	require.Equal(t, "first step answer", saved1.FullText())

	require.Equal(t, "second step final", f.ts.currentAssistant.FullText())
	saved2, err := f.svc.Get(context.Background(), step2.ID)
	require.NoError(t, err)
	require.Empty(t, saved2.Content().Text)
}
