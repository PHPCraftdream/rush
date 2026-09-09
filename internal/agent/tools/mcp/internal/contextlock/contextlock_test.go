package contextlock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type cancelAtErrContext struct {
	context.Context
	calls  atomic.Int32
	cancel int32
}

func (c *cancelAtErrContext) Err() error {
	if c.calls.Add(1) >= c.cancel {
		return context.Canceled
	}
	return nil
}

func requireLockState(t *testing.T, lock *RWMutex, readers int, writer bool, waiters int, firstWaiterWrite *bool) {
	t.Helper()
	lock.mu.Lock()
	defer lock.mu.Unlock()
	require.Equal(t, readers, lock.readers)
	require.Equal(t, writer, lock.writer)
	require.Len(t, lock.waiters, waiters)
	if firstWaiterWrite != nil {
		require.Equal(t, *firstWaiterWrite, lock.waiters[0].write)
	}
}

func TestCancellationWinsBeforeFastGrant(t *testing.T) {
	var lock RWMutex
	ctx := &cancelAtErrContext{Context: context.Background(), cancel: 2}
	require.False(t, lock.LockContext(ctx, true))
	require.Zero(t, lock.readers)
	require.False(t, lock.writer)
}

func TestCancellationWinsAtQueuedGrantAndPreservesNextWaiter(t *testing.T) {
	var lock RWMutex
	lock.Lock()
	firstQueued := make(chan struct{})
	secondQueued := make(chan struct{})
	var queued atomic.Int32
	lock.waitHook = func(bool) {
		switch queued.Add(1) {
		case 1:
			close(firstQueued)
		case 2:
			close(secondQueued)
		}
	}

	firstCtx := &cancelAtErrContext{Context: context.Background(), cancel: 3}
	firstDone := make(chan bool, 1)
	go func() { firstDone <- lock.LockContext(firstCtx, true) }()
	<-firstQueued
	secondAcquired := make(chan struct{})
	go func() {
		if lock.LockContext(context.Background(), true) {
			close(secondAcquired)
		}
	}()
	<-secondQueued
	lock.Unlock()

	require.False(t, <-firstDone)
	select {
	case <-secondAcquired:
	case <-time.After(time.Second):
		t.Fatal("next waiter did not acquire after canceled grant")
	}
	lock.Unlock()
}

func TestReadersJoinBeforeWriterQueue(t *testing.T) {
	var lock RWMutex
	lock.RLock()

	readerQueued := make(chan struct{})
	readerAcquired := make(chan struct{})
	readerRelease := make(chan struct{})
	lock.waitHook = func(write bool) {
		if !write {
			close(readerQueued)
		}
	}
	go func() {
		if !lock.LockContext(context.Background(), false) {
			return
		}
		close(readerAcquired)
		<-readerRelease
		lock.RUnlock()
	}()
	select {
	case <-readerAcquired:
	case <-readerQueued:
		t.Fatal("a reader queued behind an active reader")
	case <-time.After(time.Second):
		t.Fatal("second reader did not acquire")
	}
	close(readerRelease)
	lock.RUnlock()
}

func TestWriterQueueBlocksLaterReaders(t *testing.T) {
	var lock RWMutex
	lock.RLock()
	writerQueued := make(chan struct{})
	readerQueued := make(chan struct{})
	var queuedOnce atomic.Int32
	lock.waitHook = func(write bool) {
		if write {
			if queuedOnce.Add(1) == 1 {
				close(writerQueued)
			}
			return
		}
		close(readerQueued)
	}

	writerAcquired := make(chan struct{})
	go func() {
		if lock.LockContext(context.Background(), true) {
			close(writerAcquired)
		}
	}()
	<-writerQueued
	readerAcquired := make(chan struct{})
	go func() {
		if lock.LockContext(context.Background(), false) {
			close(readerAcquired)
			lock.RUnlock()
		}
	}()
	<-readerQueued
	lock.RUnlock()
	select {
	case <-writerAcquired:
	case <-readerAcquired:
		t.Fatal("reader bypassed a queued writer")
	case <-time.After(time.Second):
		t.Fatal("queued writer did not acquire")
	}
	select {
	case <-readerAcquired:
		t.Fatal("reader acquired while writer held the lock")
	default:
	}
	lock.Unlock()
	select {
	case <-readerAcquired:
	case <-time.After(time.Second):
		t.Fatal("reader did not acquire after writer release")
	}
}

