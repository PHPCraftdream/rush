package agent

import "context"

// ExecutionProviderIntent records caller intent before a nil provider is
// normalized to OSDisk. Only explicit OS intent may start a new epoch.
type ExecutionProviderIntent uint8

const (
	ExecutionProviderOmitted ExecutionProviderIntent = iota
	ExecutionProviderCustom
	ExecutionProviderOS
)

type executionProviderIntent struct {
	sessionID string
	intent    ExecutionProviderIntent
}

type executionProviderIntentKey struct{}

func WithExecutionProviderIntent(ctx context.Context, sessionID string, intent ExecutionProviderIntent) context.Context {
	return context.WithValue(ctx, executionProviderIntentKey{}, executionProviderIntent{sessionID: sessionID, intent: intent})
}

func executionProviderIntentFrom(ctx context.Context, sessionID string) ExecutionProviderIntent {
	value, _ := ctx.Value(executionProviderIntentKey{}).(executionProviderIntent)
	if value.sessionID != sessionID {
		return ExecutionProviderOmitted
	}
	return value.intent
}
