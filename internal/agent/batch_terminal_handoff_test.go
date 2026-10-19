package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

type terminalHandoffLeaf struct{ content string }

func (l terminalHandoffLeaf) Info() fantasy.ToolInfo                     { return fantasy.ToolInfo{Name: "view"} }
func (l terminalHandoffLeaf) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (l terminalHandoffLeaf) SetProviderOptions(fantasy.ProviderOptions) {}
func (l terminalHandoffLeaf) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse(l.content), nil
}

type terminalHandoffArgv struct {
	done     chan struct{}
	output   []byte
	disposed bool
}

func (p *terminalHandoffArgv) Done() <-chan struct{}             { return p.done }
func (*terminalHandoffArgv) Outcome() (tools.ArgvOutcome, error) { return tools.ArgvOutcome{}, nil }
func (*terminalHandoffArgv) Stop(context.Context) error          { return nil }
func (p *terminalHandoffArgv) ReadOutput(_ context.Context, _ int64) (jobbatch.OutputChunk, error) {
	return jobbatch.OutputChunk{Data: append([]byte(nil), p.output...), Next: uint64(len(p.output)), EOF: true}, nil
}

func (p *terminalHandoffArgv) Dispose(context.Context) error {
	p.disposed = true
	p.output = nil
	return nil
}

type terminalHandoffShell struct {
	done     chan struct{}
	output   []byte
	disposed bool
}

func (*terminalHandoffShell) Stop(context.Context) error               { return nil }
func (h *terminalHandoffShell) Done() <-chan struct{}                  { return h.done }
func (*terminalHandoffShell) GetOutput() (string, string, bool, error) { return "", "", false, nil }
func (h *terminalHandoffShell) ReadOutput(context.Context, uint64) (shell.ExecutionOutput, error) {
	return shell.ExecutionOutput{Data: append([]byte(nil), h.output...), Next: uint64(len(h.output)), EOF: true}, nil
}

func (h *terminalHandoffShell) Dispose(context.Context) error {
	h.disposed = true
	h.output = nil
	return nil
}

func assertHandoff(t *testing.T, cache *batchOutputCache, id, root jobbatch.ID, want string) {
	t.Helper()
	chunk, ok := cache.read(id, 0)
	if !ok || string(chunk.Data) != want {
		t.Fatalf("cache handoff for %s: got %q, %v; want %q", id, chunk.Data, ok, want)
	}
	cache.mu.Lock()
	entry := cache.entries[id]
	cache.mu.Unlock()
	if entry.rootID != string(root) {
		t.Fatalf("root metadata %q, want %q", entry.rootID, root)
	}
}

