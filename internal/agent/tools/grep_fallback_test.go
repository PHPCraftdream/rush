// Regex-fallback engine tests for the grep tool: fileMatches/readBoundedLine
// callbacks, oversized-line truncation, cancellation, phantom-line edges,
// and the scan-then-drain-then-Wait pipe contract with a real rg process.

package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFileMatchesCallbackStopsEarly verifies fileMatches stops reading the file
// immediately when the callback returns false, rather than scanning the entire
// file. This tests the regex fallback path (no ripgrep dependency).
func TestFileMatchesCallbackStopsEarly(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// Create a file with 10000 matching lines.
	var content strings.Builder
	for i := 0; i < 10000; i++ {
		content.WriteString("match line\n")
	}
	path := filepath.Join(tempDir, "big.txt")
	require.NoError(t, os.WriteFile(path, []byte(content.String()), 0o644))

	re := regexp.MustCompile("match")
	callCount := 0
	err := fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		callCount++
		return false // stop immediately after first match
	})
	require.NoError(t, err)
	require.Equal(t, 1, callCount,
		"callback must be called exactly once when it returns false on the first match")
}

// TestFileMatchesCallbackCollectsAll verifies the callback path still finds all
// matches when the callback always returns true.
func TestFileMatchesCallbackCollectsAll(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	content := "match one\nxyz\nmatch two\nmatch three\n"
	path := filepath.Join(tempDir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	re := regexp.MustCompile("match")
	var collected []lineMatch
	err := fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		collected = append(collected, lm)
		return true
	})
	require.NoError(t, err)
	require.Len(t, collected, 3)
	require.Equal(t, 1, collected[0].lineNum)
	require.Equal(t, 3, collected[1].lineNum)
	require.Equal(t, 4, collected[2].lineNum)
}

// TestScanThenWaitPattern_DrainsOnScanErrorInsteadOfHanging is a regression
// test for a deadlock confirmed by direct reproduction against the exact
// scan-then-Wait shape searchWithRipgrep uses.
//
// rg --json emits one JSON object per line, and that line embeds the ENTIRE
// matched source line. A single matched line long enough to exceed the
// scanner's 4 MiB buffer (a minified bundle, a base64 blob, any
// pathologically long line — a realistic real-world scenario, not
// contrived) makes bufio.Scanner return bufio.ErrTooLong and stop — before
// rg has necessarily finished writing the rest of its output. Per os/exec's
// documented contract, calling Wait() before all reads from the pipe
// complete can deadlock once the child blocks writing to a full OS pipe
// buffer with nobody draining it. Confirmed directly with a standalone
// reproduction of this exact pattern (scan loop, then bare cmd.Wait() with
// no drain-on-error): Wait() hung indefinitely (well past an 8s timeout)
// against a real `rg --json` invocation over a ~6 MiB single-line file.
// searchWithRipgrep's fix drains the pipe (io.Copy to io.Discard) when
// scanner.Err() is non-nil, before calling Wait — this test proves that
// exact pattern is deadlock-free using the SAME real `rg` binary and the
// SAME oversized-line scenario.
//
// This test does NOT call searchWithRipgrep directly: getRgSearchCmd goes
// through getRg(), which is a sync.OnceValue that unconditionally returns
// "" whenever testing.Testing() is true (see rg.go) — a pre-existing,
// deliberate test-time guard that means searchWithRipgrep can never
// actually invoke a real rg process under `go test`, by design. Bypassing
// that package-wide memoized guard just for this one test would be a
// larger, riskier change than the deadlock fix itself, so this test
// exercises the identical scan-then-drain-then-Wait sequence standalone
// against a real rg process resolved via exec.LookPath, which is exactly
// what triggers and then resolves the deadlock — the file-format/JSON
// parsing differences between this and searchWithRipgrep are immaterial to
// what's being proven (the pipe-draining contract).
func TestScanThenWaitPattern_DrainsOnScanErrorInsteadOfHanging(t *testing.T) {
	t.Parallel()
	rgPath, lookErr := exec.LookPath("rg")
	if lookErr != nil {
		t.Skip("rg is not in $PATH")
	}
	tempDir := t.TempDir()

	// One line comfortably past the 4 MiB scanner buffer, followed by
	// several short matching lines that would remain unread in the pipe if
	// the scan loop stopped without draining.
	var b strings.Builder
	b.WriteString(strings.Repeat("x", 6*1024*1024))
	b.WriteString(" NEEDLE_OVERSIZED_LINE\n")
	for i := range 50 {
		fmt.Fprintf(&b, "short line %d NEEDLE_OVERSIZED_LINE\n", i)
	}
	path := filepath.Join(tempDir, "huge.txt")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))

	runOnce := func(drainOnScanError bool) error {
		cmd := exec.CommandContext(t.Context(), rgPath, "--json", "NEEDLE_OVERSIZED_LINE", path)
		stdout, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
		}
		if drainOnScanError && scanner.Err() != nil {
			_, _ = io.Copy(io.Discard, stdout)
		}
		return cmd.Wait()
	}

	t.Run("with_drain_returns_promptly", func(t *testing.T) {
		t.Parallel()
		done := make(chan error, 1)
		go func() { done <- runOnce(true) }()
		select {
		case <-done:
			// rg exits fine either way; we only care that Wait() returned.
		case <-time.After(15 * time.Second):
			t.Fatal("Wait() hung even with the drain-on-scan-error fix applied")
		}
	})

	t.Run("without_drain_hangs_confirming_the_bug_shape", func(t *testing.T) {
		t.Parallel()
		done := make(chan error, 1)
		go func() { done <- runOnce(false) }()
		select {
		case <-done:
			t.Fatal("expected Wait() to hang without the drain (bug reproduction did not trigger — " +
				"the oversized-line/pipe-buffer scenario may no longer apply on this platform)")
		case <-time.After(5 * time.Second):
			// Expected: this proves the bug is real absent the fix, so the
			// "with_drain" subtest above is actually testing something.
		}
	})
}

