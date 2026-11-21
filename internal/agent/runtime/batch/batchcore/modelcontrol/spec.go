package modelcontrol

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type SpecDeps struct {
	HasRegistry        bool
	MaxDepth, MaxNodes int
	Measure            func([]jobbatch.Spec) (depth, nodes int, err error)
	BuildLeaf          func(item Item, path string, agentDepth int) (*jobbatch.TaskSpec, error)
}

func BuildSpec(raw Spec, inherited int, deps SpecDeps) (jobbatch.Spec, error) {
	if inherited < 0 || inherited > 32 {
		return jobbatch.Spec{}, errors.New("agent depth must be between 0 and 32")
	}
	return buildSpecAt(raw, inherited, "", deps)
}

func buildSpecAt(raw Spec, inherited int, path string, deps SpecDeps) (jobbatch.Spec, error) {
	if raw.MaxAgentDepth != nil {
		if *raw.MaxAgentDepth < 0 || *raw.MaxAgentDepth > inherited {
			return jobbatch.Spec{}, errors.New("nested max_agent_depth may only narrow the inherited cap")
		}
		if path == "" {
			inherited = *raw.MaxAgentDepth
		}
	}
	if !deps.HasRegistry {
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
		childPath := fmt.Sprintf("%s%d.", path, index)
		if it.Batch != nil {
			nestedCap := inherited
			if it.Batch.MaxAgentDepth != nil {
				if *it.Batch.MaxAgentDepth < 0 || *it.Batch.MaxAgentDepth > nestedCap {
					return jobbatch.Spec{}, errors.New("nested max_agent_depth may only narrow the inherited cap")
				}
				nestedCap = *it.Batch.MaxAgentDepth
			}
			nested, e := buildSpecAt(*it.Batch, nestedCap, childPath, deps)
			if e != nil {
				return jobbatch.Spec{}, e
			}
			items = append(items, nested)
			continue
		}
		cap := inherited
		if raw.MaxAgentDepth != nil {
			cap = *raw.MaxAgentDepth
		}
		task, err := deps.BuildLeaf(it, fmt.Sprintf("%s%d", path, index), cap)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		items = append(items, jobbatch.Spec{Task: task})
	}
	if depth, nodes, err := deps.Measure([]jobbatch.Spec{{Batch: &jobbatch.BatchSpec{Items: items}}}); err != nil {
		return jobbatch.Spec{}, err
	} else if depth > deps.MaxDepth || nodes > deps.MaxNodes {
		return jobbatch.Spec{}, errors.New("batch spec exceeds depth or node budget")
	}
	return jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: mode, OnFail: onFail, Notify: notify, MaxParallel: raw.MaxParallel, Items: items}}, nil
}

// RewriteLeaf preserves the host payload codec while assigning trusted call IDs.
// A non-nil maxDepth additionally checks the cap for a top-level Add leaf.
type RewriteLeaf func(task *jobbatch.TaskSpec, callID string, maxDepth *int) error

func assignModelBatchCallIDs(spec *jobbatch.Spec, base, path string, rewrite RewriteLeaf) error {
	if spec == nil {
		return errors.New("nil batch spec")
	}
	if spec.Task != nil {
		return rewrite(spec.Task, fmt.Sprintf("%s-%s", base, path), nil)
	}
	if spec.Batch != nil {
		for i := range spec.Batch.Items {
			if err := assignModelBatchCallIDs(&spec.Batch.Items[i], base, fmt.Sprintf("%s-%d", path, i), rewrite); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("invalid empty batch spec")
}

func BuildAddNodes(items []Item, trustedDepth int, callID string, build func(Spec, int) (jobbatch.Spec, error), rewrite RewriteLeaf) ([]jobbatch.Spec, error) {
	out := make([]jobbatch.Spec, 0, len(items))
	for i, item := range items {
		var spec jobbatch.Spec
		var err error
		if item.Batch != nil {
			depth := trustedDepth
			if cap := item.Batch.MaxAgentDepth; cap != nil {
				if *cap < 0 || *cap > trustedDepth {
					return nil, errors.New("batch_add max_agent_depth exceeds trusted caller cap")
				}
				depth = *cap
			}
			spec, err = build(*item.Batch, depth)
		} else {
			var wrapper jobbatch.Spec
			wrapper, err = build(Spec{Mode: "parallel", Notify: "each", Items: []Item{item}}, trustedDepth)
			if err == nil {
				spec = wrapper.Batch.Items[0]
			}
		}
		if err != nil {
			return nil, err
		}
		if spec.Task != nil {
			if err := rewrite(spec.Task, fmt.Sprintf("%s-add-%d", callID, i), &trustedDepth); err != nil {
				return nil, err
			}
		}
		out = append(out, spec)
	}
	return out, nil
}

func modelSpecHasCustomRole(spec Spec) bool {
	for _, item := range spec.Items {
		if itemHasCustomRole(item) {
			return true
		}
	}
	return false
}

func itemHasCustomRole(item Item) bool {
	if item.Batch != nil {
		for _, child := range item.Batch.Items {
			if itemHasCustomRole(child) {
				return true
			}
		}
		return false
	}
	if item.Tool != "agent" {
		return false
	}
	var input struct {
		Role agentcall.RequestedModelRole `json:"role"`
	}
	if json.Unmarshal(item.Input, &input) != nil {
		return false
	}
	return input.Role != "" && !input.Role.Builtin()
}
