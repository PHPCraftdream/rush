// Stdout/stderr mirroring for `rush run --worktree`: the run's streams
// keep flowing to the process's original os.Stdout/os.Stderr while a
// bounded tail is captured so finish() can flush it into the main
// checkout's .rush/stdin.
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// streamMirrorCapBytes is how much of each stream the mirror keeps: the
// last 8 MiB of stdout and stderr.
const streamMirrorCapBytes = 8 << 20

// streamMirrorStopTimeout bounds the wait for each pump goroutine in
// stop(), so a wedged pump cannot hang finish() forever.
const streamMirrorStopTimeout = 5 * time.Second

// tailBuffer keeps at most the last cap bytes written to it.
type tailBuffer struct {
	buf []byte
	cap int
}

// newTailBuffer returns a tail buffer retaining the last cap bytes.
func newTailBuffer(cap int) *tailBuffer {
	return &tailBuffer{cap: cap}
}

// Write appends p and trims the buffer to its last cap bytes.
func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = t.buf[len(t.buf)-t.cap:]
	}
	return len(p), nil
}

// Bytes returns the retained tail. Not safe for concurrent use.
func (t *tailBuffer) Bytes() []byte {
	return t.buf
}

// streamMirror is the swap state started by startStreamMirror. Writes to
// the tail buffers happen only from their single pump goroutine; stop()
// reads the tails only after the pumps have exited (or timed out).
type streamMirror struct {
	prevStdout *os.File
	prevStderr *os.File
	outW       *os.File
	errW       *os.File
	outDone    chan struct{}
	errDone    chan struct{}
	outTail    *tailBuffer
	errTail    *tailBuffer
}

// startStreamMirror replaces os.Stdout and os.Stderr with os.Pipe write
// ends and pumps each pipe into io.MultiWriter(original, tailBuffer) on
// its own goroutine. os.Stdout and os.Stderr are *os.File vars, so
// downstream fmt.Fprintf calls keep working unchanged. Returns an error
// only if os.Pipe fails (any already-swapped stream is restored first).
func startStreamMirror() (*streamMirror, error) {
	m := &streamMirror{
		prevStdout: os.Stdout,
		prevStderr: os.Stderr,
		outDone:    make(chan struct{}),
		errDone:    make(chan struct{}),
		outTail:    newTailBuffer(streamMirrorCapBytes),
		errTail:    newTailBuffer(streamMirrorCapBytes),
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("mirroring stdout: os.Pipe failed: %w", err)
	}
	m.outW = outW
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, fmt.Errorf("mirroring stderr: os.Pipe failed: %w", err)
	}
	m.errW = errW

	os.Stdout = outW
	os.Stderr = errW
	go m.pump(outR, m.prevStdout, m.outTail, m.outDone)
	go m.pump(errR, m.prevStderr, m.errTail, m.errDone)
	return m, nil
}

// pump copies everything read from r into both the original stream and
// the tail buffer, then closes r. Runs on its own goroutine and is the
// only writer to tail.
func (m *streamMirror) pump(r *os.File, orig *os.File, tail *tailBuffer, done chan struct{}) {
	defer close(done)
	defer r.Close()
	_, _ = io.Copy(io.MultiWriter(orig, tail), r)
}

// stop closes the pipe write ends (which unblocks the pumps), waits for
// each pump with a 5s timeout, restores the original os.Stdout and
// os.Stderr, and returns the two tails. The returned error is non-nil
// when a pump did not exit in time.
func (m *streamMirror) stop() (outTail, errTail []byte, err error) {
	_ = m.outW.Close()
	_ = m.errW.Close()
	os.Stdout = m.prevStdout
	os.Stderr = m.prevStderr
	var errs []error
	for _, pump := range []struct {
		name string
		done chan struct{}
	}{{"stdout", m.outDone}, {"stderr", m.errDone}} {
		select {
		case <-pump.done:
		case <-time.After(streamMirrorStopTimeout):
			errs = append(errs, fmt.Errorf(
				"stream mirror pump for %s did not exit within %s",
				pump.name, streamMirrorStopTimeout))
		}
	}
	return m.outTail.Bytes(), m.errTail.Bytes(), errors.Join(errs...)
}
