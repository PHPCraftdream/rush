package contextlock

import (
	"context"
	"sync"
)

type waiter struct {
	write   bool
	ready   chan struct{}
	granted bool
}

// RWMutex is a fair FIFO reader/writer lock with cancellation. Readers are
// granted as one batch, but never bypass an earlier writer.
type RWMutex struct {
	mu              sync.Mutex
	readers         int
	writer          bool
	waiters         []*waiter
	waitHook        func(bool)
	waiterAllocHook func(bool)
}

func (m *RWMutex) Lock() {
	_ = m.LockContext(context.Background(), true)
}

func (m *RWMutex) Unlock() {
	m.mu.Lock()
	if !m.writer {
		m.mu.Unlock()
		panic("sync: unlock of unlocked contextRWMutex")
	}
	m.writer = false
	m.grantLocked()
	m.mu.Unlock()
}

func (m *RWMutex) RLock() {
	_ = m.LockContext(context.Background(), false)
}

func (m *RWMutex) RUnlock() {
	m.mu.Lock()
	if m.readers == 0 {
		m.mu.Unlock()
		panic("sync: RUnlock of unlocked contextRWMutex")
	}
	m.readers--
	if m.readers == 0 {
		m.grantLocked()
	}
	m.mu.Unlock()
}

// LockContext acquires a read or write lock, returning false when ctx is
// canceled before the acquisition is returned to the caller.
func (m *RWMutex) LockContext(ctx context.Context, write bool) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	m.mu.Lock()
	// The grant decision is linearized under m.mu.
	if ctx.Err() != nil {
		m.mu.Unlock()
		return false
	}
	if !m.writer && len(m.waiters) == 0 && (!write || m.readers == 0) {
		if write {
			m.writer = true
		} else {
			m.readers++
		}
		m.mu.Unlock()
		return true
	}
	w := &waiter{write: write, ready: make(chan struct{})}
	if m.waiterAllocHook != nil {
		m.waiterAllocHook(write)
	}
	if m.waitHook != nil {
		m.waitHook(write)
	}
	m.waiters = append(m.waiters, w)
	m.mu.Unlock()

	select {
	case <-w.ready:
		m.mu.Lock()
		if ctx.Err() != nil {
			if w.write {
				m.writer = false
			} else {
				m.readers--
			}
			m.grantLocked()
			m.mu.Unlock()
			return false
		}
		m.mu.Unlock()
		return true
	case <-ctx.Done():
		m.mu.Lock()
		if w.granted {
			if w.write {
				m.writer = false
				m.grantLocked()
			} else {
				m.readers--
				if m.readers == 0 {
					m.grantLocked()
				}
			}
			m.mu.Unlock()
			return false
		}
		for i, queued := range m.waiters {
			if queued == w {
				m.waiters = append(m.waiters[:i], m.waiters[i+1:]...)
				break
			}
		}
		m.grantLocked()
		m.mu.Unlock()
		return false
	}
}

func (m *RWMutex) grantLocked() {
	if m.writer || len(m.waiters) == 0 {
		return
	}
	if m.waiters[0].write {
		if m.readers != 0 {
			return
		}
		w := m.waiters[0]
		m.waiters = m.waiters[1:]
		m.grantLockedWaiter(w)
		return
	}
	for len(m.waiters) > 0 && !m.waiters[0].write {
		w := m.waiters[0]
		m.waiters = m.waiters[1:]
		m.grantLockedWaiter(w)
	}
}

func (m *RWMutex) grantLockedWaiter(w *waiter) {
	w.granted = true
	if w.write {
		m.writer = true
	} else {
		m.readers++
	}
	close(w.ready)
}
