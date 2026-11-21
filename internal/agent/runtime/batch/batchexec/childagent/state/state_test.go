package state

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
)

func TestContextIdentityAndValueSnapshots(t *testing.T) {
	inv := BridgeInvocation{RegistryID: "pinned", MessageID: "m", AgentDepthSet: true}
	ctx := WithBridgeInvocation(context.Background(), inv)
	inv.RegistryID = "other"
	ctx = WithQuestionContext(ctx, 4)
	selected := false
	lifecycle := Lifecycle{NoChildSelected: func() { selected = true }}
	ctx = WithLifecycle(ctx, lifecycle)
	lifecycle.NoChildSelected = nil
	ctx.Value(LifecycleContextKey{}).(Lifecycle).NoChildSelected()
	got, ok := BridgeInvocationFrom(ctx)
	if !selected || !ok || got.RegistryID != "pinned" || got.MessageID != "m" || !got.AgentDepthSet || got.AgentDepth != 0 || QuestionDepth(ctx) != 4 {
		t.Fatalf("lifecycle=%v invocation=%+v valid=%v depth=%d", selected, got, ok, QuestionDepth(ctx))
	}
	if got, ok := BridgeInvocationFrom(context.Background()); ok || got != (BridgeInvocation{}) {
		t.Fatalf("unstamped context returned invocation=%+v valid=%v", got, ok)
	}
	invalid := WithBridgeInvocation(context.Background(), BridgeInvocation{MessageID: "m"})
	if got, ok := BridgeInvocationFrom(invalid); ok || got != (BridgeInvocation{}) {
		t.Fatalf("empty registry returned invocation=%+v valid=%v", got, ok)
	}
}

func TestReactionFactsReadLateValues(t *testing.T) {
	ctx := context.Background()
	var scope ChildScope
	ready := false
	facts := ReactionFacts{
		Scope: func(context.Context, string) (ChildScope, error) { return scope, nil },
		Outcome: func(_ context.Context, id string, initial batchdispatch.AsyncCompletion) (batchdispatch.AsyncCompletion, bool) {
			return initial, ready && id == "child"
		},
	}
	if open, complete := CombineDescendantFacts(ctx, facts, "child", false, true); open || !complete {
		t.Fatalf("initial read = %v, %v", open, complete)
	}
	scope.DrainPaced, ready = true, true
	if open, complete := CombineDescendantFacts(ctx, facts, "child", false, false); !open || complete {
		t.Fatalf("late read must widen liveness without repairing incompleteness: %v, %v", open, complete)
	}
	if _, ok := facts.ChildFinalOutcome(ctx, "child", batchdispatch.AsyncCompletion{}); !ok {
		t.Fatal("late final outcome was not read")
	}
}
