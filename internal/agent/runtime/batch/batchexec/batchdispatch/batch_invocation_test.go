package batchdispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

func TestLeafRunContextKeyIdentity(t *testing.T) {
	folder, allow := &permission.FolderScopeSpec{}, &permission.RunAllowlistSpec{}
	depth := 1
	policy := &RunPolicy{MessageID: "message", FolderScope: folder, RunAllowlist: allow, CustomDisk: "disk"}
	ctx := LeafRunContext(context.Background(), "session", nil, nil, policy, nil)
	if ctx.Value(tools.SessionIDContextKey) != "session" || ctx.Value(tools.MessageIDContextKey) != "message" || ctx.Value(FolderScopeContextKey{}) != folder || ctx.Value(RunAllowlistContextKey{}) != allow || ctx.Value(CustomDiskContextKey{}) != "disk" {
		t.Fatal("leaf context lost invocation values")
	}
	ctx = LeafRunContext(context.Background(), "session", &depth, func(ctx context.Context, _ int) (context.Context, error) {
		if ctx.Value(tools.SessionIDContextKey) != "session" {
			t.Fatal("session not installed before depth")
		}
		return nil, errors.New("invalid depth")
	}, policy, nil)
	if ctx.Value(AgentDepthInvalidContextKey{}) != true || ctx.Value(tools.MessageIDContextKey) != nil {
		t.Fatal("invalid depth did not short-circuit policy")
	}
}
