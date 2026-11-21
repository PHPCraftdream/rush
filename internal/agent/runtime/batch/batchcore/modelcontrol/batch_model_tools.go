package modelcontrol

import (
	"context"
	"encoding/json"
	"errors"

	"charm.land/fantasy"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

const OutputLimit = 16 << 10

type (
	Spec struct {
		Mode          string `json:"mode"`
		MaxAgentDepth *int   `json:"max_agent_depth,omitempty"`
		OnFail        string `json:"on_fail,omitempty"`
		Notify        string `json:"notify,omitempty"`
		MaxParallel   int    `json:"max_parallel,omitempty"`
		Items         []Item `json:"items"`
	}
	Item struct {
		Tool    string          `json:"tool,omitempty"`
		Input   json.RawMessage `json:"input,omitempty"`
		Timeout string          `json:"timeout,omitempty"`
		Batch   *Spec           `json:"batch,omitempty"`
	}
	Control[I any] interface {
		Run(context.Context, I, *toolwrap.PolicyLeafRegistry, Spec) (jobbatch.ID, error)
		Add(context.Context, string, string, string, I, *toolwrap.PolicyLeafRegistry, []Item) ([]jobbatch.ID, error)
		StatusNode(context.Context, string, string, string) (any, error)
		Output(context.Context, string, string, string, uint64) (any, error)
		Stop(context.Context, string, string, string) (any, error)
	}
)

type Params struct {
	Spec   Spec   `json:"spec,omitempty"`
	Root   string `json:"root_id,omitempty"`
	Parent string `json:"parent_id,omitempty"`
	Items  []Item `json:"items,omitempty"`
	Node   string `json:"node_id,omitempty"`
	Cursor uint64 `json:"cursor,omitempty"`
}

func WithoutPlaceholders(list []fantasy.AgentTool) []fantasy.AgentTool {
	out := list[:0]
	for _, tool := range list {
		switch tool.Info().Name {
		case "batch_run", "batch_add", "batch_status", "batch_output", "batch_stop":
			continue
		default:
			out = append(out, tool)
		}
	}
	return out
}

func NewPlaceholder(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, "Reserved batch control; requires an enabled batch runtime.", func(context.Context, Params, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextErrorResponse("batch runtime control is not installed"), nil
	})
}

func NewTools[I any](c Control[I], reg *toolwrap.PolicyLeafRegistry, atCall func(context.Context, fantasy.ToolCall) (I, string, error), workerStopScope bool) []fantasy.AgentTool {
	mk := func(n, d string, run func(context.Context, Params, fantasy.ToolCall) (any, error)) fantasy.AgentTool {
		return fantasy.NewAgentTool(n, d, func(ctx context.Context, p Params, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			v, e := run(ctx, p, call)
			if e != nil {
				return fantasy.NewTextErrorResponse(e.Error()), nil
			}
			raw, e := json.Marshal(v)
			if e != nil {
				return fantasy.NewTextErrorResponse(e.Error()), nil
			}
			return fantasy.NewTextResponse(string(raw)), nil
		})
	}
	return []fantasy.AgentTool{
		mk("batch_run", "Run explicit recursive batch spec: {mode,on_fail,notify,max_parallel,items}; item is {tool,input,timeout} or {batch:{...}}. Omitted notify defaults all; each is explicit. Tool names resolve only in this calling toolset.", func(ctx context.Context, p Params, call fantasy.ToolCall) (any, error) {
			callInv, _, err := atCall(ctx, call)
			if err != nil {
				return nil, err
			}
			return c.Run(ctx, callInv, reg, p.Spec)
		}),
		mk("batch_add", "Add items (batch_run item schema) to an owned live batch root. Added children inherit the target parent's notification mode. Optional parent_id selects a nested batch.", func(ctx context.Context, p Params, call fantasy.ToolCall) (any, error) {
			callInv, caller, err := atCall(ctx, call)
			if err != nil {
				return nil, err
			}
			return c.Add(ctx, caller, p.Root, p.Parent, callInv, reg, p.Items)
		}),
		mk("batch_status", "Read structured status for an owned root; incomplete reads remain nonterminal.", func(ctx context.Context, p Params, call fantasy.ToolCall) (any, error) {
			return c.StatusNode(ctx, tools.GetSessionFromContext(ctx), p.Root, p.Node)
		}),
		mk("batch_output", "Best-effort detached output cache (10-minute TTL; pressure eviction or process exit may end it sooner). Newest 1 MiB/task, 32 MiB/App. Read output_expired/output_truncated statuses; provide root_id,node_id,cursor and continue at next_cursor.", func(ctx context.Context, p Params, _ fantasy.ToolCall) (any, error) {
			v, err := c.Output(ctx, tools.GetSessionFromContext(ctx), p.Root, p.Node, p.Cursor)
			if err != nil {
				return nil, err
			}
			switch chunk := v.(type) {
			case jobbatch.OutputChunk:
				return OutputResponse(chunk, chunk.Next, int(chunk.Next), chunk.Truncated), nil
			case map[string]any:
				return chunk, nil
			default:
				return nil, errors.New("batch output returned invalid chunk")
			}
		}),
		mk("batch_stop", "Request durable stop of an owned root or descendant. Workers are restricted to their own batch trees; accepted is cancellation intent, not termination.", func(ctx context.Context, p Params, _ fantasy.ToolCall) (any, error) {
			if workerStopScope {
				ctx = WithWorkerStopScope(ctx)
			}
			return c.Stop(ctx, tools.GetSessionFromContext(ctx), p.Root, p.Node)
		}),
	}
}
