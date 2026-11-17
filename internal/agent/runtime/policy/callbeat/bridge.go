package callbeat

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/session/heartbeat"
)

const (
	HbSourceSlotStart       = hbSourceSlotStart
	HbSourceSlotLive        = hbSourceSlotLive
	HbSourceSessionOverride = hbSourceSessionOverride
	HbSourcePerCall         = hbSourcePerCall
)

func WithHeartbeat(ctx context.Context, sessionID string, purpose heartbeat.Purpose, role, source string) context.Context {
	return withHeartbeat(ctx, sessionID, purpose, role, source)
}

func InheritHeartbeat(parent context.Context, childID, role, source string) context.Context {
	return inheritHeartbeat(parent, childID, role, source)
}

func HbTurnRole(call agentcall.SessionAgentCall, hb heartbeat.Context) string {
	return hbTurnRole(call, hb)
}
func HbTurnSource(ctx context.Context, hb heartbeat.Context) string { return hbTurnSource(ctx, hb) }
func HbFastSource(ctx context.Context, hb heartbeat.Context) string { return hbFastSource(ctx, hb) }
func HbInherited(ctx context.Context) (role, source string)         { return hbInherited(ctx) }
func WithModelSource(ctx context.Context, smart, fast string) context.Context {
	return withModelSource(ctx, smart, fast)
}
func HbOverrideSource(overridden bool) string { return hbOverrideSource(overridden) }
func HbCallSource(overridden bool) string     { return hbCallSource(overridden) }
func AddHeartbeatUsage(ctx context.Context, provider, model string, usage fantasy.Usage, costDelta float64) {
	addHeartbeatUsage(ctx, provider, model, usage, costDelta)
}

func HbUsage(usage fantasy.Usage, costDelta float64) heartbeat.Usage {
	return hbUsage(usage, costDelta)
}
func HbUsageNonZero(u heartbeat.Usage) bool { return hbUsageNonZero(u) }
