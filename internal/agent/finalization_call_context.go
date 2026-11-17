package agent

import (
	"context"

	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// restoreFinalizationCallContext preserves trusted capabilities across mailbox
// handoffs without deriving authority from a session ID or driver generation.
func (a *sessionAgent) restoreFinalizationCallContext(ctx context.Context, call *SessionAgentCall) (context.Context, error) {
	if call == nil {
		return nil, ErrFinalizationFactsUnavailable
	}
	guardCtx, cancel := context.WithTimeout(ctx, a.effectiveSessionPreambleMaxDuration())
	defer cancel()
	validated, err := call.FinalizationAuthorities.RestoreCall(guardCtx, call.SessionID, call.LogicalCallID)
	if err != nil {
		return nil, err
	}
	call.FinalizationAuthorities = session.CaptureAgentFinalizationContext(validated)
	// Preserve the caller's lifetime, not the bounded validation context.
	return call.FinalizationAuthorities.Restore(ctx)
}
