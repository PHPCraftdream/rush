package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

const batchModelOutputLimit = 16 << 10

type (
	batchModelSpec struct {
		Mode        string           `json:"mode"`
		OnFail      string           `json:"on_fail,omitempty"`
		Notify      string           `json:"notify,omitempty"`
		MaxParallel int              `json:"max_parallel,omitempty"`
		Items       []batchModelItem `json:"items"`
	}
	batchModelItem struct {
		Tool    string          `json:"tool,omitempty"`
		Input   json.RawMessage `json:"input,omitempty"`
		Timeout string          `json:"timeout,omitempty"`
		Batch   *batchModelSpec `json:"batch,omitempty"`
	}
	batchModelControl interface {
		Run(context.Context, BatchInvocation, *policyLeafRegistry, batchModelSpec) (jobbatch.ID, error)
		Add(context.Context, string, string, string, BatchInvocation, *policyLeafRegistry, []batchModelItem) ([]jobbatch.ID, error)
		StatusNode(context.Context, string, string, string) (any, error)
		Output(context.Context, string, string, string, uint64) (any, error)
		Stop(context.Context, string, string, string) (any, error)
	}
)

type batchModelParams struct {
	Spec   batchModelSpec   `json:"spec,omitempty"`
	Root   string           `json:"root_id,omitempty"`
	Parent string           `json:"parent_id,omitempty"`
	Items  []batchModelItem `json:"items,omitempty"`
	Node   string           `json:"node_id,omitempty"`
	Cursor uint64           `json:"cursor,omitempty"`
}

func modelInvocationAtCall(ctx context.Context, inv BatchInvocation, call fantasy.ToolCall) (BatchInvocation, error) {
	inv.SessionID = tools.GetSessionFromContext(ctx)
	inv.MessageID = tools.GetMessageFromContext(ctx)
	inv.CallID = call.ID
	inv.Policy.MessageID = inv.MessageID
	inv.Policy.SupportsImages = tools.GetSupportsImagesFromContext(ctx)
	inv.Policy.ModelName = tools.GetModelNameFromContext(ctx)
	if opts := callOptionsFrom(ctx); opts != nil {
		if inv.Policy.DiskBindingID == "" && tools.IsCustomDiskProvider(opts.DiskProvider) {
			bindingID, err := EnsureCallDiskBinding(opts, batchDiskRegistryFromContext(ctx))
			if err != nil {
				return BatchInvocation{}, err
			}
			inv.Policy.DiskBindingID = bindingID
		}
		inv.Policy.DiskProvider = inv.Policy.DiskBindingID != ""
	}
	inv.Policy.FolderScope = folderScopeSpecFrom(ctx)
	inv.Policy.RunAllowlist = runAllowlistSpecFrom(ctx)
	// A template-pinned DiskBindingID propagates verbatim: nested batches
	// and Add inherit the binding, never re-resolving to another provider.
	return inv, nil
}

func withoutBatchControlPlaceholders(list []fantasy.AgentTool) []fantasy.AgentTool {
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

func newBatchControlPlaceholder(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, "Reserved batch control; requires an enabled batch runtime.", func(context.Context, batchModelParams, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextErrorResponse("batch runtime control is not installed"), nil
	})
}

