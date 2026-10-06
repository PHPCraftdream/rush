// Heartbeat attribution helpers: every consumer that reaches a provider call
// attaches a heartbeat.Context through these, so w1251's provider wrap can
// count requests and this package can report usage against the right root.
package agent

import (
	"context"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/heartbeat"
)

// Heartbeat source labels (design point 3).
const (
	hbSourceSlotStart       = "slot@start"
	hbSourceSessionOverride = "session-override"
	hbSourcePerCall         = "per-call"
)

// modelSourceContextKey carries the (smart, fast) source labels computed where
// the models are chosen, so runTurn/generateTitle can read them off the ctx.
type modelSourceContextKey struct{}

type hbModelSources struct{ smart, fast string }

func withModelSource(ctx context.Context, smart, fast string) context.Context {
	return context.WithValue(ctx, modelSourceContextKey{}, hbModelSources{smart: smart, fast: fast})
}

func modelSourceFrom(ctx context.Context) hbModelSources {
	s, _ := ctx.Value(modelSourceContextKey{}).(hbModelSources)
	return s
}

// hbOverrideSource maps one slot's override decision to its source label.
func hbOverrideSource(overridden bool) string {
	if overridden {
		return hbSourceSessionOverride
	}
	return hbSourceSlotStart
}

// hbFallbackRole is the role reported when nothing more specific is known:
// the default slot the code names for an ordinary run.
const hbFallbackRole = "smart"

// hbDefaultSource is the source reported when no resolution marker and no
// inherited label exist (e.g. a detached restart that never re-resolved).
const hbDefaultSource = hbSourceSlotStart

// withHeartbeat attaches attribution for sessionID, keeping the inherited
// RootSessionID (root run entry: none inherited, so the root ids all equal
// sessionID). A pre-stamped call-point purpose (agentic fetch) survives a
// turn-purpose attach; role/source fall back to inherited then defaults.
func withHeartbeat(ctx context.Context, sessionID string, purpose heartbeat.Purpose, role, source string) context.Context {
	hb := heartbeat.FromContext(ctx)
	if purpose == heartbeat.PurposeTurn && hb.Purpose != "" && hb.Purpose != heartbeat.PurposeTurn {
		purpose, role, source = hb.Purpose, hb.Role, hb.Source
	}
	role = firstNonEmptyHB(role, hb.Role, hbFallbackRole)
	source = firstNonEmptyHB(source, hb.Source, hbDefaultSource)
	root := hb.RootSessionID
	if root == "" {
		root = hb.SessionID
	}
	if root == "" {
		root = sessionID
	}
	return heartbeat.WithContext(ctx, heartbeat.Context{
		SessionID: sessionID, RootSessionID: root, AgentID: sessionID,
		Purpose: purpose, Role: role, Source: source,
	})
}

// inheritHeartbeat re-parents attribution onto childID at a delegation
// boundary: root inherited, child session/agent ids, role/source supplied by
// the boundary unless the parent ctx already carries a call-point purpose.
func inheritHeartbeat(parent context.Context, childID, role, source string) context.Context {
	hb := heartbeat.FromContext(parent)
	if hb.Purpose != "" && hb.Purpose != heartbeat.PurposeTurn {
		role, source = hb.Role, hb.Source
	}
	return withHeartbeat(parent, childID, turnPurposeOrInherited(hb), role, source)
}

func turnPurposeOrInherited(hb heartbeat.Context) heartbeat.Purpose {
	if hb.Purpose != "" {
		return hb.Purpose
	}
	return heartbeat.PurposeTurn
}

// hbTurnRole resolves the main turn's slot role: the per-call CallOptions
// role when set, else the inherited sub-agent role, else smart.
func hbTurnRole(call SessionAgentCall, hb heartbeat.Context) string {
	if call.CallOptions != nil && call.CallOptions.ModelRole != "" {
		return string(call.CallOptions.ModelRole)
	}
	return firstNonEmptyHB("", hb.Role, hbFallbackRole)
}

// hbTurnSource resolves the turn's source: the model-resolution marker when
// present, else inherited, else the default.
func hbTurnSource(ctx context.Context, hb heartbeat.Context) string {
	src := modelSourceFrom(ctx).smart
	return firstNonEmptyHB(src, hb.Source, hbDefaultSource)
}

// hbFastSource resolves the title path's source from the fast-slot marker.
func hbFastSource(ctx context.Context, hb heartbeat.Context) string {
	src := modelSourceFrom(ctx).fast
	return firstNonEmptyHB(src, hb.Source, hbDefaultSource)
}

// hbInherited is the role/source pair a detached consumer (summary,
// keepalive) inherits from its ctx, with defaults filled.
func hbInherited(ctx context.Context) (role, source string) {
	hb := heartbeat.FromContext(ctx)
	return firstNonEmptyHB("", hb.Role, hbFallbackRole), firstNonEmptyHB("", hb.Source, hbDefaultSource)
}

// hbUsage maps the normalized fantasy usage and the already-computed cost
// delta onto heartbeat.Usage.
func hbUsage(usage fantasy.Usage, costDelta float64) heartbeat.Usage {
	return heartbeat.Usage{
		Input:      usage.InputTokens,
		Output:     usage.OutputTokens,
		CacheRead:  usage.CacheReadTokens,
		CacheWrite: usage.CacheCreationTokens,
		CostUSD:    costDelta,
	}
}

// hbUsageNonZero reports whether any accounted figure is set: an all-zero
// delta must not create a registry entry.
func hbUsageNonZero(u heartbeat.Usage) bool {
	return u.Input != 0 || u.Output != 0 || u.CacheRead != 0 || u.CacheWrite != 0 || u.CostUSD != 0
}

// addHeartbeatUsage folds one accounted unit in; never blocks or panics.
func addHeartbeatUsage(ctx context.Context, provider, model string, usage fantasy.Usage, costDelta float64) {
	u := hbUsage(usage, costDelta)
	if !hbUsageNonZero(u) {
		return
	}
	heartbeat.AddUsage(ctx, provider, model, u)
}

func firstNonEmptyHB(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
