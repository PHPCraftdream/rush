package agent

import (
	"context"
	"errors"
	"fmt"
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
