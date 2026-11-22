// Historical package-agent surface retained while implementations live in the
// runtime delegation adapter.
package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter/delegation"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

const (
	DefaultAgentDepth      = delegation.DefaultAgentDepth
	MaxAgentDepthForResume = delegation.MaxAgentDepthForResume
)

var ErrAgentDepthExceeded = delegation.ErrAgentDepthExceeded

func MaxAgentDepthFrom(ctx context.Context) (int, error) { return delegation.MaxAgentDepthFrom(ctx) }

func WithAgentDepth(ctx context.Context, remaining int) (context.Context, error) {
	return delegation.WithAgentDepth(ctx, remaining)
}

func effectiveParentAgentDepth(ctx context.Context, sessions session.Service, parentID string) (int, error) {
	return delegation.EffectiveParentAgentDepth(ctx, sessions, parentID)
}

func WithPersistedSessionAgentDepth(ctx context.Context, sessions session.Service, sessionID string) (context.Context, error) {
	return delegation.WithPersistedSessionAgentDepth(ctx, sessions, sessionID)
}

func withPersistedCallAgentDepth(ctx context.Context, sessions session.Service, call *SessionAgentCall) (context.Context, int, error) {
	return delegation.WithPersistedCallAgentDepth(ctx, sessions, call)
}

func PersistSessionAgentDepth(ctx context.Context, sessions session.Service, sessionID string, requested int) (int, error) {
	return delegation.PersistSessionAgentDepth(ctx, sessions, sessionID, requested)
}
