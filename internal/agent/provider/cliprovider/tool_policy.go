package cliprovider

import (
	"context"
	"slices"
)

// ToolPolicy is a call-scoped capability ceiling for CLI-backed providers.
// Allowed contains canonical Rush tool names actually present in the turn's
// pruned Fantasy registry; it never grants a provider-native capability.
type ToolPolicy struct {
	Allowed     []string
	Constrained bool
}

type toolPolicyContextKey struct{}

// WithToolPolicy attaches an immutable capability snapshot to one invocation.
func WithToolPolicy(ctx context.Context, policy ToolPolicy) context.Context {
	policy.Allowed = slices.Clone(policy.Allowed)
	return context.WithValue(ctx, toolPolicyContextKey{}, policy)
}

// ToolPolicyFrom returns the invocation's trusted CLI capability ceiling.
func ToolPolicyFrom(ctx context.Context) (ToolPolicy, bool) {
	if ctx == nil {
		return ToolPolicy{}, false
	}
	policy, ok := ctx.Value(toolPolicyContextKey{}).(ToolPolicy)
	policy.Allowed = slices.Clone(policy.Allowed)
	return policy, ok
}

func toolPolicyAllows(ctx context.Context, name string) bool {
	policy, ok := ToolPolicyFrom(ctx)
	return !ok || !policy.Constrained || slices.Contains(policy.Allowed, name)
}

func policyHasMCPTool(policy ToolPolicy, name string) bool {
	return slices.Contains(policy.Allowed, name)
}
