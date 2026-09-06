package config

import (
	"context"
	"sync"
)

// reloadMutex is a sync.Mutex with a context-aware admission path. The
// release notification lets waiters sleep without creating a goroutine or
// polling while preserving the ordinary Lock/TryLock/Unlock operations used
// by the reload deduplication paths.
type reloadMutex struct {
	mu sync.Mutex

	stateMu  sync.Mutex
	released chan struct{}
}

func (m *reloadMutex) Lock() {
	m.mu.Lock()
}

func (m *reloadMutex) TryLock() bool {
	return m.mu.TryLock()
}

func (m *reloadMutex) Unlock() {
	m.mu.Unlock()

	m.stateMu.Lock()
	released := m.released
	if released == nil {
		released = make(chan struct{})
	}
	m.released = make(chan struct{})
	m.stateMu.Unlock()

	close(released)
}

// LockContext waits for admission without acquiring the mutex after ctx is
// canceled. A caller that supplies an already-canceled context never touches
// the mutex or any reload hand-off state.
func (m *reloadMutex) LockContext(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		released := m.releaseSignal()
		if m.mu.TryLock() {
			if err := ctx.Err(); err != nil {
				m.Unlock()
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-released:
		}
	}
}

func (m *reloadMutex) releaseSignal() <-chan struct{} {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.released == nil {
		m.released = make(chan struct{})
	}
	return m.released
}