// TestSearchFilesCancelledContextSkipsFallback verifies that when the context
// is already cancelled before searchFiles is called, the regex fallback walk is
// NOT launched: the ripgrep "not found" error is returned as-is rather than the
// walk running and surfacing context.Canceled.
func TestSearchFilesCancelledContextSkipsFallback(t *testing.T) {
	t.Parallel()
	// A tree large enough that a full fallback walk would be observably slow.
	tempDir := t.TempDir()
	for i := range 4000 {
		p := filepath.Join(tempDir, fmt.Sprintf("d%d", i/100), fmt.Sprintf("f%d.txt", i))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("needle line\n"), 0o644))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call

	start := time.Now()
	matches, truncated, err := searchFiles(ctx, "needle", tempDir, "", 100)
	elapsed := time.Since(start)

	require.Error(t, err)
	// The guard short-circuits: the ripgrep "not found" error is returned
	// as-is. Had the fallback run, it would surface context.Canceled instead.
	require.False(t, errors.Is(err, context.Canceled), "fallback must not run, got %v", err)
	require.False(t, errors.Is(err, context.DeadlineExceeded), "fallback must not run, got %v", err)
	require.Contains(t, err.Error(), "ripgrep", "the ripgrep error must be returned directly, proving the fallback was skipped")
	require.Empty(t, matches)
	require.False(t, truncated)
	t.Logf("cancelled searchFiles returned in %s (err=%v)", elapsed, err)
}

// TestFileMatchesBoundedLineTruncatesHugeLine verifies that a single line with
// no newline, several MiB long, does not force an unbounded allocation: it is
// truncated to the cap, marked, and matched without crashing or hanging.
func TestFileMatchesBoundedLineTruncatesHugeLine(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// ~6 MiB single line, needle within the retained (first 4 MiB) portion.
	var b strings.Builder
	b.WriteString(strings.Repeat("x", 1024*1024))   // 1 MiB prefix
	b.WriteString("NEEDLE")                         // match, well under the 4 MiB cap
	b.WriteString(strings.Repeat("y", 5*1024*1024)) // 5 MiB suffix, no newline
	huge := b.String()
	path := filepath.Join(tempDir, "huge.txt")
	require.NoError(t, os.WriteFile(path, []byte(huge), 0o644))

	re := regexp.MustCompile("NEEDLE")
	var got []lineMatch
	require.NoError(t, fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		got = append(got, lm)
		return true
	}))

	require.Len(t, got, 1, "the single huge line must be matched exactly once")
	require.LessOrEqual(t, len(got[0].lineText), maxFallbackLineBytes+len(fallbackTruncateSuffix),
		"truncated line must not hold the full oversized line (%d bytes)", len(huge))
	require.True(t, strings.HasSuffix(got[0].lineText, fallbackTruncateSuffix),
		"truncated line must carry the truncation marker")
	t.Logf("huge-line match lineText length=%d (full line=%d)", len(got[0].lineText), len(huge))
}

