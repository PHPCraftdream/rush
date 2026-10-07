// Tests for last-output tracking on boundedBuffer / BackgroundShell.
//
// Revert-check map (each test's target production line):
//   - TestBoundedBufferLastWriteAt_ZeroThenWriteThenUpdate:
//     `b.lastWriteNS.Store(time.Now().UnixNano())` in (*boundedBuffer).Write.
//   - TestBackgroundShellLastOutputAt:
//     `LastOutputAt` on *BackgroundShell (later-of-both-streams logic).
//   - TestLastWriteAt_ConcurrentReadWrite:
//     same atomic Store/Load pair — proves no data race under -race.
package shell

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBoundedBufferLastWriteAt_ZeroThenWriteThenUpdate(t *testing.T) {
	t.Parallel()
	b := newBoundedBuffer(64)

	_, ok := b.lastWriteAt()
	require.False(t, ok, "no write yet, so no last-write time")

	before := time.Now()
	_, err := b.WriteString("hello")
	require.NoError(t, err)
	first, ok := b.lastWriteAt()
	require.True(t, ok)
	require.False(t, first.Before(before.Add(-time.Second)))

	time.Sleep(2 * time.Millisecond)
	_, err = b.WriteString("world")
	require.NoError(t, err)
	second, ok := b.lastWriteAt()
	require.True(t, ok)
	require.True(t, second.After(first), "timestamp must update on subsequent writes")

	// Empty writes must not count as output.
	prev, _ := b.lastWriteAt()
	_, err = b.Write(nil)
	require.NoError(t, err)
	now, ok := b.lastWriteAt()
	require.True(t, ok)
	require.Equal(t, prev, now)

	// release() keeps the atomic counters intact.
	b.release()
	after, ok := b.lastWriteAt()
	require.True(t, ok)
	require.Equal(t, prev, after)
}

func TestBackgroundShellLastOutputAt(t *testing.T) {
	t.Parallel()

	// No buffers at all.
	bs := &BackgroundShell{}
	_, ok := bs.LastOutputAt()
	require.False(t, ok)

	// Buffers present but never written.
	bs = &BackgroundShell{stdout: newBoundedBuffer(16), stderr: newBoundedBuffer(16)}
	_, ok = bs.LastOutputAt()
	require.False(t, ok)

	// Write to stdout first.
	start := time.Now()
	_, err := bs.stdout.WriteString("out")
	require.NoError(t, err)
	ts, ok := bs.LastOutputAt()
	require.True(t, ok)
	require.False(t, ts.Before(start.Add(-time.Second)))

	// A later stderr write becomes the reported time.
	time.Sleep(2 * time.Millisecond)
	_, err = bs.stderr.WriteString("err")
	require.NoError(t, err)
	stderrTS, _ := bs.stderr.lastWriteAt()
	got, ok := bs.LastOutputAt()
	require.True(t, ok)
	require.Equal(t, stderrTS, got)

	// nil stderr buffer must not panic.
	bs = &BackgroundShell{stdout: newBoundedBuffer(16)}
	_, err = bs.stdout.WriteString("only-stdout")
	require.NoError(t, err)
	_, ok = bs.LastOutputAt()
	require.True(t, ok)
}

func TestLastWriteAt_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	b := newBoundedBuffer(1024)
	bs := &BackgroundShell{stdout: b, stderr: newBoundedBuffer(1024)}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = b.WriteString("x")
		}
		close(stop)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = b.lastWriteAt()
			_, _ = bs.LastOutputAt()
		}
	}()
	wg.Wait()

	_, ok := b.lastWriteAt()
	require.True(t, ok)
}
