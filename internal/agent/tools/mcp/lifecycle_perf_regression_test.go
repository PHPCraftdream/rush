package mcp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestMCPAdmissionFinalTurnCancellationAfterAcquisitionReleasesLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mcpInitTestHooks.Lock()
	previous := mcpInitTestHooks.beforeAdmissionFinalValidate
	mcpInitTestHooks.beforeAdmissionFinalValidate = func(string) { cancel() }
	mcpInitTestHooks.Unlock()
	defer func() {
		mcpInitTestHooks.Lock()
		mcpInitTestHooks.beforeAdmissionFinalValidate = previous
		mcpInitTestHooks.Unlock()
	}()

	var mutated atomic.Bool
	err := withMCPAdmissionFinalTurn(ctx, "canceled-after-acquisition", config.MCPAdmissionGuard{}, func() error {
		mutated.Store(true)
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, mutated.Load())
	lifecycleMu.Lock()
	lifecycleMu.Unlock()
}

func TestServerLeaseContentionUsesOneCancelableQueueWait(t *testing.T) {
	lease := serverLeaseFor("queued-contention")
	lease.Lock()

	var attempts atomic.Int32
	attempted := make(chan struct{})
	serverLeaseHooks.Lock()
	previous := serverLeaseHooks.beforeTryLockFn
	serverLeaseHooks.beforeTryLockFn = func(candidate *serverLease) {
		if candidate == lease {
			attempts.Add(1)
			select {
			case <-attempted:
			default:
				close(attempted)
			}
		}
	}
	serverLeaseHooks.Unlock()
	defer func() {
		serverLeaseHooks.Lock()
		serverLeaseHooks.beforeTryLockFn = previous
		serverLeaseHooks.Unlock()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- lease.lockContext(ctx, true) }()
	<-attempted
	cancel()
	require.False(t, <-done)
	require.Equal(t, int32(1), attempts.Load())

	lease.Unlock()
	require.Zero(t, leases.Len())
}

func TestRetirementReleaseQueuesExactlyOneBlockingClose(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var closeCalls atomic.Int32
	session := &ClientSession{terminal: func() {
		closeCalls.Add(1)
		close(closeStarted)
		<-releaseClose
	}}
	lifecycleMu.Lock()
	owner.trackSessionLocked(session, "release-close")
	lifecycleMu.Unlock()
	_, releaseOperation, usable := session.acquireOperation(context.Background())
	require.True(t, usable)
	require.False(t, session.retire())

	releaseDone := make(chan struct{})
	go func() {
		releaseOperation()
		close(releaseDone)
	}()
	select {
	case <-releaseDone:
	case <-time.After(time.Second):
		t.Fatal("final operation release waited for transport close")
	}
	<-closeStarted
	require.Equal(t, int32(1), closeCalls.Load())
	select {
	case <-session.closedDone():
		t.Fatal("blocking transport closed before its barrier was released")
	default:
	}

	close(releaseClose)
	select {
	case <-session.closedDone():
	case <-time.After(time.Second):
		t.Fatal("async transport closer did not complete")
	}
	require.NoError(t, session.Close())
	require.Equal(t, int32(1), closeCalls.Load())
}

func TestConcurrentFinalReleasesQueueOneCloseAndReturn(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	const operations = 4
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var closeCalls atomic.Int32
	session := &ClientSession{terminal: func() {
		closeCalls.Add(1)
		close(closeStarted)
		<-releaseClose
	}}
	lifecycleMu.Lock()
	owner.trackSessionLocked(session, "concurrent-final-release")
	lifecycleMu.Unlock()

	releases := make([]func(), 0, operations)
	for range operations {
		_, release, usable := session.acquireOperation(context.Background())
		require.True(t, usable)
		releases = append(releases, release)
	}
	require.False(t, session.retire())
	start := make(chan struct{})
	releaseDone := make(chan struct{}, operations)
	for _, release := range releases {
		go func(release func()) {
			<-start
			release()
			releaseDone <- struct{}{}
		}(release)
	}
	close(start)
	for range operations {
		select {
		case <-releaseDone:
		case <-time.After(time.Second):
			t.Fatal("final operation release waited for blocked Close")
		}
	}
	<-closeStarted
	require.Equal(t, int32(1), closeCalls.Load())
	select {
	case <-session.closedDone():
		t.Fatal("Close completed before its barrier was released")
	default:
	}
	close(releaseClose)
	select {
	case <-session.closedDone():
	case <-time.After(time.Second):
		t.Fatal("queued Close did not finish after barrier release")
	}
	require.Equal(t, int32(1), closeCalls.Load())
}

