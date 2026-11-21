package state

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
)

// Lifecycle carries the callbacks for one child selection attempt.
type Lifecycle struct {
	BindChildSession func(ctx context.Context, childSessionID string) error
	NoChildSelected  func()
}

// The coordinator aliases these key types so its readers and context masks
// see the same identities as the runtime writers.
type (
	LifecycleContextKey struct{}
	QuestionContextKey  struct{}
	QuestionBindingKey  struct{}
	BridgeInvocationKey struct{}
)

type QuestionContext struct{ Depth int }

func WithLifecycle(ctx context.Context, lifecycle Lifecycle) context.Context {
	return context.WithValue(ctx, LifecycleContextKey{}, lifecycle)
}

func WithQuestionContext(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, QuestionContextKey{}, QuestionContext{Depth: depth})
}

func QuestionDepth(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	v, _ := ctx.Value(QuestionContextKey{}).(QuestionContext)
	return v.Depth
}

func WithQuestionBinding(ctx context.Context, b agentcall.BatchAgentQuestionBinding) context.Context {
	return context.WithValue(ctx, QuestionBindingKey{}, b)
}

func QuestionBindingFrom(ctx context.Context) (agentcall.BatchAgentQuestionBinding, bool) {
	if ctx == nil {
		return agentcall.BatchAgentQuestionBinding{}, false
	}
	b, ok := ctx.Value(QuestionBindingKey{}).(agentcall.BatchAgentQuestionBinding)
	return b, ok && b.Ask != nil
}

// BridgeInvocation is the trusted enclosing invocation stamped by the agent
// driver, never derived from session state or user input.
type BridgeInvocation struct {
	RegistryID    string
	MessageID     string
	AgentDepth    int
	AgentDepthSet bool
}

func WithBridgeInvocation(ctx context.Context, inv BridgeInvocation) context.Context {
	return context.WithValue(ctx, BridgeInvocationKey{}, inv)
}

func BridgeInvocationFrom(ctx context.Context) (BridgeInvocation, bool) {
	inv, _ := ctx.Value(BridgeInvocationKey{}).(BridgeInvocation)
	if ctx == nil || inv.RegistryID == "" {
		return BridgeInvocation{}, false
	}
	return inv, true
}
