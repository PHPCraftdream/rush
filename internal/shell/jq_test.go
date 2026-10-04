package shell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestJQ_CtxCancel verifies that handleJQ polls ctx during iteration and
// returns ctx.Err() (not an interp.ExitStatus) when the context is
// cancelled. This is what lets hook timeouts interrupt long-running jq
// filters rather than waiting for the iterator to terminate naturally.
func TestJQ_CtxCancel(t *testing.T) {
	t.Parallel()

	// `range(N)` generates a large stream of values. With a slurped input
	// the filter produces all N values in sequence; ctx cancellation
	// between values should short-circuit the loop.
	const filter = "range(10000000)"
	stdin := strings.NewReader("null\n")

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel almost immediately so we catch the next iteration check.
	cancel()

	err := handleJQ(ctx, []string{"jq", filter}, stdin, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected ctx cancel error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestJQ_CtxCancel_DuringFilter verifies cancellation mid-stream: ctx is
// cancelled after jq has started producing output, and the loop must
// observe the cancel on the next iteration rather than running to
// completion.
func TestJQ_CtxCancel_DuringFilter(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	// 100M values; without ctx polling this would take many seconds to
	// fully emit. With ctx polling the loop exits shortly after the
	// deadline.
	stdin := strings.NewReader("null\n")
	var stdout, stderr bytes.Buffer

	start := time.Now()
	err := handleJQ(ctx, []string{"jq", "-c", "range(100000000)"}, stdin, &stdout, &stderr)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ctx timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	// Allow generous slack for slow CI; the important invariant is that we
	// don't run all 100M iterations (which would take orders of magnitude
	// longer than 1s).
	if elapsed > time.Second {
		t.Fatalf("handleJQ took %v after 50ms timeout; ctx polling is not tight enough", elapsed)
	}
}

// countingReader serves its bytes in chunk-sized reads, counting them. The
// signalAt-th read closes readN, so the test cancels at a KNOWN mid-stream
// point — after exactly that many chunks — instead of racing a wall-clock
// timer against the scheduler (a 50ms timer fired before the first Read on
// a loaded machine, turning the old test into the fast-fail path it was not
// meant to exercise). Every read sleeps chunkDelay, so draining the whole
// source takes orders of magnitude longer than the safety deadlines below:
// a ctxReader that stopped polling ctx fails this test on a deadline, never
// on a timing assertion.
type countingReader struct {
	remaining  []byte
	chunk      int
	chunkDelay time.Duration
	served     int
	signalAt   int
	readN      chan struct{}
}

func (r *countingReader) Read(p []byte) (int, error) {
	if len(r.remaining) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.chunkDelay)
	n := min(len(p), min(r.chunk, len(r.remaining)))
	copy(p, r.remaining[:n])
	r.remaining = r.remaining[n:]
	r.served++
	if r.served == r.signalAt && r.readN != nil {
		close(r.readN)
		r.readN = nil
	}
	return n, nil
}

// TestJQ_CtxCancel_MidReadAll verifies that ctx cancellation observed
// *during* io.ReadAll — after several chunks have already been consumed
// — short-circuits the read via ctxReader, rather than draining the
// whole source. This is the guarantee the hook runner relies on when
// it feeds a large bytes.Reader payload.
//
// The reader itself signals when the third chunk has been consumed; the
// test cancels on that signal and then asserts the CONTRACT — a
// context.Canceled error and a source left undrained — with no clock
// comparison among the assertions. The two 10s deadlines are hang guards
// only: a regression that stops ctxReader from polling would drain the
// source in ~40s of chunk sleeps and die on the second deadline instead.
func TestJQ_CtxCancel_MidReadAll(t *testing.T) {
	t.Parallel()

	const (
		size       = 4 * 1024 * 1024 // a few MiB; fully draining it is ~40s of chunk sleeps
		chunk      = 512
		chunkDelay = 5 * time.Millisecond
		signalAt   = 3 // unambiguously mid-stream
	)
	reader := &countingReader{
		remaining:  bytes.Repeat([]byte("a"), size),
		chunk:      chunk,
		chunkDelay: chunkDelay,
		signalAt:   signalAt,
		readN:      make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// handleJQ is the only consumer: it must already be reading when the
	// mid-stream point is awaited, or the signal below could never fire.
	done := make(chan error, 1)
	go func() {
		done <- handleJQ(ctx, []string{"jq", "-R", "."}, reader, io.Discard, io.Discard)
	}()

	// Cancel at the known mid-stream point.
	select {
	case <-reader.readN:
	case <-time.After(10 * time.Second):
		t.Fatal("reader never served its third chunk; the test harness is stuck")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handleJQ did not return within 10s of cancel; ctxReader is not polling between chunks")
	}

	// Mid-stream, both ways, in READ CALLS — io.ReadAll offers buffers of
	// varying sizes, so bytes consumed are not a fixed function of the call
	// count: at least the signalled calls really happened (this is not the
	// pre-read fast-fail path), and the source was NOT drained (a full
	// drain needs at least size/chunk calls, however small the offered
	// buffers are — cancel was observed while reads were still pending).
	if reader.served < signalAt {
		t.Fatalf("reader was never really read from: %d read calls", reader.served)
	}
	if reader.served >= size/chunk {
		t.Fatalf("reader was fully drained (%d read calls); cancel was not observed mid-read", reader.served)
	}
	consumed := size - len(reader.remaining)
	if consumed <= 0 || consumed >= size {
		t.Fatalf("consumed %d of %d bytes: cancel was not observed mid-read", consumed, size)
	}
}

// TestJQ_CtxCancel_PreCancel verifies the fast-fail path: a ctx already
// cancelled before handleJQ is called returns context.Canceled
// immediately via the outer-loop guard, never entering io.ReadAll.
// Complements TestJQ_CtxCancel_MidReadAll.
func TestJQ_CtxCancel_PreCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start := time.Now()
	err := handleJQ(ctx, []string{"jq", "-R", "."},
		bytes.NewReader(bytes.Repeat([]byte("a"), 1024)),
		io.Discard, io.Discard)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("pre-cancel fast-fail took %v; outer guard is not firing", elapsed)
	}
}

// TestJQ_Success confirms the ctx-aware refactor did not regress the
// success path.
func TestJQ_Success(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := handleJQ(
		t.Context(),
		[]string{"jq", "-c", ".a"},
		strings.NewReader(`{"a":1}`),
		&stdout, io.Discard,
	)
	if err != nil {
		t.Fatalf("handleJQ returned error: %v", err)
	}
	if got := stdout.String(); got != "1\n" {
		t.Fatalf("stdout = %q, want %q", got, "1\n")
	}
}
