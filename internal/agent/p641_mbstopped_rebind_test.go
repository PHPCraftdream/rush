package agent

// Task #641 (F-4): the mb.stopped branch of RunWithReservedOwnership's
// rebindDispatcher failure does not release the reservation — and that is
// NOT a protection against queued work being restarted during teardown.
// This test pins the TRUE behavior end to end: CancelAll latches stopped
// inside the admission->rebind window (via testReserveRebindSeam), the
// rebind fails, RunWithReservedOwnership returns its error, and the
// production caller's releaseOnBailout defer (mirrored here via
// ReleaseExclusive) then pops the queued call and DURABLY ENQUEUES it —
// on a hard-stopped mailbox, with the epoch still matching. Revert-check:
// this test fails if abandonOwnershipAndPopSubmitted ever grows an
// mb.stopped refusal (the enqueue would not happen), and it also fails if
// the caller-side release is removed from handleRerunMessage's contract
// reasoning (the mailbox would stay wedged at mbOwned-stopped).

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// countingModel reuses mockModel (p623_hold_ctx_test.go) but counts
// provider invocations, proving no turn ever starts on this path.
type countingModel struct {
	mockModel
	calls atomic.Int64
}

func (m *countingModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.calls.Add(1)
	return m.mockModel.Generate(ctx, call)
}

func (m *countingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	return m.mockModel.Stream(ctx, call)
}

// enqueueRecordingSessions records EnqueueRunQueueEntry calls and
// delegates to the real service, mirroring the wrapper pattern in
// p2_2_outbox_fallback_context_test.go.
type enqueueRecordingSessions struct {
	session.Service
	mu    sync.Mutex
	calls []session.SessionAgentCallData
	keys  []string
}

func (r *enqueueRecordingSessions) EnqueueRunQueueEntry(ctx context.Context, idempotencyKey, sessionID string, callData []byte) error {
	var data session.SessionAgentCallData
	_ = json.Unmarshal(callData, &data)
	r.mu.Lock()
	r.calls = append(r.calls, data)
	r.keys = append(r.keys, idempotencyKey)
	r.mu.Unlock()
	return r.Service.EnqueueRunQueueEntry(ctx, idempotencyKey, sessionID, callData)
}

func TestRunWithReservedOwnership_MbStoppedInRebindWindow_CallerReleaseStillEnqueuesQueuedWork(t *testing.T) {
	env := testEnv(t)
	rec := &enqueueRecordingSessions{Service: env.sessions}
	lm := &countingModel{}
	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:     Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:      Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt:   "test",
		IsYolo:         true,
		Sessions:       rec,
		Messages:       env.messages,
		CancelAllGrace: 200 * time.Millisecond,
	})
	sa := agentIface.(*sessionAgent)

	ctx := context.Background()
	sess, err := env.sessions.Create(ctx, "p641 mbstopped rebind")
	require.NoError(t, err)
	sessionID := sess.ID

	_, epoch, reserveCancel, ok := sa.ReserveExclusive(ctx, sessionID)
	require.True(t, ok)

	mb := sa.getMailbox(sessionID)
	sa.testReserveRebindSeam = func() {
		// Real CancelAll on a goroutine: it will latch stopped, fire the
		// placeholder dispatcher cancel, then block on runWg (our entry is
		// held) until the 200ms grace expires — by which point this call
		// has long since returned and the rebind below has failed.
		go sa.CancelAll()
		// Deterministically wait for the sweep to reach THIS mailbox.
		require.Eventually(t, mb.isStopped, 5*time.Second, 2*time.Millisecond)
		// Queue work AFTER the latch, exactly as a racing QueueMessage
		// would during teardown.
		mb.queue(SessionAgentCall{
			SessionID:       sessionID,
			Prompt:          "queued during teardown",
			LogicalCallID:   "p641-queued",
			MaxOutputTokens: 1000,
		})
	}

	_, err = sa.RunWithReservedOwnership(ctx, SessionAgentCall{
		SessionID:       sessionID,
		Prompt:          "rerun",
		MaxOutputTokens: 1000,
	}, epoch, reserveCancel, nil)
	require.Error(t, err, "rebind must fail: the mailbox was hard-stopped inside the admission->rebind window")

	// The mailbox is still mbOwned here — nothing released yet.
	mb.mu.Lock()
	state, queued := mb.state, len(mb.submitted)
	mb.mu.Unlock()
	require.Equal(t, mbOwned, state, "RunWithReservedOwnership's stopped branch must NOT release; that is the caller's job")
	require.Equal(t, 1, queued, "the queued call must still be in submitted")

	// Mirror handleRerunMessage's releaseOnBailout defer exactly: it fires
	// on this error with the same (still-matching) epoch.
	sa.ReleaseExclusive(sessionID, epoch, reserveCancel)

	// THE #641 F-4 assertions: hard-stop never bumped the epoch, so this
	// release is NOT a no-op even though mb.stopped is latched — the
	// queued call is popped and durably enqueued.
	rec.mu.Lock()
	keys := append([]string(nil), rec.keys...)
	rec.mu.Unlock()
	require.Len(t, keys, 1, "the caller-side release must durably enqueue the queued call despite the stopped latch")
	require.Contains(t, keys[0], "p641-queued")

	require.False(t, sa.IsSessionBusy(sessionID), "mailbox must be mbIdle after the caller-side release")
	require.Zero(t, lm.calls.Load(), "no provider turn may start on this path — only a durable enqueue")
}
