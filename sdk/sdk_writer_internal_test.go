package sdk

import (
	"sync"
	"testing"
	"time"
)

// gatedWriter lets the first underlying write pause while the test checks
// whether a second default writer can enter. It is intentionally not safe
// without the SDK's wrapper; its small state is protected only so the test
// can report the observed entry order.
type gatedWriter struct {
	mu            sync.Mutex
	firstEntered  chan struct{}
	secondEntered chan struct{}
	release       chan struct{}
	first         bool
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{
		firstEntered:  make(chan struct{}),
		secondEntered: make(chan struct{}),
		release:       make(chan struct{}),
	}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	first := !w.first
	if first {
		w.first = true
		close(w.firstEntered)
	} else {
		close(w.secondEntered)
	}
	w.mu.Unlock()
	if first {
		<-w.release
	}
	return len(p), nil
}

// TestClientDefaultWritersShareOneSerializationGate verifies that the
// stdout and stderr Options defaults use one Client-level gate. The second
// underlying write must stay out until the first one is released.
func TestClientDefaultWritersShareOneSerializationGate(t *testing.T) {
	underlying := newGatedWriter()
	client := &Client{stdout: underlying, stderr: underlying}

	stdout := client.defaultWriter(client.stdout)
	stderr := client.defaultWriter(client.stderr)
	firstDone := make(chan struct{})
	go func() {
		_, _ = stderr.Write([]byte("stderr"))
		close(firstDone)
	}()
	<-underlying.firstEntered

	secondDone := make(chan struct{})
	go func() {
		_, _ = stdout.Write([]byte("stdout"))
		close(secondDone)
	}()

	select {
	case <-underlying.secondEntered:
		t.Fatal("concurrent Options-level default writes entered the underlying writer")
	case <-time.After(100 * time.Millisecond):
	}

	close(underlying.release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first default write did not complete")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second default write did not complete")
	}
}