func TestCanceledWriterUnblocksQueuedReaders(t *testing.T) {
	var lock RWMutex
	lock.RLock()

	writerQueued := make(chan struct{})
	readerQueued := make(chan struct{})
	lock.waitHook = func(write bool) {
		if write {
			close(writerQueued)
			return
		}
		close(readerQueued)
	}

	writerCtx, cancelWriter := context.WithCancel(context.Background())
	readerCtx, cancelReader := context.WithCancel(context.Background())
	writerResult := make(chan bool, 1)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		writerResult <- lock.LockContext(writerCtx, true)
	}()
	<-writerQueued

	readerRelease := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		if lock.LockContext(readerCtx, false) {
			<-readerRelease
			lock.RUnlock()
		}
	}()
	<-readerQueued

	var releaseReaderOnce sync.Once
	releaseReader := func() {
		releaseReaderOnce.Do(func() { close(readerRelease) })
	}
	r1Held := true
	releaseR1 := func() {
		if r1Held {
			lock.RUnlock()
			r1Held = false
		}
	}
	writerResultRead := false
	writerWon := false
	t.Cleanup(func() {
		cancelWriter()
		cancelReader()
		releaseReader()
		releaseR1()
		if writerResultRead && writerWon {
			lock.Unlock()
		}
		<-writerDone
		<-readerDone
	})

	cancelWriter()
	writerWon = <-writerResult
	writerResultRead = true
	require.False(t, writerWon)
	requireLockState(t, &lock, 2, false, 0, nil)

	releaseReader()
	releaseR1()
	<-writerDone
	<-readerDone
}

func TestQueuedWriterPreservesReaderFairness(t *testing.T) {
	var lock RWMutex
	lock.RLock()

	writerQueued := make(chan struct{})
	readerQueued := make(chan struct{})
	lock.waitHook = func(write bool) {
		if write {
			close(writerQueued)
			return
		}
		close(readerQueued)
	}

	writerRelease := make(chan struct{})
	writerDone := make(chan struct{})
	writerCtx, cancelWriter := context.WithCancel(context.Background())
	go func() {
		defer close(writerDone)
		if lock.LockContext(writerCtx, true) {
			<-writerRelease
			lock.Unlock()
		}
	}()
	<-writerQueued

	readerCtx, cancelReader := context.WithCancel(context.Background())
	readerRelease := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		if lock.LockContext(readerCtx, false) {
			<-readerRelease
			lock.RUnlock()
		}
	}()
	<-readerQueued

	var releaseWriterOnce sync.Once
	releaseWriter := func() {
		releaseWriterOnce.Do(func() { close(writerRelease) })
	}
	var releaseReaderOnce sync.Once
	releaseReader := func() {
		releaseReaderOnce.Do(func() { close(readerRelease) })
	}
	r1Held := true
	releaseR1 := func() {
		if r1Held {
			lock.RUnlock()
			r1Held = false
		}
	}
	t.Cleanup(func() {
		cancelWriter()
		cancelReader()
		releaseWriter()
		releaseReader()
		releaseR1()
		<-writerDone
		<-readerDone
	})

	releaseR1()
	readerWaiter := false
	requireLockState(t, &lock, 0, true, 1, &readerWaiter)

	releaseWriter()
	<-writerDone
	requireLockState(t, &lock, 1, false, 0, nil)

	releaseReader()
	<-readerDone
}

