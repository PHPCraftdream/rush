package batchflow

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent/state"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
)

// Global model rebuilds must not capture one delegation's question state.
func (c GlobalQuestionContext) Value(key any) any {
	switch key.(type) {
	case batchmeta.QuestionRouteContextKey, state.QuestionContextKey, state.QuestionBindingKey, state.LifecycleContextKey:
		return nil
	}
	if c.Foreign != nil && c.Foreign(key) {
		return nil
	}
	return c.Context.Value(key)
}