func newBatchModelTools(c batchModelControl, inv BatchInvocation, reg *policyLeafRegistry) []fantasy.AgentTool {
	mk := func(n, d string, run func(context.Context, batchModelParams, fantasy.ToolCall) (any, error)) fantasy.AgentTool {
		return fantasy.NewAgentTool(n, d, func(ctx context.Context, p batchModelParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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
		mk("batch_run", "Run explicit recursive batch spec: {mode,on_fail,notify,max_parallel,items}; item is {tool,input,timeout} or {batch:{...}}. Omitted notify defaults all; each is explicit. Tool names resolve only in this calling toolset.", func(ctx context.Context, p batchModelParams, call fantasy.ToolCall) (any, error) {
			callInv, err := modelInvocationAtCall(ctx, inv, call)
			if err != nil {
				return nil, err
			}
			return c.Run(ctx, callInv, reg, p.Spec)
		}),
		mk("batch_add", "Add items (batch_run item schema) to an owned live batch root. Added children inherit the target parent's notification mode. Optional parent_id selects a nested batch.", func(ctx context.Context, p batchModelParams, call fantasy.ToolCall) (any, error) {
			callInv, err := modelInvocationAtCall(ctx, inv, call)
			if err != nil {
				return nil, err
			}
			return c.Add(ctx, callInv.SessionID, p.Root, p.Parent, callInv, reg, p.Items)
		}),
		mk("batch_status", "Read structured status for an owned root; incomplete reads remain nonterminal.", func(ctx context.Context, p batchModelParams, call fantasy.ToolCall) (any, error) {
			return c.StatusNode(ctx, tools.GetSessionFromContext(ctx), p.Root, p.Node)
		}),
		mk("batch_output", "Read retained task output capped at 16 KiB. Provide root_id,node_id,cursor; continue at next_cursor.", func(ctx context.Context, p batchModelParams, _ fantasy.ToolCall) (any, error) {
			v, err := c.Output(ctx, tools.GetSessionFromContext(ctx), p.Root, p.Node, p.Cursor)
			if err != nil {
				return nil, err
			}
			chunk, ok := v.(jobbatch.OutputChunk)
			if !ok {
				return nil, errors.New("batch output returned invalid chunk")
			}
			data := append([]byte(nil), chunk.Data...)
			return map[string]any{"data": string(data), "next_cursor": chunk.Next, "oldest_cursor": chunk.Oldest, "eof": chunk.EOF, "truncated": chunk.Truncated}, nil
		}),
		mk("batch_stop", "Request durable stop of an owned root or descendant. Accepted is cancellation intent, not termination.", func(ctx context.Context, p batchModelParams, _ fantasy.ToolCall) (any, error) {
			return c.Stop(ctx, tools.GetSessionFromContext(ctx), p.Root, p.Node)
		}),
	}
}

func modelBatchSpec(raw batchModelSpec, inv BatchInvocation, reg *policyLeafRegistry) (jobbatch.Spec, error) {
	return modelBatchSpecAt(raw, inv, reg, "")
}

func modelBatchSpecAt(raw batchModelSpec, inv BatchInvocation, reg *policyLeafRegistry, path string) (jobbatch.Spec, error) {
	if reg == nil {
		return jobbatch.Spec{}, errors.New("calling pinned tool registry is unavailable")
	}
	mode := jobbatch.Mode(raw.Mode)
	if mode != "parallel" && mode != "sequential" {
		return jobbatch.Spec{}, errors.New("mode must be parallel or sequential")
	}
	onFail := jobbatch.OnFail(raw.OnFail)
	if mode == jobbatch.ModeSequential {
		if onFail == "" {
			onFail = jobbatch.OnFailStop
		}
		if onFail != jobbatch.OnFailStop && onFail != jobbatch.OnFailContinue {
			return jobbatch.Spec{}, errors.New("on_fail must be stop or continue")
		}
	} else if onFail != "" {
		return jobbatch.Spec{}, errors.New("on_fail is only valid for sequential batches")
	}
	notify := jobbatch.Notify(raw.Notify)
	if notify == "" {
		notify = jobbatch.NotifyAll
	}
	if notify != "all" && notify != "each" {
		return jobbatch.Spec{}, errors.New("notify must be all or each")
	}
	if raw.MaxParallel < 0 {
		return jobbatch.Spec{}, errors.New("max_parallel must not be negative")
	}
	if len(raw.Items) == 0 {
		return jobbatch.Spec{}, errors.New("items must not be empty")
	}
	items := make([]jobbatch.Spec, 0, len(raw.Items))
	for index, it := range raw.Items {
		if it.Tool == "" && it.Batch == nil {
			return jobbatch.Spec{}, errors.New("item must contain a task or nested batch")
		}
		if (it.Batch != nil) == (it.Tool != "") {
			return jobbatch.Spec{}, errors.New("each item must contain exactly one of tool or batch")
		}
		if it.Batch != nil {
			nestedPath := fmt.Sprintf("%s%d.", path, index)
			nested, e := modelBatchSpecAt(*it.Batch, inv, reg, nestedPath)
			if e != nil {
				return jobbatch.Spec{}, e
			}
			items = append(items, nested)
			continue
		}
		if batchControlTarget(it.Tool) {
			return jobbatch.Spec{}, fmt.Errorf("control-plane tool %q cannot be a batch leaf", it.Tool)
		}
		leaf, e := reg.resolve(it.Tool)
		if e != nil || leaf == nil {
			return jobbatch.Spec{}, fmt.Errorf("tool %q unavailable in calling pinned toolset", it.Tool)
		}
		input, e := batchCanonicalInput(string(it.Input))
		if e != nil {
			return jobbatch.Spec{}, fmt.Errorf("tool %q input: %w", it.Tool, e)
		}
		payload, e := json.Marshal(batchLeafPayload{Tool: it.Tool, ToolCallID: fmt.Sprintf("%s-item-%s%d", inv.CallID, path, index), SessionID: inv.SessionID, Input: input, RegistryID: inv.RegistryID, Policy: &inv.Policy})
		if e != nil {
			return jobbatch.Spec{}, e
		}
		timeout := time.Duration(0)
		if it.Timeout != "" {
			timeout, e = time.ParseDuration(it.Timeout)
			if e != nil || timeout <= 0 {
				return jobbatch.Spec{}, errors.New("timeout must be a positive duration")
			}
		}
		items = append(items, jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchLeafKind(it.Tool), Payload: payload, Timeout: timeout}})
	}
	if depth, nodes, _, err := batchSpecNodes([]jobbatch.Spec{{Batch: &jobbatch.BatchSpec{Items: items}}}); err != nil {
		return jobbatch.Spec{}, err
	} else if depth > batchDefaultMaxDepth || nodes > batchDefaultMaxNodes {
		return jobbatch.Spec{}, errors.New("batch spec exceeds depth or node budget")
	}
	return jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: mode, OnFail: onFail, Notify: notify, MaxParallel: raw.MaxParallel, Items: items}}, nil
}