// TestFileMatchesBoundedLineContinuesAfterTruncation verifies that after a
// truncated (oversized) line the scanner keeps scanning subsequent lines — the
// key advantage over bufio.Scanner, which stops dead on ErrTooLong.
func TestFileMatchesBoundedLineContinuesAfterTruncation(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	var b strings.Builder
	// Line 1: oversized, needle in the retained portion.
	b.WriteString(strings.Repeat("x", 1024*1024))
	b.WriteString("NEEDLE")
	b.WriteString(strings.Repeat("y", 5*1024*1024))
	b.WriteString("\n")
	// Line 2: normal line, also matches.
	b.WriteString("NEEDLE on a short line\n")
	path := filepath.Join(tempDir, "huge3.txt")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))

	re := regexp.MustCompile("NEEDLE")
	var got []lineMatch
	require.NoError(t, fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		got = append(got, lm)
		return true
	}))

	require.Len(t, got, 2, "must continue scanning after a truncated line")
	require.Equal(t, 1, got[0].lineNum)
	require.True(t, strings.HasSuffix(got[0].lineText, fallbackTruncateSuffix))
	require.Equal(t, 2, got[1].lineNum)
	require.Equal(t, "NEEDLE on a short line", got[1].lineText)
}

// TestSearchFilesWithRegexRespectsMidWalkCancellation verifies that cancelling
// the context while the fallback walk is in progress aborts it promptly
// (surfacing context.Canceled) instead of grinding through the whole tree. The
// searched pattern does NOT match the file content so the 200-match cap never
// triggers an early stop — the walk must traverse every file, giving
// cancellation something real to interrupt mid-flight.
func TestSearchFilesWithRegexRespectsMidWalkCancellation(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	const files = 4000
	content := strings.Repeat("filler line\n", 160) // ~1.9 KiB/file, no match for the pattern below
	for i := range files {
		p := filepath.Join(tempDir, fmt.Sprintf("d%d", i/200), fmt.Sprintf("f%d.txt", i))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	const missingPattern = "ZZQ_NO_SUCH_NEEDLE_ZZQ"

	// Warm-up: an untimed, discarded walk so the OS file cache (and any
	// on-access antivirus scan) has already touched every file once. The
	// first walk over 4000 freshly-written files can be measurably slower
	// than every subsequent one -- observed on a Windows CI runner, where
	// a cold walk took 2.1s but the very next (still-cold-baseline'd, but
	// now-warm) walk finished well under fullElapsed/4 before the
	// cancellation below ever fired, so the test raced itself into "no
	// error" instead of exercising cancellation at all. Warming the cache
	// before measuring the baseline keeps the baseline and the cancelled
	// walk under comparable conditions.
	_, warmErr := searchFilesWithRegex(context.Background(), missingPattern, tempDir, "")
	require.NoError(t, warmErr)

	// Baseline: a full, uninterrupted (now warm-cache) walk over the whole
	// tree.
	fullStart := time.Now()
	_, fullErr := searchFilesWithRegex(context.Background(), missingPattern, tempDir, "")
	fullElapsed := time.Since(fullStart)
	require.NoError(t, fullErr)
	t.Logf("full walk of %d files took %s", files, fullElapsed)

	// Cancel mid-walk: wait a quarter of the measured full-walk duration, then
	// cancel, so the walk is guaranteed to be in progress when it arrives.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := searchFilesWithRegex(ctx, missingPattern, tempDir, "")
		done <- err
	}()
	time.Sleep(fullElapsed / 4)
	cancelStart := time.Now()
	cancel()

	select {
	case err := <-done:
		abortElapsed := time.Since(cancelStart)
		require.Error(t, err)
		require.True(t, errors.Is(err, context.Canceled), "expected context.Canceled, got %v", err)
		t.Logf("mid-walk cancellation returned %s after cancel (full walk %s)", abortElapsed, fullElapsed)
		// Must abort shortly after cancellation, not grind to the end.
		require.Less(t, abortElapsed, fullElapsed/2,
			"walk should abort shortly after cancellation (abort=%s, full=%s)", abortElapsed, fullElapsed)
	case <-time.After(5 * time.Second):
		t.Fatal("searchFilesWithRegex did not return within 5s after cancellation")
	}
}

