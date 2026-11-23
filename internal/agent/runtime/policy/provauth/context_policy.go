// Context-window and output-reserve budget policy shared by the agent
// runtime's context gate: effective output tokens, the input-token budget,
// and the auto-summarize threshold that decides when summarization fires.
package provauth

import (
	core "github.com/PHPCraftdream/rush/internal/session/service"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/config/providers/discover"
)

// SessionHeaders returns the HTTP headers we use for cache affinity on
// every LLM request for a given session.
//
// We use the session hash instead of the raw UUID so the header value
// is deterministic and opaque.
func SessionHeaders(sessionID string) map[string]string {
	hash := core.HashID(sessionID)
	return map[string]string{
		"x-session-id":       hash,
		"x-session-affinity": hash,
	}
}

const (
	// largeContextWindowThreshold is the context-window size above which the
	// auto-summarize headroom switches from the small-window ratio to the
	// proportional large-window divisor below.
	largeContextWindowThreshold = 200_000
	// largeContextWindowBuffer is the minimum headroom kept on large windows.
	largeContextWindowBuffer = 20_000
	// smallContextWindowRatio is the fraction of the input budget kept as
	// auto-summarize headroom on small windows.
	smallContextWindowRatio = 0.2

	// largeContextHeadroomDivisor makes large-window headroom proportional:
	// inputBudget/20 (5%), never below largeContextWindowBuffer.
	largeContextHeadroomDivisor = 20
)

// TurnSmartReasoningEffort reports the effective effort pinned to this turn's
// resolved smart model. Session fields are deliberately not consulted: the
// resolved model configuration already incorporates applicable preferences.
func TurnSmartReasoningEffort(smartModel agentcall.Model) string {
	return smartModel.ModelCfg.ReasoningEffort
}

func EffectiveMaxOutputTokens(model agentcall.Model, callOverride int64) int64 {
	if callOverride > 0 {
		return callOverride
	}
	if model.ModelCfg.MaxTokens > 0 {
		return model.ModelCfg.MaxTokens
	}
	return model.CatwalkCfg.DefaultMaxTokens
}

// ModelInputBudget is the input-token budget the context gate works against:
// window minus output reserve (the whole window when no reserve is known),
// never above the vendor-documented maximum input of the model.
func ModelInputBudget(model agentcall.Model, callOverride int64) int64 {
	cw := int64(model.CatwalkCfg.ContextWindow)
	reserve := EffectiveMaxOutputTokens(model, callOverride)
	budget := contextInputBudget(cw, reserve)
	if reserve <= 0 {
		// Legacy gate for models without a known output reserve.
		budget = cw
	}
	id := model.CatwalkCfg.ID
	if id == "" {
		id = model.ModelCfg.Model
	}
	if limit := discover.LookupModelMaxInput(model.ModelCfg.Provider, id); limit > 0 && budget > limit {
		budget = limit
	}
	return budget
}

func contextInputBudget(contextWindow, outputReserve int64) int64 {
	if contextWindow <= 0 || outputReserve <= 0 || outputReserve >= contextWindow {
		return 0
	}
	return contextWindow - outputReserve
}

// ContextAutoSummarizeThreshold is the remaining-input headroom at or below
// which the context gate fires. Large windows keep at least
// largeContextWindowBuffer but scale with the budget (1/largeContextHeadroomDivisor),
// so one tool-heavy step cannot jump past a fixed 20k margin on a ~1M window.
func ContextAutoSummarizeThreshold(contextWindow, inputBudget int64) int64 {
	if contextWindow <= 0 || inputBudget <= 0 {
		return 0
	}
	threshold := int64(float64(inputBudget) * smallContextWindowRatio)
	if contextWindow > largeContextWindowThreshold {
		threshold = max(largeContextWindowBuffer, inputBudget/largeContextHeadroomDivisor)
	}
	return min(inputBudget, threshold)
}
