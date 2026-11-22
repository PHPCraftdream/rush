package delegation

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/config"
)

type parentRoleConfigContextKey struct{}

// ParentRoleConfigFrom returns the parent configuration snapshot carried by ctx.
func ParentRoleConfigFrom(ctx context.Context) (*config.Config, bool) {
	if cfg, ok := ctx.Value(parentRoleConfigContextKey{}).(*config.Config); ok && cfg != nil {
		return cfg, true
	}
	return nil, false
}

// WithParentRoleConfigSnapshot carries the parent's configuration snapshot in ctx.
func WithParentRoleConfigSnapshot(ctx context.Context, cfg *config.Config) context.Context {
	return context.WithValue(ctx, parentRoleConfigContextKey{}, cfg)
}

// DelegationRoleModel is the resolved model bundle used for an agent delegation.
type DelegationRoleModel struct {
	Model       agentcall.Model
	Provider    config.ProviderConfig
	Source      string
	Credentials *agentcall.CredentialSet
}