// TestFileMatchesHonoursDeadlineMidHugeLine proves the regex fallback aborts a
// single multi-MiB line (no '\n') promptly when the context deadline passes,
// rather than reading the whole line and returning nil. fileMatches checks ctx
// only between lines, so with one line that check never fires — the fix lives
// inside readBoundedLine. Timing is measured relative to a full-read baseline
// on this machine, so the assertion is machine-independent.
func TestFileMatchesHonoursDeadlineMidHugeLine(t *testing.T) {
	// Deliberately NOT t.Parallel(): this test takes two sequential live
	// wall-clock measurements and compares them to each other (not against
	// a fixed constant). Under load from sibling t.Parallel() tests, the
	// two measurements can land in different-contention moments and
	// disagree even though the deadline mechanism itself works correctly
	// -- observed flaking with the SECOND (deadline-bounded) measurement
	// coming out slower than the FIRST (full-read baseline), which should
	// be structurally impossible if both ran under comparable load.
	tempDir := t.TempDir()

	// A single line, no newline, several MiB. The pattern never matches, so
	// the entire line is scanned (capped at maxFallbackLineBytes, then bytes
	// discarded) before fileMatches can move on.
	huge := strings.Repeat("x", 16*1024*1024) // 16 MiB
	path := filepath.Join(tempDir, "huge.txt")
	require.NoError(t, os.WriteFile(path, []byte(huge), 0o644))

	re := regexp.MustCompile("ZZQ_NO_SUCH_NEEDLE_ZZQ")

	// Baseline: an uninterrupted full read of the single huge line.
	fullStart := time.Now()
	require.NoError(t, fileMatches(context.Background(), path, re, func(lineMatch) bool { return true }))
	fullElapsed := time.Since(fullStart)
	t.Logf("full read of 16 MiB single-line file took %s", fullElapsed)

	// Deadline short enough that the read must still be in progress.
	deadline := fullElapsed / 4
	if deadline < 5*time.Millisecond {
		deadline = 5 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- fileMatches(ctx, path, re, func(lineMatch) bool { return true })
	}()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"fileMatches must honour the deadline mid-line, got %v", err)
		require.Less(t, elapsed, fullElapsed,
			"fileMatches should abort near the deadline (%s), not after a full read (%s)", deadline, fullElapsed)
		t.Logf("aborted %s after start (deadline %s, full read %s)", elapsed, deadline, fullElapsed)
	case <-time.After(10 * time.Second):
		t.Fatal("fileMatches did not return within 10s")
	}
}

// slowEndlessLineReader yields an endless stream of non-newline bytes,
// sleeping a fixed interval on each Read. It models a single pathological
// line with no '\n' that takes wall-clock time to read, letting a unit test
// assert cancellation latency deterministically rather than depending on
// disk/CPU speed.
type slowEndlessLineReader struct {
	chunkInterval time.Duration
	chunk         []byte
}

func (r *slowEndlessLineReader) Read(p []byte) (int, error) {
	time.Sleep(r.chunkInterval)
	return copy(p, r.chunk), nil
}

// TestReadBoundedLineHonoursContextCancellation proves readBoundedLine reacts
// to a cancelled context mid-line (every ~64 KiB) rather than only between
// lines. Against a single pathological line with no newline — which never lets
// fileMatches' per-line check fire — the in-line cadence is the only thing
// that bounds cancellation latency.
func TestReadBoundedLineHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	r := &slowEndlessLineReader{
		chunkInterval: 2 * time.Millisecond,
		chunk:         bytes.Repeat([]byte{'x'}, 4096),
	}
	br := bufio.NewReader(r)
	var buf bytes.Buffer

	done := make(chan error, 1)
	go func() {
		_, err := readBoundedLine(ctx, br, &buf, maxFallbackLineBytes)
		done <- err
	}()

	// Let it start producing bytes, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled,
			"readBoundedLine must surface the context error once cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("readBoundedLine did not return within 2s of cancellation " +
			"(it read the whole pathological line instead of honouring ctx)")
	}
}

