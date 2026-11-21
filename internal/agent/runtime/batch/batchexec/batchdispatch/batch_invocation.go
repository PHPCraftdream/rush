package batchdispatch

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// RunPolicy holds the invocation values needed to reconstruct a leaf context.
type RunPolicy struct {
	MessageID      string
	SupportsImages bool
	ModelName      string
	FolderScope    *permission.FolderScopeSpec
	RunAllowlist   *permission.RunAllowlistSpec
	CustomDisk     string
	WithRole       func(context.Context) context.Context
}

// LeafRunContext restores trusted values using the same keys as the caller.
func LeafRunContext(base context.Context, sessionID string, depth *int, withDepth func(context.Context, int) (context.Context, error), policy *RunPolicy, provider tools.DiskProvider) context.Context {
	ctx := context.WithValue(base, tools.SessionIDContextKey, sessionID)
	if depth != nil {
		next, err := withDepth(ctx, *depth)
		if err != nil {
			return context.WithValue(ctx, AgentDepthInvalidContextKey{}, true)
		}
		ctx = next
	}
	if policy == nil {
		return ctx
	}
	if provider != nil {
		ctx = context.WithValue(ctx, DiskProviderContextKey{}, provider)
	}
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, policy.MessageID)
	ctx = context.WithValue(ctx, tools.SupportsImagesContextKey, policy.SupportsImages)
	ctx = context.WithValue(ctx, tools.ModelNameContextKey, policy.ModelName)
	if policy.WithRole != nil {
		ctx = policy.WithRole(ctx)
	}
	if policy.FolderScope != nil {
		ctx = context.WithValue(ctx, FolderScopeContextKey{}, policy.FolderScope)
	}
	if policy.RunAllowlist != nil {
		ctx = context.WithValue(ctx, RunAllowlistContextKey{}, policy.RunAllowlist)
	}
	if policy.CustomDisk != "" {
		ctx = context.WithValue(ctx, CustomDiskContextKey{}, policy.CustomDisk)
	}
	return ctx
}

type (
	FolderScopeContextKey       struct{}
	RunAllowlistContextKey      struct{}
	CustomDiskContextKey        struct{}
	AgentDepthInvalidContextKey struct{}
	DiskProviderContextKey      struct{}
)

// DiskProviderFrom returns the provider for a resolved pinned binding, if any.
func DiskProviderFrom(ctx context.Context) tools.DiskProvider {
	p, _ := ctx.Value(DiskProviderContextKey{}).(tools.DiskProvider)
	return p
}
