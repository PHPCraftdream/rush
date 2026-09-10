package agent

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// fakeUINotifyMsgSvc implements message.Service via a nil embedded
// interface, overriding only Notify -- the sole method turnUINotifier
// calls.
type fakeUINotifyMsgSvc struct {
	message.Service
	mu       sync.Mutex
	notified []message.Message
}

func (m *fakeUINotifyMsgSvc) Notify(msg message.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notified = append(m.notified, msg)
}

func (m *fakeUINotifyMsgSvc) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.notified)
}

func (m *fakeUINotifyMsgSvc) last() *message.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &m.notified[len(m.notified)-1]
}

// No existing test exercised the streaming UI-notify ticker end-to-end
// before this split -- it used to be an anonymous goroutine plus an
// anonymous closure inside runTurn. This closes that gap the same way the
// checkpoint and peak-hours splits did.
func TestTurnUINotifierPublishesLatestSnapshotAtTickRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msgSvc := &fakeUINotifyMsgSvc{}
		a := &sessionAgent{messages: msgSvc}
		var sessionLock sync.Mutex
		currentAssistant := &message.Message{ID: "msg-1", SessionID: "sess-1"}

		genCtx, genCancel := context.WithCancel(t.Context())
		defer genCancel()

		n := newTurnUINotifier(a, genCtx, &sessionLock, &currentAssistant)
		n.start()

		// Two notify() calls before the ticker ever fires: latest-value
		// semantics must mean the ticker publishes only the SECOND one.
		sessionLock.Lock()
		currentAssistant.AppendContent("first")
		sessionLock.Unlock()
		require.NoError(t, n.notify())

		sessionLock.Lock()
		currentAssistant.AppendContent(" second")
		sessionLock.Unlock()
		require.NoError(t, n.notify())

		// Sleep past the 50ms tick inside the synctest bubble: the fake
		// clock advances and lets the ticker goroutine's own pending fire
		// happen before this returns.
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()

		require.Equal(t, 1, msgSvc.count(), "the ticker must coalesce two pending notifies into one publish")
		require.Equal(t, "first second", msgSvc.last().FullText(), "the published snapshot must be the LATEST one, not the first")
	})
}

func TestTurnUINotifierFlushesPendingSnapshotOnGenCtxDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msgSvc := &fakeUINotifyMsgSvc{}
		a := &sessionAgent{messages: msgSvc}
		var sessionLock sync.Mutex
		currentAssistant := &message.Message{ID: "msg-1", SessionID: "sess-1"}
		currentAssistant.AppendContent("final")

		genCtx, genCancel := context.WithCancel(t.Context())

		n := newTurnUINotifier(a, genCtx, &sessionLock, &currentAssistant)
		n.start()
		require.NoError(t, n.notify())

		// Cancel BEFORE the 50ms ticker would otherwise fire, and before
		// calling synctest.Wait -- exercises the genCtx.Done() flush path,
		// not the ordinary ticker path.
		genCancel()
		synctest.Wait()

		require.Equal(t, 1, msgSvc.count(), "genCtx cancellation must flush exactly the one pending snapshot")
		require.Equal(t, "final", msgSvc.last().FullText())
	})
}

func TestTurnUINotifierDrainPendingSuppressesStalePublish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msgSvc := &fakeUINotifyMsgSvc{}
		a := &sessionAgent{messages: msgSvc}
		var sessionLock sync.Mutex
		currentAssistant := &message.Message{ID: "msg-1", SessionID: "sess-1"}
		currentAssistant.AppendContent("stale")

		genCtx, genCancel := context.WithCancel(t.Context())
		defer genCancel()

		n := newTurnUINotifier(a, genCtx, &sessionLock, &currentAssistant)
		n.start()
		require.NoError(t, n.notify())

		// drainPending must remove the queued snapshot before the ticker
		// gets a chance to publish it -- this is what runTurn calls right
		// before its own authoritative Update of the final state, so the
		// ticker cannot race a stale snapshot in behind it.
		n.drainPending()
		synctest.Wait()

		require.Equal(t, 0, msgSvc.count(), "a drained snapshot must never reach messages.Notify")
	})
}

func TestTurnUINotifierNotifyIsNoopWithNilAssistant(t *testing.T) {
	msgSvc := &fakeUINotifyMsgSvc{}
	a := &sessionAgent{messages: msgSvc}
	var sessionLock sync.Mutex
	var currentAssistant *message.Message // nil: no assistant message created yet

	n := newTurnUINotifier(a, context.Background(), &sessionLock, &currentAssistant)
	require.NoError(t, n.notify(), "notify must not panic or error before PrepareStep has set an assistant message")

	select {
	case <-n.latestMsgCh:
		t.Fatal("notify must not enqueue anything when the assistant pointer is nil")
	default:
	}
}
