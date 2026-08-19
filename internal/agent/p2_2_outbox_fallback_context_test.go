// Test for P2-2 (task #579): the orphan-outbox fallback write must get its
// OWN fresh context, not reuse the primary enqueue's already-possibly-
// exhausted one.
package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// deadlineCapturingSessions wraps a real session.Service, fails
// EnqueueRunQueueEntry unconditionally, and records the ctx.Done() channel
// each of EnqueueRunQueueEntry/WriteToOrphanOutbox actually observed --
// enough to prove (or disprove) that the fallback got a DIFFERENT,
// independently rooted context rather than the same one reused. Done()
// returns the identical channel only for the exact same context value, so
// comparing channel identity is exact and immune to any clock-resolution
// rounding a Deadline()-based (time.Time) comparison could suffer from
// when two independent context.WithTimeout(Background(), 30*time.Second)
// calls happen only microseconds apart in the same synchronous call path.
type deadlineCapturingSessions struct {
	session.Service
	enqueueCtxDone <-chan struct{}
	outboxCtxDone  <-chan struct{}
	outboxCtxErr   error
}

var errP2_2SimulatedEnqueueFailure = errors.New("p2-2 test: simulated durable enqueue failure")

func (d *deadlineCapturingSessions) EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error {
	d.enqueueCtxDone = ctx.Done()
	return errP2_2SimulatedEnqueueFailure
}

func (d *deadlineCapturingSessions) WriteToOrphanOutbox(ctx context.Context, id, sessionID string, callData []byte) error {
	d.outboxCtxDone = ctx.Done()
	d.outboxCtxErr = ctx.Err()
	// Delegate to the real service so the outbox row still gets written --
	// this test also doubles as proof the fallback still succeeds.
	return d.Service.WriteToOrphanOutbox(ctx, id, sessionID, callData)
}

// TestRestartOrphanedWithRetry_OutboxFallbackGetsOwnContext is the P2-2
// regression test. Before the fix, agent_ownership.go's restartOrphanedWithRetry
// passed the SAME enqueueCtx to both a.sessions.EnqueueRunQueueEntry and the
// a.sessions.WriteToOrphanOutbox fallback -- so if the primary call had
// exhausted enqueueCtx's 30s budget (a hung/locked DB, not a fast
// constraint/serialization error), the fallback inherited an
// already-expired context and got no attempt of its own. This proves the
// fallback now receives an INDEPENDENTLY rooted context (a distinct Done()
// channel from the primary's), and that its own context is not already in
// an error state when WriteToOrphanOutbox is entered.
//
// Revert-check: passing enqueueCtx to WriteToOrphanOutbox again (the
// pre-fix shape) makes this test fail -- see the task report for the
// verbatim failure output.
func TestRestartOrphanedWithRetry_OutboxFallbackGetsOwnContext(t *testing.T) {
	env := testEnv(t)
	model := newFastSSEModel(t, "p2-2 outbox fallback context test")

	tracking := &deadlineCapturingSessions{Service: env.sessions}

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel:   model,
		FastModel:    model,
		SystemPrompt: "test system prompt",
		Sessions:     tracking,
		Messages:     env.messages,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "p2-2-outbox-fallback-context-test")
	require.NoError(t, err)

	call := SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "test prompt",
		LogicalCallID:   "p2-2-logical-id",
		MaxOutputTokens: 100,
	}

	retryErr := sa.restartOrphanedWithRetry([]SessionAgentCall{call})
	require.Error(t, retryErr, "enqueue fails unconditionally, so restartOrphanedWithRetry must report an error")

	require.NotNil(t, tracking.enqueueCtxDone, "EnqueueRunQueueEntry must have been called with a context")
	require.NotNil(t, tracking.outboxCtxDone, "WriteToOrphanOutbox must have been called with a context")

	require.NotEqual(t, tracking.enqueueCtxDone, tracking.outboxCtxDone,
		"the outbox fallback must receive an INDEPENDENTLY rooted context, not the same enqueueCtx handed to the primary "+
			"EnqueueRunQueueEntry call -- Done() returns the identical channel only when it is the SAME context value, "+
			"so an equal Done() channel here proves the same context object was reused for both calls")

	require.NoError(t, tracking.outboxCtxErr,
		"the fallback's own context must not already be in an error state when WriteToOrphanOutbox is entered -- "+
			"a fresh context gives the fallback its own real budget instead of inheriting whatever state the primary call's context was left in")

	// The outbox row must still have actually been written, proving the
	// fallback context was usable (not merely present but already dead).
	pendingOutbox, err := env.sessions.ListPendingOrphanOutboxEntries(t.Context())
	require.NoError(t, err)
	require.Len(t, pendingOutbox, 1, "the fallback write must have succeeded using its own fresh context")
}
