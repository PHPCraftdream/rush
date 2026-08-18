// Buffer release lifecycle after job completion: placeholder output,
// backing-memory reclamation, the retention timer, idempotent release,
// and TotalWrittenBytes surviving overflow and release.
package shell

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBackgroundShell_ReleaseBuffers_KeepsPlaceholderNotEmpty proves that
// once buffers are released post-completion, GetOutput doesn't silently
// return an empty string for a stream that actually produced output — that
// would look indistinguishable from "the command produced no output", which
// is a regression in its own right. It should instead surface an explicit
// placeholder.
func TestBackgroundShell_ReleaseBuffers_KeepsPlaceholderNotEmpty(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout: newBoundedBuffer(maxStreamBufferBytes),
		stderr: newBoundedBuffer(maxStreamBufferBytes),
		done:   make(chan struct{}),
	}
	_, err := bgShell.stdout.WriteString("some real output")
	require.NoError(t, err)
	close(bgShell.done)

	stdoutBefore, _, _, _ := bgShell.GetOutput()
	require.Equal(t, "some real output", stdoutBefore)

	bgShell.releaseBuffers()

	stdoutAfter, stderrAfter, done, _ := bgShell.GetOutput()
	require.True(t, done)
	require.NotEmpty(t, stdoutAfter, "must not silently look like empty output after release")
	require.NotEqual(t, "some real output", stdoutAfter, "test sanity: content must actually have been released")
	require.Empty(t, stderrAfter, "stream that never produced output stays empty after release")
}

// TestBackgroundShell_TotalWrittenBytes_SurvivesOverflow proves
// TotalWrittenBytes (the correct baseline for WaitForChange) keeps growing
// 1:1 with real writes even once the underlying stream has overflowed its
// cap and GetOutput's snapshot has stopped growing at the same rate. This
// guards against a regression where a caller mistakenly derives its
// WaitForChange baseline from len(stdout)+len(stderr) (a bounded snapshot)
// instead of TotalWrittenBytes: once overflowed, such a baseline would
// already sit below the live monotonic counters, making WaitForChange return
// immediately (falsely reporting "new output") instead of actually waiting.
func TestBackgroundShell_TotalWrittenBytes_SurvivesOverflow(t *testing.T) {
	t.Parallel()

	const maxBytes = 2 * 1024
	bgShell := &BackgroundShell{
		stdout:    newBoundedBuffer(maxBytes),
		stderr:    newBoundedBuffer(maxBytes),
		done:      make(chan struct{}),
		StartTime: time.Now(),
	}

	// Overflow stdout well past its cap.
	overflow := strings.Repeat("line-of-output-content\n", 500)
	_, err := bgShell.stdout.WriteString(overflow)
	require.NoError(t, err)

	snapshot := bgShell.stdout.String()
	total := bgShell.TotalWrittenBytes()

	require.Greater(t, total, len(snapshot),
		"once overflowed, the monotonic total must exceed the bounded snapshot length")
	require.Equal(t, len(overflow), total, "total must equal every byte ever written, not just what's resident")

	// A baseline taken from TotalWrittenBytes right now must NOT look like
	// "growth" has already happened relative to itself.
	baseline := bgShell.TotalWrittenBytes()
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	t.Cleanup(cancel)

	start := time.Now()
	bgShell.WaitForChange(ctx, baseline)
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, 350*time.Millisecond,
		"WaitForChange must actually wait out the ctx deadline when using a TotalWrittenBytes baseline with no further writes, not return immediately")
}

// TestBoundedBuffer_Release_FreesBackingMemory proves release() actually
// drops the backing arrays (letting GC reclaim them), not just resets the
// logical length — bytes.Buffer.Reset() keeps the capacity (documented stdlib
// behaviour), which would leave up to maxStreamBufferBytes per stream resident
// in the heap indefinitely. We check cap() of the underlying slice directly:
// after release it must be zero.
func TestBoundedBuffer_Release_FreesBackingMemory(t *testing.T) {
	t.Parallel()

	b := newBoundedBuffer(maxStreamBufferBytes)

	// Write well past the cap to force both a large backing-array allocation
	// and trigger truncation (so all counter fields are non-zero).
	big := strings.Repeat("x", maxStreamBufferBytes*2) // 6 MiB
	_, err := b.WriteString(big)
	require.NoError(t, err)

	// Capture counter state before release.
	writtenBefore := b.writtenBytes.Load()
	truncatedBefore := b.truncated.Load()
	droppedBefore := b.droppedBytes.Load()
	require.Positive(t, writtenBefore, "sanity: must have written data")
	require.True(t, truncatedBefore, "sanity: must have triggered truncation")
	require.Positive(t, droppedBefore, "sanity: must have dropped bytes")

	require.Positive(t, cap(b.buf.Bytes()),
		"sanity: buf must have a backing array")

	b.release()

	// Direct memory-free assertion: cap must be zero after release.
	// Reset() would have kept the old capacity alive here.
	require.Zero(t, cap(b.buf.Bytes()),
		"release must drop the buf backing array, not just reset the length")
	require.Zero(t, cap(b.head.Bytes()),
		"release must drop the head backing array, not just reset the length")

	// Counters must be preserved — they drive TotalWrittenBytes /
	// WaitForChange baseline and String()'s truncation marker.
	require.Equal(t, writtenBefore, b.writtenBytes.Load(),
		"writtenBytes must survive release")
	require.Equal(t, truncatedBefore, b.truncated.Load(),
		"truncated flag must survive release")
	require.Equal(t, droppedBefore, b.droppedBytes.Load(),
		"droppedBytes must survive release")
}

