package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
)

// The batch question answer-route capability is established ONLY by a real
// answering surface, decided at trusted intake time: a served API intake
// and a parent-owned orchestrator delegation have one; the standalone
// unattended `rush batch run` CLI surface has none. The decision is carried
// durably in the agent leaf payload field `question_route`, stamped
// verbatim onto each incarnation's run context by the driver, inherited by
// nested delegations from the calling incarnation, and never derivable
// from model input, session ids, file names, config switches or an old
// pinned registry.
//
// The context marker is present EXACTLY inside batch-driven incarnations
// (both values). Its absence means a non-batch (top-level interactive or
// served) session whose own surface is the established answer route.
type batchQuestionRouteContext struct{ Route bool }

type batchQuestionRouteContextKey struct{}

func withBatchQuestionRoute(ctx context.Context, route bool) context.Context {
	return context.WithValue(ctx, batchQuestionRouteContextKey{}, batchQuestionRouteContext{Route: route})
}

// batchQuestionRouteFrom reports the route only when the marker is present;
// a missing marker means a non-batch session, not a route decision.
func batchQuestionRouteFrom(ctx context.Context) (route, ok bool) {
	if ctx == nil {
		return false, false
	}
	v, ok := ctx.Value(batchQuestionRouteContextKey{}).(batchQuestionRouteContext)
	if !ok {
		return false, false
	}
	return v.Route, true
}

// applyBatchQuestionRouteFloor is the final AllowedTools floor for batch
// incarnations without an answer route. It must run AFTER worker tool
// layering (workerToolNames re-adds ask_question) and every other per-call
// layering so an explicit model role can never restore the tool. The
// marker being absent or carrying route=true changes nothing, so every
// non-batch and route-ful build stays byte-identical.
func applyBatchQuestionRouteFloor(ctx context.Context, agent config.Agent) config.Agent {
	route, ok := batchQuestionRouteFrom(ctx)
	if !ok || route {
		return agent
	}
	kept := make([]string, 0, len(agent.AllowedTools))
	for _, name := range agent.AllowedTools {
		if name == tools.AskQuestionToolName {
			continue
		}
		kept = append(kept, name)
	}
	agent.AllowedTools = kept
	return agent
}

// stripAskQuestionTools drops ask_question from a toolset slice. It is the
// ONE strip implementation: the no-route shaping in buildBatchQuestionTools
// and runTurn's fail-closed floor for fallback toolsets both call it, so
// the two surfaces can never drift.
func stripAskQuestionTools(list []fantasy.AgentTool) []fantasy.AgentTool {
	kept := make([]fantasy.AgentTool, 0, len(list))
	for _, tool := range list {
		if tool.Info().Name == tools.AskQuestionToolName {
			continue
		}
		kept = append(kept, tool)
	}
	return kept
}

// batchChildQuestionState freezes ONE batch delegation's question
// capability onto its driver call template: the stamped route, the
// held-answer binding and the question depth. runSubAgent captures it
// from the driver-stamped run context; runTurn re-arms it onto the
// turn context of EVERY turn of the delegation (initial, wake Drain,
// interrupt replacement), because a wake arrives on a background
// callback context that carries none of them.
type batchChildQuestionState struct {
	Depth     int
	Route     bool
	Binding   batchAgentQuestionBinding
	BindingOK bool
}

// armBatchQuestionTurnContext re-arms the delegation's question
// capability onto ctx from the call's frozen state. A nil state is a
// non-batch call: nothing is stamped and the legacy contracts stand.
// Builders and durable consumers reconstruct deny-only state from delegation
// facts when the live driver is missing; ordinary roots and forks remain nil.
func armBatchQuestionTurnContext(ctx context.Context, call SessionAgentCall) context.Context {
	state := call.batchQuestion
	if state == nil {
		return ctx
	}
	ctx = withBatchAgentQuestionContext(ctx, state.Depth)
	ctx = withBatchQuestionRoute(ctx, state.Route)
	if state.BindingOK {
		ctx = withBatchAgentQuestionBinding(ctx, state.Binding)
	}
	return ctx
}
