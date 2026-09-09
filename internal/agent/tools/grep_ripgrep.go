package tools

// The ripgrep backend: process invocation, its JSON stream decoding and the typed errors that wrap a malformed or oversized stream. Split out of grep.go when the 1000-line file limit landed.

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

func searchWithRipgrep(ctx context.Context, pattern, path, include string, limit int) ([]grepMatch, bool, error) {
	cmd := getRgSearchCmd(ctx, pattern, path, include, 0)
	if cmd == nil {
		return nil, false, fmt.Errorf("ripgrep not found in $PATH")
	}
	appendRgIgnoreFiles(cmd, path)
	return searchWithRipgrepCommand(cmd, limit, maxRipgrepJSONLineBytes)
}

// ErrRipgrepJSONOutput identifies output that could not be consumed as a
// complete JSON stream. Callers must not silently switch search engines after
// this error: doing so can miss data beyond the parser's line limit.
var ErrRipgrepJSONOutput = errors.New("ripgrep JSON output error")

// ErrRipgrepJSONOutputTooLong identifies a JSON line larger than the parser's
// configured limit.
var ErrRipgrepJSONOutputTooLong = errors.New("ripgrep JSON output line too long")

// RipgrepJSONError preserves the scanner error after rg has been reaped.
type RipgrepJSONError struct {
	err     error
	tooLong bool
}

func (e *RipgrepJSONError) Error() string {
	if e.tooLong {
		return fmt.Sprintf("ripgrep JSON output line exceeds parser limit: %v", e.err)
	}
	return fmt.Sprintf("ripgrep JSON output could not be scanned: %v", e.err)
}

func (e *RipgrepJSONError) Unwrap() error { return e.err }

func (e *RipgrepJSONError) Is(target error) bool {
	if target == ErrRipgrepJSONOutput {
		return true
	}
	return e.tooLong && target == ErrRipgrepJSONOutputTooLong
}

func newRipgrepJSONError(err error) *RipgrepJSONError {
	return &RipgrepJSONError{
		err:     err,
		tooLong: errors.Is(err, bufio.ErrTooLong),
	}
}

func searchWithRipgrepCommand(cmd *exec.Cmd, limit, scannerMaxBytes int, statFns ...func(string) (os.FileInfo, error)) ([]grepMatch, bool, error) {

	// Stream rg's stdout line-by-line instead of buffering the entire output.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}

	stat := os.Stat
	if len(statFns) > 0 && statFns[0] != nil {
		stat = statFns[0]
	}
	capacity := max(0, limit)
	statCapacity := ripgrepStatCacheCapacity
	if capacity == 0 {
		statCapacity = 0
	}
	statCache := newBoundedStatCache(statCapacity)
	h := &boundedMatchHeap{}
	var seq int64

	scanner := bufio.NewScanner(stdout)
	// Allow long lines (minified JS etc.) — up to 4 MiB per JSON line.
	initialBufferBytes := min(64*1024, scannerMaxBytes)
	scanner.Buffer(make([]byte, 0, initialBufferBytes), scannerMaxBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var match ripgrepMatch
		if err := json.Unmarshal(line, &match); err != nil {
			continue
		}
		if match.Type != "match" || len(match.Data.Submatches) == 0 {
			continue
		}
		// Only take the first submatch per line (matches original behaviour).
		sub := match.Data.Submatches[0]

		fi, statErr := statCache.get(match.Data.Path.Text, stat)
		if statErr != nil {
			continue // Skip files we can't access.
		}

		seq++
		gm := grepMatch{
			path:     match.Data.Path.Text,
			modTime:  fi.ModTime(),
			lineNum:  match.Data.LineNumber,
			charNum:  sub.Start + 1, // ensure 1-based
			lineText: strings.TrimSpace(match.Data.Lines.Text),
			seq:      seq,
		}

		if capacity > 0 && h.Len() < capacity {
			heap.Push(h, gm)
		} else if capacity > 0 && !evictFirst(gm, (*h)[0]) {
			// gm is less evictable than the root — it deserves a spot.
			(*h)[0] = gm
			heap.Fix(h, 0)
		}
	}

	// If the scan loop stopped early due to a scanner error (most notably
	// bufio.ErrTooLong: a single JSON line — e.g. one match inside a
	// pathologically long line, a minified bundle, a base64 blob — exceeding
	// the 4 MiB buffer above) rather than reaching the pipe's natural EOF,
	// rg may still be mid-write with more output queued. Per os/exec's
	// documented contract, Wait must not be called until all reads from the
	// pipe have completed; skipping this drain lets rg block forever on a
	// full OS pipe buffer once nobody is reading it, and Wait then hangs
	// forever waiting for a process that will never exit on its own.
	// Confirmed by reproduction: a single ~6 MiB matched line reliably hung
	// Wait() indefinitely without this drain.
	scanErr := scanner.Err()
	if scanErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}

	// Wait for rg to finish and check exit code (1 = no matches, not an error).
	if waitErr := cmd.Wait(); waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// rg exit code 1 = no matches found.
		} else {
			return nil, false, waitErr
		}
	}
	if scanErr != nil {
		return nil, false, newRipgrepJSONError(scanErr)
	}

	// Extract and sort the bounded heap: newest modTime first, ties by seq asc
	// (preserves original line order within the same file/modTime).
	matches := []grepMatch(*h)
	sort.SliceStable(matches, func(i, j int) bool {
		if !matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].modTime.After(matches[j].modTime)
		}
		return matches[i].seq < matches[j].seq
	})

	return matches, (limit <= 0 && seq > 0) || (limit > 0 && seq > int64(limit)), nil
}

type ripgrepMatch struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}