func TestBatchTerminalOutputHandoffBeforeReport(t *testing.T) {
	ctx := context.Background()
	cache := newBatchOutputCache()
	var now atomic.Int64
	now.Store(100)
	cache.now = func() time.Time { return time.Unix(now.Load(), 0) }
	cases := []struct {
		name string
		id   jobbatch.ID
		want string
		run  func(*testing.T, func(jobbatch.Result))
	}{
		{
			name: "generic", id: "root.generic", want: "generic-result",
			run: func(t *testing.T, report func(jobbatch.Result)) {
				e := &batchGenericExecution{cache: cache, nodeID: "root.generic", rootID: batchOutputRoot("root.generic"), cancel: func() {}, done: make(chan struct{})}
				e.run(ctx, terminalHandoffLeaf{content: "generic-result"}, fantasy.ToolCall{}, func(r jobbatch.Result) {
					assertHandoff(t, cache, "root.generic", "root", "generic-result")
					require.NoError(t, e.Dispose(ctx))
					report(r)
				})
			},
		},
		{
			name: "agent", id: "root.agent", want: "agent-summary",
			run: func(t *testing.T, report func(jobbatch.Result)) {
				e := &batchAgentExecution{cache: cache, nodeID: "root.agent", rootID: batchOutputRoot("root.agent"), finished: make(chan struct{}), driverDone: make(chan struct{}), watcherDone: make(chan struct{}), cancel: func() {}}
				e.scope = newBatchAgentScope()
				close(e.driverDone)
				close(e.watcherDone)
				e.finish(jobbatch.Result{State: jobbatch.StateCompleted, Summary: "agent-summary"}, func(r jobbatch.Result) {
					assertHandoff(t, cache, "root.agent", "root", "agent-summary")
					require.NoError(t, e.Dispose(ctx))
					report(r)
				})
			},
		},
		{
			name: "shell", id: "root.shell", want: "shell-bytes",
			run: func(t *testing.T, report func(jobbatch.Result)) {
				h := &terminalHandoffShell{done: make(chan struct{}), output: []byte("shell-bytes")}
				close(h.done)
				e := &batchShellExecution{cache: cache, nodeID: "root.shell", rootID: batchOutputRoot("root.shell"), handle: h, cancel: func() {}, finished: make(chan struct{})}
				e.run(ctx, terminalHandoffLeaf{content: "response-not-process-output"}, fantasy.ToolCall{}, func(r jobbatch.Result) {
					assertHandoff(t, cache, "root.shell", "root", "shell-bytes")
					require.NoError(t, e.Dispose(ctx))
					if len(h.output) != 0 {
						t.Fatal("shell Dispose did not clear output")
					}
					report(r)
				})
			},
		},
		{
			name: "argv", id: "root.argv", want: "argv-bytes",
			run: func(t *testing.T, report func(jobbatch.Result)) {
				p := &terminalHandoffArgv{done: make(chan struct{}), output: []byte("argv-bytes")}
				close(p.done)
				e := &batchArgvExecution{cache: cache, nodeID: "root.argv", rootID: batchOutputRoot("root.argv"), process: p, changed: make(chan struct{}), finished: make(chan struct{}), cancel: func() {}}
				leaf := &batchGenericTestTool{name: tools.RunCommandToolName, run: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					return fantasy.NewTextResponse("response-not-process-output"), nil
				}}
				e.run(ctx, leaf, fantasy.ToolCall{}, func(r jobbatch.Result) {
					assertHandoff(t, cache, "root.argv", "root", "argv-bytes")
					require.NoError(t, e.Dispose(ctx))
					if len(p.output) != 0 {
						t.Fatal("argv Dispose did not clear output")
					}
					report(r)
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reports := make(chan jobbatch.Result, 1)
			tc.run(t, func(r jobbatch.Result) { reports <- r })
			select {
			case <-reports:
			case <-time.After(time.Second):
				t.Fatal("terminal report not delivered")
			}
			assertHandoff(t, cache, tc.id, batchOutputRoot(tc.id), tc.want)
		})
	}
}

// captureShellHandle mirrors the real shell live contract
// (internal/shell BackgroundExecution.ReadOutput on a finished stream with a
// complete buffer, oldest = 0): start = max(cursor, 0) clamped to next,
// Next = next, Truncated = cursor < 0 (always false here), EOF = done.
type captureShellHandle struct {
	done     chan struct{}
	content  []byte
	disposed bool
}

func (h *captureShellHandle) Stop(context.Context) error             { return nil }
func (h *captureShellHandle) Done() <-chan struct{}                  { return h.done }
func (*captureShellHandle) GetOutput() (string, string, bool, error) { return "", "", false, nil }
func (h *captureShellHandle) ReadOutput(_ context.Context, cursor uint64) (shell.ExecutionOutput, error) {
	next := uint64(len(h.content))
	start := cursor
	if start > next {
		start = next
	}
	return shell.ExecutionOutput{Data: append([]byte(nil), h.content[start:]...), Next: next, EOF: true}, nil
}

func (h *captureShellHandle) Dispose(context.Context) error {
	h.disposed = true
	h.content = nil
	return nil
}

// captureArgvHandle mirrors the real argv live contract
// (tools.ArgvProcess.ReadOutput on a finished process with a complete
// buffer, oldest = 0): negative or over-total cursors restart at zero,
// Next = total, EOF = done, Truncated = cursor < 0 (always false here).
type captureArgvHandle struct {
	done     chan struct{}
	content  []byte
	disposed bool
}

func (p *captureArgvHandle) Done() <-chan struct{}             { return p.done }
func (*captureArgvHandle) Outcome() (tools.ArgvOutcome, error) { return tools.ArgvOutcome{}, nil }
func (*captureArgvHandle) Stop(context.Context) error          { return nil }
func (p *captureArgvHandle) ReadOutput(_ context.Context, cursor int64) (jobbatch.OutputChunk, error) {
	total := uint64(len(p.content))
	if cursor < 0 || cursor > int64(total) {
		cursor = 0
	}
	return jobbatch.OutputChunk{Data: append([]byte(nil), p.content[cursor:]...), Next: total, EOF: true}, nil
}

func (p *captureArgvHandle) Dispose(context.Context) error {
	p.disposed = true
	p.content = nil
	return nil
}

// batchParityCursors derives the parity cursor set for retained content of
// length l: boundaries inside and just outside the live range plus the extreme
// cursors that exercise each family's clamp/restart contract. For l < 2 the
// near-range cursors (1, l/2, l-1, l+1) are skipped; l-1 would underflow at 0.
func batchParityCursors(l uint64) []uint64 {
	if l < 2 {
		return []uint64{0, math.MaxInt64, uint64(math.MaxInt64) + 1, math.MaxUint64 - 1, ^uint64(0)}
	}
	return []uint64{0, 1, l / 2, l - 1, l, l + 1, math.MaxInt64, uint64(math.MaxInt64) + 1, math.MaxUint64 - 1, ^uint64(0)}
}

func TestBatchTerminalCaptureLiveCacheParity(t *testing.T) {
	ctx := context.Background()
	cache := newBatchOutputCache()
	cases := []struct {
		name    string
		id      jobbatch.ID
		cursors []uint64
		run     func(*testing.T, func(jobbatch.Result)) map[uint64]jobbatch.OutputChunk
	}{
		{
			name: "generic", id: "cap.generic", cursors: batchParityCursors(14),
			run: func(t *testing.T, report func(jobbatch.Result)) map[uint64]jobbatch.OutputChunk {
				e := &batchGenericExecution{cache: cache, nodeID: "cap.generic", rootID: batchOutputRoot("cap.generic"), cancel: func() {}, done: make(chan struct{})}
				cursors := batchParityCursors(14)
				chunks := map[uint64]jobbatch.OutputChunk{}
				e.run(ctx, terminalHandoffLeaf{content: "generic-result"}, fantasy.ToolCall{}, func(r jobbatch.Result) {
					for _, c := range cursors {
						chunk, err := e.ReadOutput(ctx, c)
						require.NoError(t, err)
						chunks[c] = chunk
					}
					require.NoError(t, e.Dispose(ctx))
					report(r)
				})
				return chunks
			},
		},
		{
			name: "agent", id: "cap.agent", cursors: batchParityCursors(13),
			run: func(t *testing.T, report func(jobbatch.Result)) map[uint64]jobbatch.OutputChunk {
				e := &batchAgentExecution{cache: cache, nodeID: "cap.agent", rootID: batchOutputRoot("cap.agent"), finished: make(chan struct{}), driverDone: make(chan struct{}), watcherDone: make(chan struct{}), cancel: func() {}}
				e.scope = newBatchAgentScope()
				close(e.driverDone)
				close(e.watcherDone)
				cursors := batchParityCursors(13)
				chunks := map[uint64]jobbatch.OutputChunk{}
				e.finish(jobbatch.Result{State: jobbatch.StateCompleted, Summary: "agent-summary"}, func(r jobbatch.Result) {
					for _, c := range cursors {
						chunk, err := e.ReadOutput(ctx, c)
						require.NoError(t, err)
						chunks[c] = chunk
					}
					require.NoError(t, e.Dispose(ctx))
					report(r)
				})
				return chunks
			},
		},
		{
			name: "shell", id: "cap.shell", cursors: batchParityCursors(11),
			run: func(t *testing.T, report func(jobbatch.Result)) map[uint64]jobbatch.OutputChunk {
				h := &captureShellHandle{done: make(chan struct{}), content: []byte("shell-bytes")}
				close(h.done)
				e := &batchShellExecution{cache: cache, nodeID: "cap.shell", rootID: batchOutputRoot("cap.shell"), handle: h, cancel: func() {}, finished: make(chan struct{})}
				cursors := batchParityCursors(11)
				chunks := map[uint64]jobbatch.OutputChunk{}
				e.run(ctx, terminalHandoffLeaf{content: "response-not-process-output"}, fantasy.ToolCall{}, func(r jobbatch.Result) {
					for _, c := range cursors {
						chunk, err := e.ReadOutput(ctx, c)
						require.NoError(t, err)
						chunks[c] = chunk
					}
					require.NoError(t, e.Dispose(ctx))
					report(r)
				})
				return chunks
			},
		},
		{
			name: "argv", id: "cap.argv", cursors: batchParityCursors(10),
			run: func(t *testing.T, report func(jobbatch.Result)) map[uint64]jobbatch.OutputChunk {
				p := &captureArgvHandle{done: make(chan struct{}), content: []byte("argv-bytes")}
				close(p.done)
				e := &batchArgvExecution{cache: cache, nodeID: "cap.argv", rootID: batchOutputRoot("cap.argv"), process: p, changed: make(chan struct{}), finished: make(chan struct{}), cancel: func() {}}
				leaf := &batchGenericTestTool{name: tools.RunCommandToolName, run: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					return fantasy.NewTextResponse("response-not-process-output"), nil
				}}
				cursors := batchParityCursors(10)
				chunks := map[uint64]jobbatch.OutputChunk{}
				e.run(ctx, leaf, fantasy.ToolCall{}, func(r jobbatch.Result) {
					for _, c := range cursors {
						chunk, err := e.ReadOutput(ctx, c)
						require.NoError(t, err)
						chunks[c] = chunk
					}
					require.NoError(t, e.Dispose(ctx))
					report(r)
				})
				return chunks
			},
		},
		{
			// This row catches re-caching the rejection text (the leaf's
			// resp.Content) — the live-vs-cached data divergence at cursor 0 —
			// NOT a contract-constant mutation: with no process both contracts
			// return an empty page at Next 0 for this empty entry. Argv
			// contract-mutation coverage comes from the process-backed argv
			// row above.
			name: "argv-no-process", id: "cap.argvnopro", cursors: batchParityCursors(0),
			run: func(t *testing.T, report func(jobbatch.Result)) map[uint64]jobbatch.OutputChunk {
				e := &batchArgvExecution{cache: cache, nodeID: "cap.argvnopro", rootID: batchOutputRoot("cap.argvnopro"), changed: make(chan struct{}), finished: make(chan struct{}), cancel: func() {}}
				leaf := &batchGenericTestTool{name: tools.RunCommandToolName, run: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					return fantasy.NewTextResponse("rejected-content"), nil
				}}
				cursors := batchParityCursors(0)
				chunks := map[uint64]jobbatch.OutputChunk{}
				e.run(ctx, leaf, fantasy.ToolCall{}, func(r jobbatch.Result) {
					for _, c := range cursors {
						chunk, err := e.ReadOutput(ctx, c)
						require.NoError(t, err)
						chunks[c] = chunk
					}
					require.NoError(t, e.Dispose(ctx))
					report(r)
				})
				return chunks
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reports := make(chan jobbatch.Result, 1)
			chunks := tc.run(t, func(r jobbatch.Result) { reports <- r })
			select {
			case <-reports:
			case <-time.After(time.Second):
				t.Fatal("terminal report not delivered")
			}
			for _, cursor := range tc.cursors {
				got, has := chunks[cursor]
				if !has {
					t.Fatalf("missing live chunk at cursor %d", cursor)
				}
				cached, found := cache.read(tc.id, cursor)
				if !found {
					t.Fatalf("%s: cache miss at cursor %d", tc.id, cursor)
				}
				if string(got.Data) != string(cached.Data) || got.Next != cached.Next || got.EOF != cached.EOF || got.Oldest != cached.Oldest || got.Truncated != cached.Truncated {
					t.Fatalf("%s: cursor %d live=(data %q, next %d, eof %v, oldest %d, truncated %v) cached=(data %q, next %d, eof %v, oldest %d, truncated %v)",
						tc.id, cursor, got.Data, got.Next, got.EOF, got.Oldest, got.Truncated, cached.Data, cached.Next, cached.EOF, cached.Oldest, cached.Truncated)
				}
			}
		})
	}
}

