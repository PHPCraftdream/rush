package tools

import (
	"context"
	"sync"
)

// LiveOutputBuffer is a concurrency-safe, size-bounded output sink a
// still-running tool execution registers with the work ledger (currently
// only run_command, task #1023 §3) so job_output/job_kill and the ledger's
// own timeout capturePartial can read accumulated output WHILE the process
// is still running. run_command has no BackgroundShellManager entry to read
// from otherwise (see the run_command branch of ResolveJobShellID's callers
// in job_shell_resolver.go).
type LiveOutputBuffer interface {
	// Read returns the bytes written from cursor onward, plus the new
	// cursor (== total bytes ever written). A cursor outside [0, total] is
	// treated as 0 (wake-tools-contract.md §2.1: a stale/negative cursor is
	// not an error).
	Read(cursor int64) (data string, nextCursor int64)
	// String returns everything currently resident, for a one-shot
	// best-effort read (job_kill's stopped notice, timeout's capturePartial).
	String() string
}

// runCommandLiveBufferMax bounds how much of a still-running run_command
// job's output stays resident for progressive job_output reads. Distinct
// from MaxOutputLength (the FINAL response's display truncation): this
// bounds memory for a long-running, chatty command, not display width.
const runCommandLiveBufferMax = 1 << 20 // 1 MiB

// runCommandOutputBuffer implements LiveOutputBuffer and io.Writer so it can
// be used directly as cmd.Stdout/cmd.Stderr. Oldest bytes are dropped once
// resident content would exceed maxBytes -- simpler than internal/shell's
// boundedBuffer (head+tail retention): a run_command job's FINAL output is
// separately truncated for display already (truncateOutput), so this only
// needs to serve "recent progress" reads while running, not preserve the
// command's early output verbatim.
type runCommandOutputBuffer struct {
	mu       sync.Mutex
	data     []byte
	maxBytes int
	total    int64 // monotonic count of all bytes ever written
}

func newRunCommandOutputBuffer(maxBytes int) *runCommandOutputBuffer {
	if maxBytes <= 0 {
		maxBytes = runCommandLiveBufferMax
	}
	return &runCommandOutputBuffer{maxBytes: maxBytes}
}

// Write implements io.Writer. Always reports len(p) bytes written (matching
// io.Writer semantics) even once bytes start being dropped from the front.
func (b *runCommandOutputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	b.data = append(b.data, p...)
	if excess := len(b.data) - b.maxBytes; excess > 0 {
		b.data = b.data[excess:]
	}
	return len(p), nil
}

// availableFromLocked is the earliest logical offset still resident (bytes
// before it were dropped by Write's cap). Caller must hold b.mu.
func (b *runCommandOutputBuffer) availableFromLocked() int64 {
	return b.total - int64(len(b.data))
}

func (b *runCommandOutputBuffer) Read(cursor int64) (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cursor < 0 || cursor > b.total {
		cursor = 0
	}
	start := cursor - b.availableFromLocked()
	if start < 0 {
		start = 0 // cursor points at already-dropped data; return what's left rather than erroring
	}
	if start > int64(len(b.data)) {
		start = int64(len(b.data))
	}
	return string(b.data[start:]), b.total
}

func (b *runCommandOutputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// liveOutputSinkContextKey is the context key WithLiveOutputSink installs.
type liveOutputSinkContextKey struct{}

// WithLiveOutputSink attaches a callback that a tool execution invokes with
// its LiveOutputBuffer as soon as the underlying process starts, so
// job_kill/job_output can read/stop it while still running (task #1023 §3).
// Wired by internal/agent's asyncTool.run for run_command only;
// LiveOutputSinkFromContext returns nil for every other tool/origin, and
// callers must treat a nil sink as "do not register".
func WithLiveOutputSink(ctx context.Context, sink func(LiveOutputBuffer)) context.Context {
	return context.WithValue(ctx, liveOutputSinkContextKey{}, sink)
}

// LiveOutputSinkFromContext returns the sink WithLiveOutputSink attached, or
// nil.
func LiveOutputSinkFromContext(ctx context.Context) func(LiveOutputBuffer) {
	sink, _ := ctx.Value(liveOutputSinkContextKey{}).(func(LiveOutputBuffer))
	return sink
}
