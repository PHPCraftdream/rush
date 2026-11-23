package modelkit

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/PHPCraftdream/rush/internal/agent/provider/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/providers/discover"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/qjebbs/go-jsons"
)

type providerModel = agentcall.Model

// effectiveReasoningEffort returns the reasoning effort to apply for provider calls.
// It prefers the user-selected effort when valid, otherwise the model default when
// valid, and finally falls back to the first configured reasoning level.
func effectiveReasoningEffort(model providerModel) string {
	if !model.CatwalkCfg.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatwalkCfg.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatwalkCfg.ReasoningLevels) > 0 {
		return model.CatwalkCfg.ReasoningLevels[0]
	}
	return ""
}

// fantasyAcceptsReasoningEffort reports whether fantasy's OpenAI-compatible
// providers take effort as the top-level reasoning_effort value.
func fantasyAcceptsReasoningEffort(effort string) bool {
	switch openai.ReasoningEffort(effort) {
	case openai.ReasoningEffortNone, openai.ReasoningEffortMinimal, openai.ReasoningEffortLow,
		openai.ReasoningEffortMedium, openai.ReasoningEffortHigh, openai.ReasoningEffortXHigh:
		return true
	}
	return false
}

func GetProviderOptions(sessionID string, model providerModel, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}
	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.CatwalkCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal([]byte(got), &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	shouldSetEffort := model.CatwalkCfg.CanReason &&
		reasoningEffort != "" &&
		slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort)
	if verified, ok := providerCfg.LiveEfforts[model.ModelCfg.Model]; ok &&
		model.ModelCfg.ReasoningEffort == "" && verified.Default == "" {
		shouldSetEffort = false
	}

	switch providerCfg.Type {
	case openai.Name, azure.Name:
		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
		if openai.IsResponsesModel(model.CatwalkCfg.ID) {
			if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) {
				mergedOptions["reasoning_summary"] = "auto"
				mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
			}
			parsed, err := openai.ParseResponsesOptions(mergedOptions)
			if err == nil {
				// Route cache-affine requests to the same backend host (session-stable, opaque).
				if parsed.PromptCacheKey == nil && sessionID != "" {
					hash := session.HashID(sessionID)
					parsed.PromptCacheKey = &hash
				}
				options[openai.Name] = parsed
			}
		} else {
			parsed, err := openai.ParseOptions(mergedOptions)
			if err == nil {
				// Route cache-affine requests to the same backend host (session-stable, opaque).
				if parsed.PromptCacheKey == nil && sessionID != "" {
					hash := session.HashID(sessionID)
					parsed.PromptCacheKey = &hash
				}
				options[openai.Name] = parsed
			}
		}
	case anthropic.Name, bedrock.Name:
		var (
			_, hasEffort = mergedOptions["effort"]
			_, hasThink  = mergedOptions["thinking"]
			extraBody    = make(map[string]any)
		)

		switch providerCfg.ID {
		case string(catwalk.InferenceProviderAlibabaSingapore):
			switch {
			case !hasEffort && shouldSetEffort:
				extraBody["reasoning_effort"] = reasoningEffort
			case !hasThink && model.CatwalkCfg.CanReason:
				if model.ModelCfg.Think {
					extraBody["thinking"] = map[string]any{"type": "enabled"}
				} else {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
				}
			}
			mergedOptions["extra_body"] = extraBody

		default:
			switch {
			case !hasEffort && shouldSetEffort:
				mergedOptions["effort"] = reasoningEffort
			case !hasThink && model.ModelCfg.Think:
				mergedOptions["thinking"] = map[string]any{"budget_tokens": 2000}
			}
		}

		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err == nil {
			options[anthropic.Name] = parsed
		}

	case openrouter.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := openrouter.ParseOptions(mergedOptions)
		if err == nil {
			options[openrouter.Name] = parsed
		}
	case vercel.Name:
		_, hasReasoning := mergedOptions["reasoning"]
		if !hasReasoning && shouldSetEffort {
			mergedOptions["reasoning"] = map[string]any{
				"enabled": true,
				"effort":  reasoningEffort,
			}
		}
		parsed, err := vercel.ParseOptions(mergedOptions)
		if err == nil {
			options[vercel.Name] = parsed
		}
	case google.Name, catwalk.TypeVertexAI:
		_, hasReasoning := mergedOptions["thinking_config"]
		if !hasReasoning {
			if strings.HasPrefix(model.CatwalkCfg.ID, "gemini-2") {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_budget":  2000,
					"include_thoughts": true,
				}
			} else {
				mergedOptions["thinking_config"] = map[string]any{
					"thinking_level":   reasoningEffort,
					"include_thoughts": true,
				}
			}
		}
		parsed, err := google.ParseOptions(mergedOptions)
		if err == nil {
			options[google.Name] = parsed
		}
	case openaicompat.Name, hyper.Name:
		extraBody := make(map[string]any)

		_, hasReasoningEffort := mergedOptions["reasoning_effort"]
		if !hasReasoningEffort && shouldSetEffort {
			switch providerCfg.ID {
			case string(catwalk.InferenceProviderIoNet):
				extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
			default:
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		}

		// "reasoning effort" is a standard OpenAI field, but "thinking" is not.
		// Setting it in the right way for each provider.
		// TODO: Abstract this in Fantasy somehow?
		// TODO: Allow custom providers to specify how to set this?
		//
		// SYNC WARNING: the per-provider effort/thinking mapping below (ZAI,
		// DeepSeek, io.net, Alibaba Singapore, hyper) is restated in prose for
		// users by `rush models efforts` (providerEffortDocs in
		// internal/cmd/models_efforts.go). That prose is NOT derived from this
		// switch — if you change a mapping here, update the matching entry
		// there too, or the CLI help will describe stale behavior.
		switch providerCfg.ID {
		case hyper.Name:
			extraBody["thinking"] = model.ModelCfg.Think
		case string(catwalk.InferenceProviderIoNet):
			if _, ok := extraBody["reasoning"]; !ok && model.CatwalkCfg.CanReason {
				if model.ModelCfg.Think {
					extraBody["reasoning"] = map[string]string{"effort": "medium"}
				} else {
					extraBody["reasoning"] = map[string]string{"effort": "none"}
				}
			}
		case string(catwalk.InferenceProviderZAI):
			effort := strings.ToLower(model.ModelCfg.ReasoningEffort)
			modelID := strings.ToLower(model.ModelCfg.Model)
			if verified, ok := providerCfg.LiveEfforts[model.ModelCfg.Model]; ok {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
				if effort == "" || !slices.Contains(verified.Levels, effort) {
					break
				}
				if effort == "none" || effort == "minimal" {
					extraBody["thinking"] = map[string]any{"type": "disabled"}
					delete(mergedOptions, "reasoning_effort")
				} else {
					extraBody["reasoning_effort"] = effort
				}
				break
			}
			if zai53ModelIDs[modelID] {
				// GLM-5.3/5.3-Flash can't disable reasoning at all (unlike
				// every model in the branch below) and take low/high/max
				// instead of off/high/max — see zai53ReasoningLevels' comment
				// in models_atoms.go for the verification and SYNC WARNING.
				// "off" (raw-syntax only, rejected by `rush models use` for
				// these two atoms) degrades to "low"; unset stays "high" per
				// this fork's existing convention, not the doc's own "max".
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
				switch effort {
				case "off", "low":
					extraBody["reasoning_effort"] = "low"
				case "high":
					extraBody["reasoning_effort"] = "high"
				case "xhigh", "max", "ultracode":
					extraBody["reasoning_effort"] = "max"
				default:
					extraBody["reasoning_effort"] = "high"
				}
				break
			}
			// GLM-5.x (pre-5.3) exposes two thinking-effort levels
			// (high / max) and no "unset" state on z.ai's side — reasoning
			// is either on at some level or off. Z.AI recommends max for
			// hard coding/math tasks, so an unset effort defaults to
			// thinking ON at "high" rather than silently disabling
			// reasoning. Explicitly opt out with ReasoningEffort == "off"
			// (e.g. `rush models use zai/glm-5.2@off <fast>` — the raw
			// provider/model@effort syntax accepts any suffix, unvalidated
			// against ReasoningLevels).
			//
			// We forward via `extra_body.reasoning_effort` — the
			// OpenAI-compat field z.ai already accepts on its Anthropic-
			// compatible coding endpoint. Mapping mirrors z.ai's own
			// "Claude Code selected effort → GLM-5.2 actual mapped effort"
			// table from docs.z.ai/devpack/latest-model:
			//   (unset), low, medium, high (default) → high
			//   xhigh, max, ultracode                → max
			//   off                                  → thinking disabled
			// Older GLM-4.x ignore the field harmlessly.
			if effort == "off" {
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			} else {
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
				switch effort {
				case "xhigh", "max", "ultracode":
					extraBody["reasoning_effort"] = "max"
				default:
					extraBody["reasoning_effort"] = "high"
				}
			}
		case string(catwalk.InferenceProviderDeepSeek):
			// DeepSeek keeps the fork's original "unset reasoning = thinking
			// off" default. Reasoning is only enabled when the user opts in
			// (via Think or an explicit ReasoningEffort); the ZAI-only default
			// of turning thinking ON at "high" for an unset effort deliberately
			// does NOT apply here.
			if model.ModelCfg.Think || model.ModelCfg.ReasoningEffort != "" {
				extraBody["thinking"] = map[string]any{
					"type": "enabled",
				}
			} else {
				extraBody["thinking"] = map[string]any{
					"type": "disabled",
				}
			}
			// When reasoning is enabled, map the selected effort onto the two
			// effort levels the endpoint exposes (high / max). Mirrors the ZAI
			// table above:
			//   low, medium, high (default) → high
			//   xhigh, max, ultracode       → max
			if model.ModelCfg.ReasoningEffort != "" {
				switch strings.ToLower(model.ModelCfg.ReasoningEffort) {
				case "xhigh", "max", "ultracode":
					extraBody["reasoning_effort"] = "max"
				default:
					extraBody["reasoning_effort"] = "high"
				}
			}
		case string(catwalk.InferenceProviderAlibabaSingapore):
			if model.CatwalkCfg.CanReason {
				extraBody["enable_thinking"] = model.ModelCfg.Think
			}
		}

		// z.ai and DeepSeek carry their effort in extra_body, in wire values
		// ("max") the OpenAI vocabulary does not have. Fantasy validates the
		// top-level reasoning_effort against that vocabulary and fails the
		// whole call ("reasoning model `max` not supported"), so a value it
		// would reject must not also ride there: it made every reviewer pass
		// at @max fail.
		switch providerCfg.ID {
		case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
			if effort, ok := mergedOptions["reasoning_effort"].(string); ok && !fantasyAcceptsReasoningEffort(effort) {
				delete(mergedOptions, "reasoning_effort")
			}
		}

		mergedOptions["extra_body"] = extraBody

		parsed, err := openaicompat.ParseOptions(mergedOptions)
		if err == nil {
			options[openaicompat.Name] = parsed
		}
	default:
		// Known custom providers (litellm, ollama, omlx, lmstudio) speak
		// openai-compat under the hood, so route their options through the
		// openai-compat parser too.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			parsed, err := openaicompat.ParseOptions(mergedOptions)
			if err == nil {
				options[openaicompat.Name] = parsed
			}
		}
	}

	return options
}
