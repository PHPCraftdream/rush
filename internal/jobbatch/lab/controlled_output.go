package lab

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func (a *controlledAttempt) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	c := a.owner
	c.mu.Lock()
	if a.phase == controlledDisposed {
		c.mu.Unlock()
		return jobbatch.OutputChunk{}, ErrUnknownAttempt
	}
	a.reads++
	var gate *controlledGate
	if a.holdRead {
		if a.read != nil {
			a.reads--
			c.mu.Unlock()
			return jobbatch.OutputChunk{}, ErrAttemptPhase
		}
		gate = &controlledGate{done: make(chan struct{})}
		a.read = gate
		a.holdRead = false
	}
	c.observation(a, "read_entered", 0)
	c.signal()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		a.reads--
		if gate != nil {
			a.read = nil
		}
		c.observation(a, "read_returned", 0)
		c.signal()
		c.mu.Unlock()
	}()
	if gate != nil {
		<-gate.done
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		end := uint64(len(a.output))
		if cursor < end {
			return jobbatch.OutputChunk{Data: append([]byte(nil), a.output[cursor:]...), Next: end, EOF: a.phase == controlledReported}, nil
		}
		if a.phase == controlledReported {
			return jobbatch.OutputChunk{Next: end, EOF: true}, nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			c.mu.Lock()
			return jobbatch.OutputChunk{}, ctx.Err()
		}
		c.mu.Lock()
	}
}