func TestOwnerCloserUsesOneWorkerForBlockedRetirements(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)

	const count = 8
	releaseClose := make(chan struct{})
	var releaseOnce sync.Once
	var closeStarts atomic.Int32
	firstHook := make(chan struct{})
	owner.closer.mu.Lock()
	owner.closer.beforeClose = func(closeRequest) {
		if closeStarts.Add(1) == 1 {
			close(firstHook)
		}
	}
	owner.closer.mu.Unlock()
	defer func() {
		releaseOnce.Do(func() { close(releaseClose) })
		owner.closer.mu.Lock()
		owner.closer.beforeClose = nil
		owner.closer.mu.Unlock()
		require.NoError(t, owner.Close(context.Background()))
	}()

	sessionsToClose := make([]*ClientSession, 0, count)
	for i := 0; i < count; i++ {
		session := &ClientSession{terminal: func() { <-releaseClose }}
		lifecycleMu.Lock()
		owner.trackSessionLocked(session, "blocked-retire")
		lifecycleMu.Unlock()
		sessionsToClose = append(sessionsToClose, session)
		retireMCPClient("blocked-retire", session)
	}
	<-firstHook
	require.Equal(t, int32(1), closeStarts.Load(), "blocked Close must stop the single closer worker")
	releaseOnce.Do(func() { close(releaseClose) })
	for _, session := range sessionsToClose {
		select {
		case <-session.closedDone():
		case <-time.After(time.Second):
			t.Fatal("owner closer did not drain a retired session")
		}
	}
}

func TestOwnerCloseKeepsFenceWhileDetachedCloseBlocks(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	session := &ClientSession{terminal: func() {
		close(closeStarted)
		<-releaseClose
	}}
	lifecycleMu.Lock()
	owner.trackSessionLocked(session, "detached-fence")
	lifecycleMu.Unlock()
	retireMCPClient("detached-fence", session)
	ctx, cancel := context.WithCancel(context.Background())
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(ctx) }()
	<-closeStarted
	cancel()
	require.ErrorIs(t, <-closeDone, context.Canceled)
	_, err = Acquire()
	require.ErrorIs(t, err, ErrOwnerBusy)
	close(releaseClose)
	require.NoError(t, owner.Close(context.Background()))
	next, err := Acquire()
	require.NoError(t, err)
	require.NoError(t, next.Close(context.Background()))
}

func TestOwnerCloseCancellationWinsWhileLifecycleIsHeld(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)

	closeRequested := make(chan struct{})
	var requestOnce sync.Once
	mcpInitTestHooks.Lock()
	previous := mcpInitTestHooks.beforeCloseRequest
	mcpInitTestHooks.beforeCloseRequest = func(requested *Owner) {
		if requested == owner {
			requestOnce.Do(func() { close(closeRequested) })
		}
	}
	mcpInitTestHooks.Unlock()
	t.Cleanup(func() {
		mcpInitTestHooks.Lock()
		mcpInitTestHooks.beforeCloseRequest = previous
		mcpInitTestHooks.Unlock()
	})

	lifecycleMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(ctx) }()
	awaitMCPSignal(t, closeRequested)
	cancel()
	require.ErrorIs(t, <-closeDone, context.Canceled)
	select {
	case <-owner.closeDone:
		t.Fatal("close coordinator completed while lifecycleMu was held")
	default:
	}
	lifecycleMu.Unlock()

	require.NoError(t, owner.Close(context.Background()))
	next, err := Acquire()
	require.NoError(t, err)
	require.NoError(t, next.Close(context.Background()))
}

func TestTrackRejectsAdoptionAfterCloseBegins(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	session := &ClientSession{terminal: func() {
		close(closeStarted)
		<-releaseClose
	}}
	closeDone := make(chan struct{})
	go func() {
		_ = session.Close()
		close(closeDone)
	}()
	<-closeStarted
	lifecycleMu.Lock()
	owner.trackSessionLocked(session, "close-race")
	tracked := len(owner.trackedSessions)
	lifecycleMu.Unlock()
	require.Zero(t, tracked)
	close(releaseClose)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("close did not complete after barrier release")
	}
	require.NoError(t, owner.Close(context.Background()))
}
