package turnstep

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	testguard.Main(m)
}

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
		var sessionLock sync.Mutex
		currentAssistant := &message.Message{ID: "msg-1", SessionID: "sess-1"}

		genCtx, genCancel := context.WithCancel(t.Context())
		defer genCancel()

		n := NewTurnUINotifier(genCtx, &sessionLock, &currentAssistant, func(msg message.Message) { msgSvc.Notify(msg) })
		n.Start()

		// Two notify() calls before the ticker ever fires: latest-value
		// semantics must mean the ticker publishes only the SECOND one.
		sessionLock.Lock()
		currentAssistant.AppendContent("first")
		sessionLock.Unlock()
		require.NoError(t, n.Notify())

		sessionLock.Lock()
		currentAssistant.AppendContent(" second")
		sessionLock.Unlock()
		require.NoError(t, n.Notify())

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
		var sessionLock sync.Mutex
		currentAssistant := &message.Message{ID: "msg-1", SessionID: "sess-1"}
		currentAssistant.AppendContent("final")

		genCtx, genCancel := context.WithCancel(t.Context())

		n := NewTurnUINotifier(genCtx, &sessionLock, &currentAssistant, func(msg message.Message) { msgSvc.Notify(msg) })
		n.Start()
		require.NoError(t, n.Notify())

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
		var sessionLock sync.Mutex
		currentAssistant := &message.Message{ID: "msg-1", SessionID: "sess-1"}
		currentAssistant.AppendContent("stale")

		genCtx, genCancel := context.WithCancel(t.Context())
		defer genCancel()

		n := NewTurnUINotifier(genCtx, &sessionLock, &currentAssistant, func(msg message.Message) { msgSvc.Notify(msg) })
		n.Start()
		require.NoError(t, n.Notify())

		// drainPending must remove the queued snapshot before the ticker
		// gets a chance to publish it -- this is what runTurn calls right
		// before its own authoritative Update of the final state, so the
		// ticker cannot race a stale snapshot in behind it.
		n.DrainPending()
		synctest.Wait()

		require.Equal(t, 0, msgSvc.count(), "a drained snapshot must never reach messages.Notify")
	})
}

// blockingUINotifyMsgSvc parks its first Notify until release is closed and
// logs the order in which publishes finish and the test's own events happen.
type blockingUINotifyMsgSvc struct {
	message.Service
	inNotify chan struct{}
	release  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	events   []string
}

func (m *blockingUINotifyMsgSvc) Notify(message.Message) {
	m.once.Do(func() { close(m.inNotify) })
	<-m.release
	m.record("publish-done")
}

func (m *blockingUINotifyMsgSvc) record(event string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
}

// The receive-before-publish interleaving: the ticker has taken an unfinished
// snapshot off the channel but has not published it yet when runTurn drains the
// (now empty) channel and goes on to write the final state. drainPending must
// wait for that publish, or the stale snapshot lands after the final one and
// the server keeps it as the message's last update.
//
// Revert-check: without pubMu drainPending returns at once, "drain-return" is
// recorded first and the not-returned-yet assertion fails.
func TestTurnUINotifierDrainPendingWaitsForInFlightPublish(t *testing.T) {
	msgSvc := &blockingUINotifyMsgSvc{inNotify: make(chan struct{}), release: make(chan struct{})}
	var sessionLock sync.Mutex
	currentAssistant := &message.Message{ID: "msg-1", SessionID: "sess-1"}
	currentAssistant.AppendContent("unfinished")

	genCtx, genCancel := context.WithCancel(t.Context())
	defer genCancel()
	n := NewTurnUINotifier(genCtx, &sessionLock, &currentAssistant, func(msg message.Message) { msgSvc.Notify(msg) })
	n.Start()
	require.NoError(t, n.Notify())

	select {
	case <-msgSvc.inNotify:
	case <-time.After(5 * time.Second):
		t.Fatal("the ticker never started publishing the snapshot")
	}

	drained := make(chan struct{})
	go func() {
		n.DrainPending()
		msgSvc.record("drain-return")
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("drainPending returned while a snapshot was still being published")
	case <-time.After(100 * time.Millisecond):
	}

	close(msgSvc.release)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("drainPending never returned after the publish finished")
	}
	msgSvc.mu.Lock()
	defer msgSvc.mu.Unlock()
	require.Equal(t, []string{"publish-done", "drain-return"}, msgSvc.events)
}

func TestTurnUINotifierNotifyIsNoopWithNilAssistant(t *testing.T) {
	msgSvc := &fakeUINotifyMsgSvc{}
	var sessionLock sync.Mutex
	var currentAssistant *message.Message // nil: no assistant message created yet

	n := NewTurnUINotifier(context.Background(), &sessionLock, &currentAssistant, func(msg message.Message) { msgSvc.Notify(msg) })
	require.NoError(t, n.Notify(), "notify must not panic or error before PrepareStep has set an assistant message")

	select {
	case <-n.latestMsgCh:
		t.Fatal("notify must not enqueue anything when the assistant pointer is nil")
	default:
	}
}
