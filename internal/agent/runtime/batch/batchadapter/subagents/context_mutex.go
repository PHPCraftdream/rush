package subagents

import (
	"context"
	"sync"
)

// ContextMutex is a mutex whose waiter can give up with its context. The zero value
// is ready to use.
type ContextMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (m *ContextMutex) init() { m.once.Do(func() { m.ch = make(chan struct{}, 1) }) }

// Lock acquires m, or returns ctx's error.
func (m *ContextMutex) Lock(ctx context.Context) error {
	m.init()
	select {
	case m.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *ContextMutex) Unlock() { <-m.ch }
