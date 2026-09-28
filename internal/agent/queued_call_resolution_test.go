package agent

// P1 hang found in orchestrator review of async phase 2: a call queued
// behind a busy mailbox with onQueueResolved set (coordinator.
// runAwaitingAdmission, #1036) could be silently durably-enqueued (or
// dropped by ClearQueue / an overwritten interrupt-and-replace) instead of
// resolved, leaving its waiter blocked forever -- no implicit timeout exists
// anywhere in this chain. See queued_call_resolution.go.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// blockingErrorModel is a fantasy.LanguageModel whose Stream call blocks
// until released, then returns a plain (non-cancel) error. Gives a test a
// controlled window during which a session's mailbox is genuinely owned by
// an in-flight turn, before that turn fails.
//
// fantasy's own Agent.Stream retries a provider error internally with
// exponential backoff, so Stream can be called more than once per turn --
// startOnce guards the started signal so only the FIRST attempt fires it;
// release, once closed, reads as immediately-ready on every subsequent
// attempt for free (close(chan) semantics), so it needs no such guard.
type blockingErrorModel struct {
	started   chan struct{}
	startOnce sync.Once
	release   chan struct{}
	err       error
}

func newBlockingErrorModel(err error) *blockingErrorModel {
	return &blockingErrorModel{started: make(chan struct{}), release: make(chan struct{}), err: err}
}

func (m *blockingErrorModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, m.err
}

func (m *blockingErrorModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.startOnce.Do(func() { close(m.started) })
	<-m.release
	return nil, m.err
}

func (m *blockingErrorModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, m.err
}

func (m *blockingErrorModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, m.err
}

func (*blockingErrorModel) Provider() string { return "test" }
func (*blockingErrorModel) Model() string    { return "blocking-error" }

// TestRunAwaitingAdmission_OrphanedQueuedCallResolvesInsteadOfHanging
// reproduces the hang: call1 becomes the mailbox owner and blocks mid-turn;
// call2 is submitted via runAwaitingAdmission while call1 is still busy, so
// it queues into mb.submitted with onQueueResolved set; call1 then fails
// with a non-cancel provider error, triggering runOwned's deferred
// abandonOwnershipWithHandoff -> abandonOwnershipAndPopSubmitted ->
// restartOrphanedWithRetry with call2 still queued. Without the fix, call2's
// hook is lost to durable-enqueue serialization (onQueueResolved is
// json:"-") and runAwaitingAdmission blocks until ctx2's own deadline.
//
// Revert-check performed: reverted restartOrphanedWithRetry's first line to
// skip splitCallsWithoutWaiters (durably enqueue everything, as before this
// fix) -- this test FAILED: the "runAwaitingAdmission for call2 never
// returned" branch fired (it blocked past the 9s bound, only settling at
// ctx2's own 10s timeout with context.DeadlineExceeded, not
// errQueuedTurnNotRun). Restored the fix; re-ran, passed with call2
// resolving in well under a second of call1 failing.
func TestRunAwaitingAdmission_OrphanedQueuedCallResolvesInsteadOfHanging(t *testing.T) {
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "orphan hook test")
	require.NoError(t, err)

	failErr := errors.New("boom: provider failed")
	model := newBlockingErrorModel(failErr)
	agentIface := testSessionAgent(env, model, erroringModel{}, "test system prompt")

	call1Done := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "call1",
			MaxOutputTokens: 100,
			LogicalCallID:   "call1",
		})
		call1Done <- runErr
	}()

	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("call1's turn never started")
	}

	coord := &coordinator{}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel2()
	type outcome struct {
		err    error
		queued bool
	}
	call2Done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		_, admitErr, queued := coord.runAwaitingAdmission(ctx2, agentIface, SessionAgentCall{
			SessionID:       sess.ID,
			Prompt:          "call2",
			MaxOutputTokens: 100,
			LogicalCallID:   "call2",
		})
		call2Done <- outcome{admitErr, queued}
	}()

	// call2's own Run attempt is a fast, lock-only mailbox append -- no need
	// to wait on call1's own progress for it to land in mb.submitted.
	require.Eventually(t, func() bool {
		return agentIface.QueuedPrompts(sess.ID) > 0
	}, 2*time.Second, 10*time.Millisecond, "call2 must be queued behind the busy mailbox")

	close(model.release) // let call1's turn fail with a non-cancel error

	select {
	case <-call1Done:
	case <-time.After(5 * time.Second):
		t.Fatal("call1's Run never returned")
	}

	select {
	case o := <-call2Done:
		require.Less(t, time.Since(start), 5*time.Second,
			"runAwaitingAdmission must resolve promptly once call1 fails, not block until ctx2's own deadline")
		require.Error(t, o.err)
		require.ErrorIs(t, o.err, errQueuedTurnNotRun)
		require.True(t, o.queued)
	case <-time.After(9 * time.Second):
		t.Fatal("runAwaitingAdmission for call2 never returned -- the orphaned call's hook was dropped")
	}
}

