package agent

import "context"

type globalQuestionContext struct{ context.Context }

// Global model rebuilds must not capture one delegation's question state.
func (c globalQuestionContext) Value(key any) any {
	switch key.(type) {
	case batchQuestionRouteContextKey, batchAgentQuestionContextKey, batchAgentQuestionBindingKey, batchAgentLifecycleContextKey, persistedAgentDepthContextKey:
		return nil
	default:
		return c.Context.Value(key)
	}
}
