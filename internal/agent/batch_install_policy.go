package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent/state"
	"github.com/PHPCraftdream/rush/internal/session"
)

// CoordinatorBatchAgentDepsWithLive builds the flagged agent-family seams
// with the R10a live descendant reader wired (P1-5b). The app install passes
// the reader over the same writer pool the batch store uses. Nil keeps the
// fail-closed incomplete read.
func CoordinatorBatchAgentDepsWithLive(live *session.BatchLiveReader) BatchAgentDeps {
	return batchAgentDeps{
		Runs:        batchCoordinatorAgentRuns{},
		Descendants: batchCoordinatorDescendants{live: live},
	}
}

// CoordinatorBatchAgentDepsWithChildScope is the flagged install seam with
// the child reaction-scope reader wired (package S, jb/psc). Poll is
// raised from the 10ms default: each poll now runs the child's full
// CLIScope read (live scope, debt, arbiter verdict), which must not run
// at 100Hz.
func CoordinatorBatchAgentDepsWithChildScope(live *session.BatchLiveReader, child BatchChildReactionFacts) BatchAgentDeps {
	return batchAgentDeps{
		Runs:        batchCoordinatorAgentRuns{},
		Descendants: batchCoordinatorDescendants{live: live, child: child},
		ChildFacts:  child,
		Poll:        state.ChildScopePollInterval,
	}
}

type (
	batchAgentLifecycle           = state.Lifecycle
	batchAgentLifecycleContextKey = state.LifecycleContextKey
	batchAgentQuestionContext     = state.QuestionContext
	batchAgentQuestionContextKey  = state.QuestionContextKey
	batchAgentQuestionBindingKey  = state.QuestionBindingKey
	batchBridgeInvocation         = state.BridgeInvocation
	batchBridgeInvocationKey      = state.BridgeInvocationKey
	BatchChildReactionFacts       = state.BatchChildReactionFacts
)

func withBatchAgentLifecycle(ctx context.Context, lifecycle batchAgentLifecycle) context.Context {
	return state.WithLifecycle(ctx, lifecycle)
}

func withBatchAgentQuestionContext(ctx context.Context, depth int) context.Context {
	return state.WithQuestionContext(ctx, depth)
}
func batchAgentQuestionDepth(ctx context.Context) int { return state.QuestionDepth(ctx) }
func withBatchAgentQuestionBinding(ctx context.Context, b batchAgentQuestionBinding) context.Context {
	return state.WithQuestionBinding(ctx, b)
}

func batchAgentQuestionBindingFrom(ctx context.Context) (batchAgentQuestionBinding, bool) {
	return state.QuestionBindingFrom(ctx)
}

func withBatchBridgeInvocation(ctx context.Context, inv batchBridgeInvocation) context.Context {
	return state.WithBridgeInvocation(ctx, inv)
}

func batchBridgeInvocationFrom(ctx context.Context) (batchBridgeInvocation, bool) {
	return state.BridgeInvocationFrom(ctx)
}

const batchArmingPolicyNote = state.ArmingPolicyNote

var errPscChildScopeUnavailable = state.ErrChildScopeUnavailable

type batchCoordinatorDescendants struct {
	live  *session.BatchLiveReader
	child BatchChildReactionFacts
}

func (d batchCoordinatorDescendants) DescendantsLive(ctx context.Context, req batchAgentRunRequest) (bool, bool) {
	return (state.Descendants{Live: d.live, Child: d.child}).DescendantsLive(ctx, req.ChildSessionID)
}

func pscCombineDescendantFacts(ctx context.Context, child BatchChildReactionFacts, childSessionID string, live, complete bool) (bool, bool) {
	return state.CombineDescendantFacts(ctx, child, childSessionID, live, complete)
}

type pscChildReactionFacts struct {
	scope   func(context.Context, string) (CLIScopeState, error)
	outcome func(context.Context, string, AsyncCompletion) (AsyncCompletion, bool)
}

func projectChildScope(st CLIScopeState) state.ChildScope {
	return state.ChildScope{WorkOpen: st.WorkOpen, DrainOwed: st.Drain == DrainOwed, DrainPaced: st.Drain == DrainPaced}
}

func pscChildScopeOpen(st CLIScopeState) bool { return projectChildScope(st).Open() }

func (f pscChildReactionFacts) ChildReactionScopeOpen(ctx context.Context, childSessionID string) (bool, bool) {
	var facts state.ReactionFacts
	if scope := f.scope; scope != nil {
		facts.Scope = func(ctx context.Context, id string) (state.ChildScope, error) {
			st, err := scope(ctx, id)
			return projectChildScope(st), err
		}
	}
	return facts.ChildReactionScopeOpen(ctx, childSessionID)
}

func (f pscChildReactionFacts) ChildFinalOutcome(ctx context.Context, childSessionID string, initial AsyncCompletion) (AsyncCompletion, bool) {
	return (state.ReactionFacts{Outcome: f.outcome}).ChildFinalOutcome(ctx, childSessionID, initial)
}