// TestMailbox_ClearAll_ResolvesQueuedCallsWithHooks pins the ClearQueue path
// named in the orchestrator's review: a call sitting in mb.submitted with
// onQueueResolved set must be resolved with errQueuedTurnNotRun, not merely
// dropped, when the queue is explicitly cleared.
//
// Revert-check performed: reverted clearAll to the pre-fix body (`mb.mu.Lock();
// defer mb.mu.Unlock(); mb.submitted = nil; mb.replacement = nil; mb.injects =
// nil`) -- this test FAILED (resolved channel stayed empty). Restored the
// fix; re-ran, passed.
func TestMailbox_ClearAll_ResolvesQueuedCallsWithHooks(t *testing.T) {
	mb := &mailbox{}
	owner := SessionAgentCall{SessionID: "s1", Prompt: "owner"}
	becomeOwner, _ := mb.submit(owner, func() {})
	require.True(t, becomeOwner)

	resolved := make(chan error, 1)
	queued := SessionAgentCall{SessionID: "s1", Prompt: "queued", onQueueResolved: func(_ *fantasy.AgentResult, e error) {
		resolved <- e
	}}
	becomeOwner2, _ := mb.submit(queued, func() {})
	require.False(t, becomeOwner2)
	require.Len(t, mb.submitted, 1)

	mb.clearAll()

	select {
	case err := <-resolved:
		require.Error(t, err)
		require.ErrorIs(t, err, errQueuedTurnNotRun)
	default:
		t.Fatal("clearAll must resolve a dropped call's onQueueResolved hook")
	}
	require.Empty(t, mb.submitted)
}

// TestMailbox_InterruptAndReplace_ResolvesOverwrittenReplacementHook pins
// the third path named in the orchestrator's review: a replacement carrying
// onQueueResolved that gets overwritten by a newer interrupt-and-replace,
// before it ever ran, must be resolved with errQueuedTurnNotRun rather than
// silently discarded via the bare pointer overwrite.
//
// Revert-check performed: reverted interruptAndReplaceLocked to the pre-fix
// body (`if !call.FromDurableQueue { mb.replacement = &call }`, no resolve
// call) -- this test FAILED (resolvedA channel stayed empty). Restored the
// fix; re-ran, passed.
func TestMailbox_InterruptAndReplace_ResolvesOverwrittenReplacementHook(t *testing.T) {
	mb := &mailbox{}
	owner := SessionAgentCall{SessionID: "s1", Prompt: "owner"}
	becomeOwner, _ := mb.submit(owner, func() {})
	require.True(t, becomeOwner)

	resolvedA := make(chan error, 1)
	callA := SessionAgentCall{SessionID: "s1", Prompt: "replacement A", onQueueResolved: func(_ *fantasy.AgentResult, e error) {
		resolvedA <- e
	}}
	_, hadOwner := mb.interruptAndReplace(callA)
	require.True(t, hadOwner)
	require.NotNil(t, mb.replacement)

	resolvedB := make(chan error, 1)
	callB := SessionAgentCall{SessionID: "s1", Prompt: "replacement B", onQueueResolved: func(_ *fantasy.AgentResult, e error) {
		resolvedB <- e
	}}
	_, hadOwner2 := mb.interruptAndReplace(callB)
	require.True(t, hadOwner2)

	select {
	case err := <-resolvedA:
		require.Error(t, err)
		require.ErrorIs(t, err, errQueuedTurnNotRun)
	default:
		t.Fatal("interruptAndReplace must resolve the OLD replacement's hook when overwriting it")
	}
	select {
	case <-resolvedB:
		t.Fatal("the NEW replacement's hook must not be resolved yet -- it hasn't run or been dropped")
	default:
	}
	require.Equal(t, callB.Prompt, mb.replacement.Prompt)
}