func TestCanceledWriterRacesUnlock(t *testing.T) {
	for i := 0; i < 100; i++ {
		var lock RWMutex
		lock.RLock()

		writerQueued := make(chan struct{})
		readerQueued := make(chan struct{})
		lock.waitHook = func(write bool) {
			if write {
				close(writerQueued)
				return
			}
			close(readerQueued)
		}

		writerCtx, cancelWriter := context.WithCancel(context.Background())
		readerCtx, cancelReader := context.WithCancel(context.Background())
		writerResult := make(chan bool, 1)
		writerDone := make(chan struct{})
		writerRelease := make(chan struct{})
		go func() {
			defer close(writerDone)
			if lock.LockContext(writerCtx, true) {
				writerResult <- true
				<-writerRelease
				lock.Unlock()
				return
			}
			writerResult <- false
		}()
		<-writerQueued
		readerRelease := make(chan struct{})
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			if lock.LockContext(readerCtx, false) {
				<-readerRelease
				lock.RUnlock()
			}
		}()
		<-readerQueued

		var releaseWriterOnce sync.Once
		releaseWriter := func() {
			releaseWriterOnce.Do(func() { close(writerRelease) })
		}
		var releaseReaderOnce sync.Once
		releaseReader := func() {
			releaseReaderOnce.Do(func() { close(readerRelease) })
		}
		r1Held := true
		t.Cleanup(func() {
			cancelWriter()
			cancelReader()
			releaseWriter()
			releaseReader()
			if r1Held {
				lock.RUnlock()
			}
			<-writerDone
			<-readerDone
		})

		start := make(chan struct{})
		var race sync.WaitGroup
		race.Add(2)
		go func() {
			defer race.Done()
			<-start
			cancelWriter()
		}()
		go func() {
			defer race.Done()
			<-start
			lock.RUnlock()
		}()
		close(start)
		race.Wait()
		r1Held = false

		writerWon := <-writerResult
		if writerWon {
			readerWaiter := false
			requireLockState(t, &lock, 0, true, 1, &readerWaiter)
			releaseWriter()
			<-writerDone
		} else {
			requireLockState(t, &lock, 1, false, 0, nil)
		}
		<-writerDone
		releaseReader()
		<-readerDone
	}
}

func TestCanceledWaiterPreservesNextGrant(t *testing.T) {
	var lock RWMutex
	lock.Lock()
	firstQueued := make(chan struct{})
	secondQueued := make(chan struct{})
	var queued atomic.Int32
	lock.waitHook = func(bool) {
		switch queued.Add(1) {
		case 1:
			close(firstQueued)
		case 2:
			close(secondQueued)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan bool, 1)
	go func() { firstDone <- lock.LockContext(ctx, true) }()
	<-firstQueued
	secondAcquired := make(chan struct{})
	go func() {
		if lock.LockContext(context.Background(), true) {
			close(secondAcquired)
		}
	}()
	<-secondQueued
	cancel()
	require.False(t, <-firstDone)
	lock.Unlock()
	select {
	case <-secondAcquired:
	case <-time.After(time.Second):
		t.Fatal("next writer lost its grant after canceled waiter removal")
	}
	lock.Unlock()
}

func TestMisusePanicsLikeRWMutex(t *testing.T) {
	var lock RWMutex
	require.Panics(t, lock.Unlock)
	require.Panics(t, lock.RUnlock)
}

func TestFastPathConstructsNoWaiter(t *testing.T) {
	var lock RWMutex
	var allocations atomic.Int32
	lock.waiterAllocHook = func(bool) { allocations.Add(1) }

	require.True(t, lock.LockContext(context.Background(), false))
	lock.RUnlock()
	require.True(t, lock.LockContext(context.Background(), true))
	lock.Unlock()
	require.Zero(t, allocations.Load())

	lock.Lock()
	queued := make(chan struct{})
	lock.waitHook = func(bool) { close(queued) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- lock.LockContext(ctx, true) }()
	<-queued
	cancel()
	require.False(t, <-done)
	require.Equal(t, int32(1), allocations.Load())
	lock.Unlock()
}