// TestFileMatchesDoesNotReportSpuriousMatchOnCancelledPartialLine proves that
// when readBoundedLine surfaces a non-EOF error (context cancellation fired
// mid-line by its in-line ~64 KiB cadence, see #135), fileMatches must not
// call onMatch against the partial line buffered so far — even when a real
// pattern match sits in the bytes already read. Before the fix, fileMatches
// ran pattern.FindStringIndex on the partial line before checking rerr, so a
// match in the already-read prefix of a huge, never-terminated line was
// reported right before the cancellation error was returned.
func TestFileMatchesDoesNotReportSpuriousMatchOnCancelledPartialLine(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// A single line, no newline, with a real match in its first bytes,
	// followed by filler well past readBoundedLine's ~64 KiB
	// cancellation-check cadence (see grep.go's cancellationCheckBytes).
	// readBoundedLine buffers the match into lineBuf immediately (byte 0-16
	// of the line), long before the first 64 KiB checkpoint, so an
	// ALREADY-CANCELLED context is guaranteed to be observed at that first
	// checkpoint — deterministically, with no dependency on timing/scheduler
	// load. 256 KiB of filler comfortably clears the 64 KiB checkpoint (4x
	// margin) while keeping the test file small and the run fast.
	const needle = "needle-match-XYZ"
	filler := needle + " " + strings.Repeat("x", 256*1024)
	path := filepath.Join(tempDir, "filler-with-early-match.txt")
	require.NoError(t, os.WriteFile(path, []byte(filler), 0o644))

	re := regexp.MustCompile(needle)

	// Sanity check: an uncancelled read must find the needle for real.
	var baselineCalls int
	require.NoError(t, fileMatches(context.Background(), path, re, func(lineMatch) bool {
		baselineCalls++
		return true
	}))
	require.Equal(t, 1, baselineCalls, "sanity check: the needle must be a real, findable match")

	// Cancel BEFORE calling fileMatches, rather than racing a timer against
	// the read: readBoundedLine checks ctx.Err() every 64 KiB, and the
	// needle sits in the first bytes of a line with 256 KiB of filler behind
	// it, so the already-cancelled context is guaranteed to be observed at
	// the first checkpoint — well before io.EOF — with no timing dependency.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var onMatchCalls int
	done := make(chan error, 1)
	go func() {
		done <- fileMatches(ctx, path, re, func(lineMatch) bool {
			onMatchCalls++
			return true
		})
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled,
			"fileMatches must surface the cancellation error, got %v", err)
		require.Equal(t, 0, onMatchCalls,
			"a match found only in a partial, not-fully-read line must not reach onMatch "+
				"when the read was aborted by context cancellation")
	case <-time.After(10 * time.Second):
		t.Fatal("fileMatches did not return within 10s of the cancelled context")
	}
}

// TestFileMatchesTrailingNewlineDoesNotProduceEmptyPhantomLine is a
// regression test for the opposite edge case from
// TestFileMatchesDoesNotReportSpuriousMatchOnCancelledPartialLine (#147)
// above: here the file legitimately ends with '\n', so readBoundedLine's
// final ReadByte call consumes that trailing newline and returns (lineBuf,
// nil) for the real last line — then the NEXT loop iteration immediately
// hits io.EOF with an empty lineBuf (buf.Reset() ran at the top of
// readBoundedLine, and there is nothing left to read). Before the fix, that
// empty "line" was still handed to pattern.FindStringIndex, so a pattern
// that matches the empty string (like "a*") reported a phantom match one
// line past the real end of file. The fix breaks out of the loop as soon as
// io.EOF arrives with lineBuf.Len() == 0, since that combination can only
// mean "nothing left to read", never "line complete".
func TestFileMatchesTrailingNewlineDoesNotProduceEmptyPhantomLine(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// Two real lines, both ending in '\n' - no partial trailing line.
	content := "aaa\nbbb\n"
	path := filepath.Join(tempDir, "trailing-newline.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	// "a*" matches the empty string, so it would also match a phantom empty
	// line 3 if the EOF-with-empty-buffer iteration were allowed to reach
	// FindStringIndex.
	re := regexp.MustCompile("a*")

	var lines []int
	require.NoError(t, fileMatches(t.Context(), path, re, func(lm lineMatch) bool {
		lines = append(lines, lm.lineNum)
		return true
	}))

	require.Equal(t, []int{1, 2}, lines,
		"must report exactly the two real lines, with no phantom line 3 from the "+
			"post-trailing-newline EOF iteration")
}

// TestFileMatchesEmptyFileReportsNoMatches is a related edge case: a
// completely empty file (0 bytes) must report zero matches even for a
// pattern that matches the empty string, since there are no lines at all -
// not even one. Before the fix, the single fileMatches iteration would see
// io.EOF with an empty lineBuf and still call FindStringIndex against "",
// reporting a phantom line 1.
func TestFileMatchesEmptyFileReportsNoMatches(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	path := filepath.Join(tempDir, "empty.txt")
	require.NoError(t, os.WriteFile(path, []byte{}, 0o644))

	re := regexp.MustCompile("a*")

	var calls int
	require.NoError(t, fileMatches(t.Context(), path, re, func(lineMatch) bool {
		calls++
		return true
	}))

	require.Equal(t, 0, calls, "an empty file must never produce a match, even for a pattern matching the empty string")
}
