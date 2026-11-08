package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

type prebindClosureLeaf struct {
	fantasy.AgentTool
	run func(context.Context) (fantasy.ToolResponse, error)
}

func (l prebindClosureLeaf) Run(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return l.run(ctx)
}

type prebindClosureFactory struct{ leaf fantasy.AgentTool }

func (f prebindClosureFactory) StartAgentTurn(ctx context.Context, req batchAgentRunRequest, host batchAgentHost) (batchAgentTurn, error) {
	return startBatchAgentTurn(ctx, req, host, f.leaf, nil, func() {})
}

func TestBatchPrebindFailureSettlesScope(t *testing.T) {
	for _, kind := range []string{"denied", "error", "panic", "cancelled", "explicit_no_child", "bind_error"} {
		t.Run(kind, func(t *testing.T) {
			trace := batchAgentTraceFor(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			leaf := prebindClosureLeaf{run: func(ctx context.Context) (fantasy.ToolResponse, error) {
				trace.addf("leaf entered: %s", kind)
				lifecycle := ctx.Value(batchAgentLifecycleContextKey{}).(batchAgentLifecycle)
				switch kind {
				case "denied":
					return fantasy.NewTextErrorResponse("Tool call blocked by hook"), nil
				case "panic":
					panic("pre-bind failure")
				case "cancelled":
					return fantasy.ToolResponse{}, context.Canceled
				case "explicit_no_child":
					lifecycle.NoChildSelected()
				case "bind_error":
					return fantasy.ToolResponse{}, lifecycle.BindChildSession(ctx, "child")
				}
				return fantasy.ToolResponse{}, errors.New("pre-bind failure")
			}}
			reports := make(chan jobbatch.Result, 1)
			family := batchAgentFamily{deps: batchAgentDeps{
				Runs: prebindClosureFactory{leaf: leaf},
				// Отсутствие child намеренно остаётся incomplete в production reader.
				Descendants: batchCoordinatorDescendants{},
				Bind: func(context.Context, string, string, string, uint64, string) error {
					return errors.New("bind rejected")
				},
			}}
			execution, err := family.start(ctx, batchLeafEnv{}, jobbatch.Launch{ID: "root.1", Token: 1}, batchLeafPayload{SessionID: "owner", Tool: AgentToolName}, func(result jobbatch.Result) {
				trace.addf("report: state=%s code=%s", result.State, result.Code)
				reports <- result
			})
			require.NoError(t, err)
			e := execution.(*batchAgentExecution)
			t.Cleanup(func() {
				// Останавливаем regression-mutant watcher, не превращая incomplete в успех в product.
				e.markNoChildSelected()
				_ = e.Stop(context.Background())
			})
			select {
			case result := <-reports:
				require.Equal(t, jobbatch.StateFailed, result.State)
			case <-ctx.Done():
				t.Fatal("pre-bind failure left batch scope held after the driver exited")
			}
			require.NoError(t, e.Dispose(ctx))
			trace.addf("disposed")
			select {
			case <-reports:
				t.Fatal("duplicate terminal report")
			default:
			}
		})
	}
}

func TestBatchDriverSuccessfulBindDoesNotDeclareNoChild(t *testing.T) {
	trace := batchAgentTraceFor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var missing atomic.Int32
	leaf := prebindClosureLeaf{run: func(ctx context.Context) (fantasy.ToolResponse, error) {
		lifecycle := ctx.Value(batchAgentLifecycleContextKey{}).(batchAgentLifecycle)
		if err := lifecycle.BindChildSession(ctx, "child"); err != nil {
			return fantasy.ToolResponse{}, err
		}
		trace.addf("child bound; leaf fails afterwards")
		return fantasy.ToolResponse{}, errors.New("failure after bind")
	}}
	turn, err := startBatchAgentTurn(ctx, batchAgentRunRequest{}, batchAgentHost{
		BindChildSession: func(context.Context, string) error { return nil },
		NoChildSelected:  func() { missing.Add(1); trace.addf("no child declared") },
	}, leaf, nil, func() {})
	require.NoError(t, err)
	select {
	case <-turn.Exit():
	case <-ctx.Done():
		t.Fatal("driver failed to exit")
	}
	require.Error(t, turn.Err())
	require.Zero(t, missing.Load(), "a bound child must retain descendant closure checks even on error")
}
