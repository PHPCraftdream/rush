package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Batch intake limits (design 17.3 rules 3 and 4).
const (
	batchIntakeMaxNodes        = 64
	batchIntakeMaxDepth        = 3
	batchIntakeDefaultParallel = 2
	batchIntakeDefaultTimeout  = 30 * time.Minute
)

// agentToolName is the sub-agent delegation tool every intake token may use.
const agentToolName = "agent"

// batchIntakePolicy is what one token may submit. Zero fields take the defaults.
type batchIntakePolicy struct {
	// Tools are the exact extra tool names the token may submit besides the
	// default (`agent`). Wildcards are not supported on purpose.
	Tools []string
	// ReadOnly narrows the token to read-only I/O tools: agent, agentic_fetch
	// and mcp_* are refused because the read-only filter skips sub-agents.
	ReadOnly    bool
	MaxNodes    int
	MaxDepth    int
	MaxParallel int
	// MaxItemTimeout is both the default and the ceiling of every item timeout.
	MaxItemTimeout time.Duration
}

func (p batchIntakePolicy) maxNodes() int {
	if p.MaxNodes > 0 {
		return min(p.MaxNodes, batchIntakeMaxNodes)
	}
	return batchIntakeMaxNodes
}

func (p batchIntakePolicy) maxDepth() int {
	if p.MaxDepth > 0 {
		return min(p.MaxDepth, batchIntakeMaxDepth)
	}
	return batchIntakeMaxDepth
}

func (p batchIntakePolicy) maxParallel() int {
	if p.MaxParallel > 0 {
		return p.MaxParallel
	}
	return batchIntakeDefaultParallel
}

func (p batchIntakePolicy) itemTimeout() time.Duration {
	if p.MaxItemTimeout > 0 {
		return p.MaxItemTimeout
	}
	return batchIntakeDefaultTimeout
}

// toolAllowed applies the allowlist to one tool name.
func (p batchIntakePolicy) toolAllowed(name string) error {
	if name == "" {
		return errors.New("batch item has no tool")
	}
	if p.ReadOnly {
		if name == agentToolName || name == "agentic_fetch" || strings.HasPrefix(name, "mcp_") {
			return fmt.Errorf("tool %q is not available to a read-only token", name)
		}
		if !slices.Contains(agent.ReviewerReadOnlyToolNames(), name) {
			return fmt.Errorf("tool %q is not a read-only tool", name)
		}
		return nil
	}
	if name == agentToolName || slices.Contains(p.Tools, name) {
		return nil
	}
	return fmt.Errorf("tool %q is not allowed for this token", name)
}

// normalize validates spec against the policy and forces the limits that are
// not the caller's to choose: every batch node gets a parallelism cap and every
// task a bounded timeout. It must run before the spec is translated or any row
// is written, and it visits the whole tree, so a forbidden tool nested at any
// depth is refused.
func (p batchIntakePolicy) normalize(spec jobbatch.Spec) (jobbatch.Spec, error) {
	nodes := 0
	out, depth, err := p.walk(spec, 1, &nodes)
	if err != nil {
		return jobbatch.Spec{}, err
	}
	if nodes > p.maxNodes() {
		return jobbatch.Spec{}, fmt.Errorf("batch has %d nodes, limit is %d", nodes, p.maxNodes())
	}
	if depth > p.maxDepth() {
		return jobbatch.Spec{}, fmt.Errorf("batch is %d levels deep, limit is %d", depth, p.maxDepth())
	}
	return out, nil
}

func (p batchIntakePolicy) walk(spec jobbatch.Spec, level int, nodes *int) (jobbatch.Spec, int, error) {
	*nodes++
	if *nodes > p.maxNodes() {
		return jobbatch.Spec{}, 0, fmt.Errorf("batch has more than %d nodes", p.maxNodes())
	}
	if level > p.maxDepth() {
		return jobbatch.Spec{}, 0, fmt.Errorf("batch is more than %d levels deep", p.maxDepth())
	}
	switch {
	case spec.Task != nil && spec.Batch == nil:
		var task agent.BatchCLITask
		if err := json.Unmarshal(spec.Task.Payload, &task); err != nil {
			return jobbatch.Spec{}, 0, fmt.Errorf("batch task payload: %w", err)
		}
		if err := p.toolAllowed(task.Tool); err != nil {
			return jobbatch.Spec{}, 0, err
		}
		if task.Tool == agentToolName || task.Tool == "agentic_fetch" {
			if task.AgentDepth == nil {
				depth := apiBatchTrustedAgentDepth
				task.AgentDepth = &depth
			} else if *task.AgentDepth < 0 || *task.AgentDepth > apiBatchTrustedAgentDepth {
				return jobbatch.Spec{}, 0, errors.New("max_agent_depth exceeds trusted API batch cap")
			}
			if *task.AgentDepth == 0 {
				return jobbatch.Spec{}, 0, agent.ErrAgentDepthExceeded
			}
		}
		payload, err := json.Marshal(task)
		if err != nil {
			return jobbatch.Spec{}, 0, err
		}
		task2 := *spec.Task
		task2.Payload = payload
		limit := p.itemTimeout()
		switch {
		case task2.Timeout < 0:
			return jobbatch.Spec{}, 0, errors.New("item timeout must not be negative")
		case task2.Timeout == 0:
			task2.Timeout = limit
		case task2.Timeout > limit:
			return jobbatch.Spec{}, 0, fmt.Errorf("item timeout %s exceeds the token maximum %s", task2.Timeout, limit)
		}
		return jobbatch.Spec{Task: &task2}, level, nil
	case spec.Batch != nil && spec.Task == nil:
		batch := *spec.Batch
		switch {
		case batch.Mode == jobbatch.ModeSequential:
			// A sequential batch runs one child at a time; the engine refuses a larger cap.
			batch.MaxParallel = 1
		case batch.MaxParallel <= 0 || batch.MaxParallel > p.maxParallel():
			batch.MaxParallel = p.maxParallel()
		}
		batch.Items = make([]jobbatch.Spec, 0, len(spec.Batch.Items))
		deepest := level
		for _, child := range spec.Batch.Items {
			item, childDepth, err := p.walk(child, level+1, nodes)
			if err != nil {
				return jobbatch.Spec{}, 0, err
			}
			deepest = max(deepest, childDepth)
			batch.Items = append(batch.Items, item)
		}
		return jobbatch.Spec{Batch: &batch}, deepest, nil
	default:
		return jobbatch.Spec{}, 0, errors.New("batch item must be exactly one of a task or a batch")
	}
}
