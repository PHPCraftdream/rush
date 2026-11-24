package sesstypes

// ModelCfg is a JSON-serializable subset of config.SelectedModel
// (task #340, ROUND 3 migration). It mirrors the JSON-tagged fields
// of config.SelectedModel without importing the config package to avoid
// import cycles. The coordinator (which can import config) reconstructs
// the full Model from this data during pump execution.
type ModelCfg struct {
	Model            string         `json:"model"`
	Provider         string         `json:"provider"`
	ReasoningEffort  string         `json:"reasoning_effort,omitempty"`
	Think            bool           `json:"think,omitempty"`
	MaxTokens        int64          `json:"max_tokens,omitempty"`
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	TopK             *int64         `json:"top_k,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	ProviderOptions  map[string]any `json:"provider_options,omitempty"`
}