// TestBackgroundShell_BufferReleaseTimer_FiresWithoutCleanup proves the
// post-completion buffer-release timer (armed in Start's completion goroutine)
// fires and releases buffers after the retention window WITHOUT requiring a
// subsequent bash task to trigger Cleanup. This is the core scheduling fix:
// previously releaseBuffers was only reachable via Cleanup, which only ran
// when the next bash task started.
//
// NOT parallel: temporarily overrides the package-level bufferRetention.
func TestBackgroundShell_BufferReleaseTimer_FiresWithoutCleanup(t *testing.T) {
	// Override the retention to a short window so the test runs fast.
	originalRetention := bufferRetention
	bufferRetention = 100 * time.Millisecond
	t.Cleanup(func() { bufferRetention = originalRetention })

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo hi", "")
	require.NoError(t, err)

	// Wait for the job to complete.
	require.Eventually(t, bgShell.IsDone, 5*time.Second, 50*time.Millisecond,
		"job should complete")

	// Sanity: buffers must NOT be released immediately after completion —
	// the timer hasn't fired yet.
	require.False(t, bgShell.bufReleased.Load(),
		"buffers must not be released immediately after completion")

	// Now wait for the timer to fire — well past the 100ms retention but far
	// less than the default 15 minutes. Crucially, we do NOT call Cleanup
	// or start any new bash task: the timer is the sole release trigger.
	require.Eventually(t, func() bool {
		return bgShell.bufReleased.Load()
	}, 3*time.Second, 20*time.Millisecond,
		"buffer-release timer must fire without a Cleanup call or new bash task")

	// TotalWrittenBytes must still report the real total after release
	// (writtenBytes is preserved, not zeroed) — this guards the
	// WaitForChange baseline path used by job_output.
	require.Positive(t, bgShell.TotalWrittenBytes(),
		"TotalWrittenBytes must survive timer-based buffer release")
}

// TestBackgroundShell_ReleaseBuffers_Idempotent proves calling releaseBuffers
// multiple times — e.g. the completion timer fires AND then Cleanup runs when
// the next bash task starts — is safe: no panic, no state corruption. The
// bufReleased Swap guard inside releaseBuffers ensures only the first call
// performs the actual release.
func TestBackgroundShell_ReleaseBuffers_Idempotent(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{
		stdout: newBoundedBuffer(maxStreamBufferBytes),
		stderr: newBoundedBuffer(maxStreamBufferBytes),
		done:   make(chan struct{}),
	}
	_, err := bgShell.stdout.WriteString("real output content")
	require.NoError(t, err)
	close(bgShell.done)

	totalBefore := bgShell.TotalWrittenBytes()
	require.Positive(t, totalBefore)

	// First release (simulates the completion timer firing).
	require.NotPanics(t, func() { bgShell.releaseBuffers() })
	require.True(t, bgShell.bufReleased.Load(),
		"bufReleased must be set after first release")

	stdoutAfter1, _, done1, _ := bgShell.GetOutput()
	require.True(t, done1)
	require.NotEmpty(t, stdoutAfter1,
		"placeholder must be returned after first release")

	// TotalWrittenBytes must survive the first release.
	require.Equal(t, totalBefore, bgShell.TotalWrittenBytes(),
		"TotalWrittenBytes must not change after release")

	// Second release (simulates Cleanup running later). Must not panic.
	require.NotPanics(t, func() { bgShell.releaseBuffers() })

	// Observable state must be identical after the second call.
	stdoutAfter2, _, done2, _ := bgShell.GetOutput()
	require.True(t, done2)
	require.Equal(t, stdoutAfter1, stdoutAfter2,
		"second releaseBuffers must not change observable state")
	require.Equal(t, totalBefore, bgShell.TotalWrittenBytes(),
		"TotalWrittenBytes must still be unchanged after double release")
}
