// Output capture via boundedBuffer: snapshot size caps, head+tail
// truncation with marker, growth detection after overflow, and
// concurrent read/write safety.
package shell

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBoundedBuffer_CapsSnapshotSize proves that writing far more than
// maxBytes into a boundedBuffer never yields a String() snapshot larger than
// the configured cap (plus the small, fixed marker overhead already
// accounted for in enforceLimitLocked's budget) — this is the core memory
// fix: previously (syncBuffer wrapping bytes.Buffer) an arbitrarily large
// write grew the snapshot without bound.
func TestBoundedBuffer_CapsSnapshotSize(t *testing.T) {
	t.Parallel()

	const maxBytes = 64 * 1024 // small cap so the test writes fast
	b := newBoundedBuffer(maxBytes)

	line := strings.Repeat("x", 100) + "\n"
	totalWritten := 0
	// Write ~50x the cap worth of data.
	for totalWritten < maxBytes*50 {
		n, err := b.WriteString(line)
		require.NoError(t, err)
		totalWritten += n
	}

	snapshot := b.String()
	require.LessOrEqual(t, len(snapshot), maxBytes,
		"bounded buffer snapshot must never exceed the configured cap")

	// The monotonic counter must reflect everything ever written, not just
	// what's resident.
	require.Equal(t, totalWritten, b.Len())
	require.Greater(t, b.Len(), maxBytes, "test sanity: must have written more than the cap")
}

// TestBoundedBuffer_PreservesHeadAndTailWithMarker proves that once a
// boundedBuffer overflows, the snapshot contains BOTH the first bytes ever
// written (head — usually identifies the command / earliest errors) AND the
// most recently written bytes (tail — current state), joined by an explicit
// truncation marker reporting how many bytes were dropped from the middle.
// This is the classic head+tail log-truncation pattern; a naive "keep only
// the first N" or "keep only the last N" (ring buffer) policy would fail
// this test by construction.
func TestBoundedBuffer_PreservesHeadAndTailWithMarker(t *testing.T) {
	t.Parallel()

	const maxBytes = 8 * 1024
	b := newBoundedBuffer(maxBytes)

	require.NoError(t, mustWrite(b, "HEAD-MARKER-START\n"))

	// Write enough padding to guarantee the head marker would be evicted by
	// a plain ring-buffer / tail-only policy.
	padding := strings.Repeat("pad-line-filler-content\n", 2000)
	require.NoError(t, mustWrite(b, padding))

	require.NoError(t, mustWrite(b, "TAIL-MARKER-END\n"))

	snapshot := b.String()

	require.Contains(t, snapshot, "HEAD-MARKER-START", "head must survive truncation")
	require.Contains(t, snapshot, "TAIL-MARKER-END", "tail must survive truncation")
	require.Regexp(t, `\[\d+ bytes truncated\]`, snapshot, "must report an explicit truncation marker with a byte count")
	require.LessOrEqual(t, len(snapshot), maxBytes)
}

func mustWrite(b *boundedBuffer, s string) error {
	_, err := b.WriteString(s)
	return err
}

// TestBackgroundShell_WaitForChange_DetectsGrowthAfterOverflow proves
// WaitForChange keeps detecting new output as "change" even once a stream's
// bounded buffer has already overflowed and started dropping old bytes —
// i.e. the monotonic writtenBytes counter (not the bounded/possibly-shrunk
// resident snapshot) drives the comparison, so growth is never silently
// missed after truncation kicks in.
func TestBackgroundShell_WaitForChange_DetectsGrowthAfterOverflow(t *testing.T) {
	t.Parallel()

	const maxBytes = 4 * 1024
	bgShell := &BackgroundShell{
		stdout:    newBoundedBuffer(maxBytes),
		stderr:    newBoundedBuffer(maxBytes),
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	// Overflow the buffer well past its cap before establishing a baseline.
	overflow := strings.Repeat("overflow-line-content-here\n", 1000)
	_, err := bgShell.stdout.WriteString(overflow)
	require.NoError(t, err)
	require.Greater(t, bgShell.stdout.Len(), maxBytes, "test sanity: must have overflowed")

	baseline := bgShell.stdout.Len() + bgShell.stderr.Len()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	t.Cleanup(cancel)

	waitDone := make(chan struct{})
	go func() {
		bgShell.WaitForChange(ctx, baseline)
		close(waitDone)
	}()

	// Confirm it does NOT return immediately (no growth yet).
	select {
	case <-waitDone:
		t.Fatal("WaitForChange returned before any new output was written past the baseline")
	case <-time.After(300 * time.Millisecond):
		// Expected: still waiting.
	}

	// Write more — even though this keeps overflowing the resident buffer,
	// the monotonic counter must still grow and WaitForChange must notice.
	_, err = bgShell.stdout.WriteString("more-output-after-overflow\n")
	require.NoError(t, err)

	select {
	case <-waitDone:
		// Expected: detected growth past baseline despite ongoing truncation.
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForChange did not detect growth after buffer overflow")
	}
}

// TestBackgroundShell_ConcurrentReadWrite exercises concurrent GetOutput /
// WaitForChange readers against a concurrent writer goroutine (mirroring the
// real ExecStream call pattern where the shell interpreter writes
// continuously while job_output/bash poll concurrently). Intended to run
// under `-race`: it must complete without triggering the race detector and
// without ever observing a snapshot larger than the cap.
func TestBackgroundShell_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()

	const maxBytes = 16 * 1024
	bgShell := &BackgroundShell{
		stdout:    newBoundedBuffer(maxBytes),
		stderr:    newBoundedBuffer(maxBytes),
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	var wg sync.WaitGroup

	// Writer: simulates ExecStream writing continuously.
	wg.Go(func() {
		for i := 0; i < 2000; i++ {
			_, _ = bgShell.stdout.WriteString("line of output data\n")
			_, _ = bgShell.stderr.WriteString("err line\n")
		}
	})

	// Readers: simulate job_output polling via GetOutput and WaitForChange.
	for range 4 {
		wg.Go(func() {
			for i := 0; i < 200; i++ {
				stdout, stderr, _, _ := bgShell.GetOutput()
				require.LessOrEqual(t, len(stdout), maxBytes)
				require.LessOrEqual(t, len(stderr), maxBytes)
			}
		})
	}
	wg.Go(func() {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		bgShell.WaitForChange(ctx, 0)
	})

	wg.Wait()
	close(bgShell.done)
}
