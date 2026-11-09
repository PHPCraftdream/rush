package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session"
)

const (
	DefaultAgentDepth      = 2
	MaxAgentDepthForResume = 32
)

// ErrAgentDepthExceeded reports refusal before a child session is created.
var ErrAgentDepthExceeded = errors.New("agent_depth_exceeded")

type persistedAgentDepthContextKey struct{}

// MaxAgentDepthFrom returns the trusted remaining delegation-edge budget.
func MaxAgentDepthFrom(ctx context.Context) (int, error) {
	remaining := DefaultAgentDepth
	if opts := callOptionsFrom(ctx); opts != nil && opts.AgentDepthSet {
		if opts.MaxAgentDepth < 0 || opts.MaxAgentDepth > MaxAgentDepthForResume {
			return 0, errors.New("agent depth must be between 0 and 32")
		}
		remaining = opts.MaxAgentDepth
	}
	if saved, ok := ctx.Value(persistedAgentDepthContextKey{}).(int); ok {
		remaining = min(remaining, saved)
	}
	return remaining, nil
}

// WithAgentDepth sets the immutable trusted remaining delegation budget.
func WithAgentDepth(ctx context.Context, remaining int) (context.Context, error) {
	if remaining < 0 || remaining > 32 {
		return nil, errors.New("agent depth must be between 0 and 32")
	}
	opts := CallOptions{}
	if existing := callOptionsFrom(ctx); existing != nil {
		opts = *existing
	}
	opts.MaxAgentDepth = remaining
	opts.AgentDepthSet = true
	return WithCallOptions(ctx, &opts), nil
}

func effectiveParentAgentDepth(ctx context.Context, sessions session.Service, parentID string) (int, error) {
	requested, err := MaxAgentDepthFrom(ctx)
	if err != nil {
		return 0, err
	}
	store, ok := sessions.(session.AgentDepthStore)
	if !ok {
		return 0, errors.New("agent depth persistence unavailable")
	}
	// A narrower delegation must not lower the parent's sibling budget.
	return store.ResolveAgentDepth(ctx, parentID, requested)
}

// WithPersistedSessionAgentDepth applies the saved session and lineage caps.
func WithPersistedSessionAgentDepth(ctx context.Context, sessions session.Service, sessionID string) (context.Context, error) {
	requested, err := MaxAgentDepthFrom(ctx)
	if err != nil {
		return nil, err
	}
	remaining, err := PersistSessionAgentDepth(ctx, sessions, sessionID, requested)
	if err != nil {
		return nil, err
	}
	// A durable cap must not turn legacy admission into an explicit-options call.
	if callOptionsFrom(ctx) == nil {
		return context.WithValue(ctx, persistedAgentDepthContextKey{}, remaining), nil
	}
	return WithAgentDepth(ctx, remaining)
}

// withPersistedCallAgentDepth tightens a queued call immediately before execution.
func withPersistedCallAgentDepth(ctx context.Context, sessions session.Service, call *SessionAgentCall) (context.Context, int, error) {
	if call.CallOptions != nil {
		ctx = WithCallOptions(ctx, call.CallOptions)
	}
	ctx, err := WithPersistedSessionAgentDepth(ctx, sessions, call.SessionID)
	if err != nil {
		return nil, 0, err
	}
	remaining, err := MaxAgentDepthFrom(ctx)
	if err != nil {
		return nil, 0, err
	}
	call.CallOptions = callOptionsFrom(ctx)
	return ctx, remaining, nil
}

// PersistSessionAgentDepth stores the minimum of the current and lineage limits.
func PersistSessionAgentDepth(ctx context.Context, sessions session.Service, sessionID string, requested int) (int, error) {
	if requested < 0 || requested > MaxAgentDepthForResume {
		return 0, errors.New("agent depth must be between 0 and 32")
	}
	store, ok := sessions.(session.AgentDepthStore)
	if !ok {
		return 0, errors.New("agent depth persistence unavailable")
	}
	remaining, err := store.ResolveAgentDepth(ctx, sessionID, requested)
	if err != nil {
		return 0, err
	}
	if err := store.LowerAgentDepth(ctx, sessionID, remaining); err != nil {
		return 0, fmt.Errorf("persist agent depth: %w", err)
	}
	return remaining, nil
}
