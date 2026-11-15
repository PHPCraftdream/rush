package shell

import (
	"context"
	"io"
	"sync"
)

// ExecutionOutput is a caller-owned read from the retained merged byte stream.
type ExecutionOutput struct {
	Data      []byte
	Oldest    uint64
	Next      uint64
	Truncated bool
	EOF       bool
}

type retainedOutput struct {
	mu      sync.Mutex
	data    []byte
	next    uint64
	limit   int
	changed chan struct{}
}

func newRetainedOutput(limit int) *retainedOutput {
	return &retainedOutput{limit: limit, changed: make(chan struct{})}
}

func (o *retainedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.next += uint64(len(p))
	if len(p) >= o.limit {
		o.data = append(o.data[:0], p[len(p)-o.limit:]...)
	} else {
		drop := len(o.data) + len(p) - o.limit
		if drop > 0 {
			o.data = append(o.data[:0], o.data[drop:]...)
		}
		o.data = append(o.data, p...)
	}
	close(o.changed)
	o.changed = make(chan struct{})
	return len(p), nil
}

// readOutput returns the bounded snapshot of the merged stream at cursor,
// waiting for growth or completion. isDone and doneCh carry the owning
// shell's completion signal so this method stays free of the manager types.
func (o *retainedOutput) readOutput(ctx context.Context, cursor uint64, isDone func() bool, doneCh <-chan struct{}) (ExecutionOutput, error) {
	for {
		o.mu.Lock()
		oldest := o.next - uint64(len(o.data))
		start := cursor
		if start < oldest {
			start = oldest
		}
		if start > o.next {
			start = o.next
		}
		chunk := ExecutionOutput{Oldest: oldest, Next: o.next, Truncated: cursor < oldest}
		chunk.Data = append([]byte(nil), o.data[start-oldest:]...)
		changed := o.changed
		done := isDone()
		chunk.EOF = done
		o.mu.Unlock()
		if len(chunk.Data) > 0 || done {
			return chunk, nil
		}
		select {
		case <-changed:
		case <-doneCh:
		case <-ctx.Done():
			return ExecutionOutput{}, ctx.Err()
		}
	}
}

// retainedWriter preserves legacy per-stream snapshots alongside merged output.
// The merged mutex orders bytes as observed by writes, not process emission time.
type retainedWriter struct {
	snapshot io.Writer
	merged   *retainedOutput
}

func (w retainedWriter) Write(p []byte) (int, error) {
	n, err := w.snapshot.Write(p)
	_, _ = w.merged.Write(p[:n])
	return n, err
}
