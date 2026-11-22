package batchflow

import "context"

// GlobalQuestionContext wraps a delegation's context for GLOBAL model
// rebuilds: per-delegation question state must not leak into the rebuild.
type GlobalQuestionContext struct {
	context.Context
	// Foreign masks agent-owned context keys the batchflow layer cannot
	// name (e.g. the agent package's persisted-depth key). It receives the
	// lookup key and returns true to mask it.
	Foreign func(any) bool
}
