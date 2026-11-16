package server

import (
	"bytes"
	"sync"
)

// syncBuffer is a concurrency-safe bytes.Buffer — small local copy (sprint 0
// L4 escape rule): the definition lived in dispatch_test.go, which moved to
// hub (hub/dispatch_test.go, from dispatch_test.go:494-517 at HEAD); its
// staying consumers (api_serve_*, api_matrix_*, api_read_handlers, api_run
// integration tests) remain in package server. Verbatim copy; servertest
// (L6/L7) absorbs it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
