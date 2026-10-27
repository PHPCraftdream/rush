package agent

import (
	"context"
	"slices"
	"sync"

	"charm.land/fantasy"
)

// Rush relies on a tool's non-nil Go error ending the turn and coming back
// from agent.Stream (ask_question, peak-hours and cancellation paths, every
// "fatal" tool failure). fantasy v0.45.3+ instead turns that error into an
// ordinary error result and keeps the loop going. fatalToolTool restores the
// contract at the one boundary Rush owns: the error is recorded, the step
// stops the turn via StopTurn, and runTurn returns the recorded error.

// fatalToolCollector keeps the first fatal tool error of one turn.
type fatalToolCollector struct {
	mu  sync.Mutex
	err error
}

func (c *fatalToolCollector) record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

// take returns and clears the recorded error, so a stall-retried attempt
// starts clean.
func (c *fatalToolCollector) take() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.err
	c.err = nil
	return err
}

type fatalToolTool struct {
	inner     fantasy.AgentTool
	collector *fatalToolCollector
}

func (t *fatalToolTool) Info() fantasy.ToolInfo { return t.inner.Info() }

func (t *fatalToolTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	resp, err := t.inner.Run(ctx, call)
	if err == nil {
		return resp, nil
	}
	t.collector.record(err)
	stop := fantasy.NewTextErrorResponse(err.Error())
	stop.Metadata = resp.Metadata
	stop.StopTurn = true
	return stop, nil
}

func (t *fatalToolTool) ProviderOptions() fantasy.ProviderOptions { return t.inner.ProviderOptions() }

func (t *fatalToolTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

// withFatalToolErrors returns a copy of tools whose Go errors end the turn
// and are recorded in c. The shared tool objects are never mutated.
func withFatalToolErrors(tools []fantasy.AgentTool, c *fatalToolCollector) []fantasy.AgentTool {
	if len(tools) == 0 || c == nil {
		return tools
	}
	out := slices.Clone(tools)
	for i, tool := range out {
		out[i] = &fatalToolTool{inner: tool, collector: c}
	}
	return out
}
