//go:build !race

package contextlock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUncontendedOperationsAllocateNothing(t *testing.T) {
	background := context.Background()

	var writeLock RWMutex
	require.Zero(t, testing.AllocsPerRun(100, func() {
		writeLock.Lock()
		writeLock.Unlock()
	}))

	var readLock RWMutex
	require.Zero(t, testing.AllocsPerRun(100, func() {
		readLock.RLock()
		readLock.RUnlock()
	}))

	var contextWriteLock RWMutex
	require.Zero(t, testing.AllocsPerRun(100, func() {
		if !contextWriteLock.LockContext(background, true) {
			panic("background write lock was not acquired")
		}
		contextWriteLock.Unlock()
	}))

	var contextReadLock RWMutex
	require.Zero(t, testing.AllocsPerRun(100, func() {
		if !contextReadLock.LockContext(background, false) {
			panic("background read lock was not acquired")
		}
		contextReadLock.RUnlock()
	}))
}
