package cliprovider

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedReader hands out chunks on demand and signals every time it is asked
// for data while empty, so a test knows the consumer fully processed the
// previous chunk before the next source moves.
type gatedReader struct {
	ch      chan []byte
	rest    []byte
	starved chan struct{}
}

func newGatedReader() *gatedReader {
	return &gatedReader{ch: make(chan []byte), starved: make(chan struct{}, 64)}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if len(g.rest) == 0 {
		g.starved <- struct{}{}
		b, ok := <-g.ch
		if !ok {
			return 0, io.EOF
		}
		g.rest = b
	}
	n := copy(p, g.rest)
	g.rest = g.rest[n:]
	return n, nil
}

// feed delivers one chunk and waits until the reader is starved again.
func (g *gatedReader) feed(t *testing.T, chunk string) {
	t.Helper()
	select {
	case g.ch <- []byte(chunk):
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not take the chunk")
	}
	select {
	case <-g.starved:
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not ask for more data")
	}
}

type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func startGated(t *testing.T) (a, b *gatedReader, out *lockedBuf, wait func()) {
	t.Helper()
	a, b, out = newGatedReader(), newGatedReader(), &lockedBuf{}
	wait = mergeLines(out, a, b)
	// Both readers are blocked waiting for their first chunk.
	for _, g := range []*gatedReader{a, b} {
		select {
		case <-g.starved:
		case <-time.After(10 * time.Second):
			t.Fatal("source goroutine did not start")
		}
	}
	return a, b, out, wait
}

// Revert-check: copyWholeLines replaced by a chunk-wise io.Copy lets the
// stdout line land inside the stderr line and this test goes red.
func TestMergeLinesDoesNotSpliceIntoALine(t *testing.T) {
	a, b, out, wait := startGated(t)
	a.feed(t, "aaaa")
	a.feed(t, "bbbb")
	b.feed(t, `{"type":"result"}`+"\n")
	a.feed(t, "cc\n")
	close(a.ch)
	close(b.ch)
	wait()
	if got, want := out.String(), `{"type":"result"}`+"\n"+"aaaabbbbcc\n"; got != want {
		t.Fatalf("merged output = %q, want %q", got, want)
	}
}

// A line longer than the read buffer is accumulated, not split.
func TestMergeLinesKeepsALineLongerThanTheReadBuffer(t *testing.T) {
	a, b, out, wait := startGated(t)
	long := strings.Repeat("x", 200*1024)
	a.feed(t, long)
	b.feed(t, "short\n")
	a.feed(t, "tail\n")
	close(a.ch)
	close(b.ch)
	wait()
	if got, want := out.String(), "short\n"+long+"tail\n"; got != want {
		t.Fatalf("merged output differs: len=%d want len=%d", len(got), len(want))
	}
}

// A final line without a newline is still delivered at EOF.
func TestMergeLinesFlushesAnUnterminatedLastLine(t *testing.T) {
	a, b, out, wait := startGated(t)
	a.feed(t, "no newline at end")
	close(a.ch)
	close(b.ch)
	wait()
	if got, want := out.String(), "no newline at end"; got != want {
		t.Fatalf("merged output = %q, want %q", got, want)
	}
}