func TestBatchOutputExpiredMarkerAfterRealRelease(t *testing.T) {
	env := newBatchInventoryEnv(t, "view")
	var seconds atomic.Int64
	seconds.Store(time.Now().Unix())
	env.install.OutputCache.now = func() time.Time { return time.Unix(seconds.Load(), 0) }
	result, err := env.invoke("terminal-cache", "view")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("invoke failed: %s", result.Content)
	}
	var root string
	if err := env.conn.QueryRowContext(env.ctx, `SELECT root_id FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=? AND node_id=root_id`, env.sessionID, "terminal-cache").Scan(&root); err != nil {
		t.Fatal(err)
	}
	handle := env.install.RunnerHandle
	batch := handle.batchFor(jobbatch.ID(root))
	if batch == nil {
		t.Fatal("submitted batch not retained")
	}
	if _, err := batch.Wait(env.ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.install.Runner.Release(env.ctx, jobbatch.ID(root)); err != nil && !errors.Is(err, jobbatch.ErrReleased) {
		t.Fatal(err)
	}
	_, err = env.install.Runner.Output(env.ctx, jobbatch.ID(root)+".1", 0)
	if !errors.Is(err, jobbatch.ErrReleased) {
		t.Fatalf("runner output after release: %v", err)
	}
	control := NewInstalledBatchModelControl(handle, env.install.Delivery, env.store, nil, batchTestSessions(env.sessionID), env.install.Runtime.cfg.Host, env.install.Runtime.cfg.RunnerID, nil, nil).(*installedBatchModelControl)
	live, err := control.Output(env.ctx, env.sessionID, root, root+".1", 0)
	if err != nil {
		t.Fatal(err)
	}
	chunk, ok := live.(map[string]any)
	if !ok || chunk["data"] != "ok" {
		t.Fatalf("released output handoff: %#v", live)
	}
	seconds.Store(seconds.Load() + int64(batchOutputTTL/time.Second))
	expired, err := control.Output(env.ctx, env.sessionID, root, root+".1", 0)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := expired.(map[string]any)
	if !ok || fmt.Sprint(meta["output_status"]) != "output_expired" {
		t.Fatalf("expiry marker: %#v", expired)
	}
}
