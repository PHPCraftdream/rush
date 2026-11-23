package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"charm.land/catwalk/pkg/catwalk"
)

func init() {
	RegisterEnricher("ollama", &ollamaEnricher{})
}

// ollamaShowResponse mirrors the response from Ollama's POST /api/show
// endpoint. Only the fields we care about are decoded.
type ollamaShowResponse struct {
	ModelInfo map[string]any `json:"model_info"`
}

// ollamaEnricher fetches model metadata from Ollama's /api/show
// endpoint and populates context window on discovered models.
type ollamaEnricher struct{}

func (e *ollamaEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) ([]catwalk.Model, error) {
	// Collect indices that need enrichment.
	var needEnrichment []int
	for i := range models {
		if models[i].ContextWindow == 0 {
			needEnrichment = append(needEnrichment, i)
		}
	}
	if len(needEnrichment) == 0 {
		return models, nil
	}

	// Fetch metadata concurrently with bounded parallelism.
	type result struct {
		index         int
		contextLength int64
	}

	results := make([]result, len(needEnrichment))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5) // Max 5 concurrent requests.

	for ri, idx := range needEnrichment {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			resp, err := doRequest(ctx, cfg.HTTPClient, http.MethodPost, stripV1Suffix(cfg.BaseURL), "/api/show",
				cfg.APIKey, cfg.ExtraHeaders, resolver,
				map[string]string{"model": models[idx].ID})
			if err != nil {
				return
			}
			defer resp.Body.Close()

			var showResp ollamaShowResponse
			if err := json.NewDecoder(resp.Body).Decode(&showResp); err != nil {
				return
			}

			if cl := extractContextLength(showResp.ModelInfo); cl > 0 {
				results[ri] = result{index: idx, contextLength: cl}
			}
		})
	}
	wg.Wait()

	for _, r := range results {
		if r.contextLength > 0 {
			models[r.index].ContextWindow = r.contextLength
		}
	}

	return models, nil
}

// extractContextLength finds the context_length value in Ollama's
// model_info map. The key is architecture-specific (e.g.
// "llama.context_length", "qwen2.context_length"), so we scan for any
// key ending in ".context_length".
func extractContextLength(info map[string]any) int64 {
	for k, v := range info {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case json.Number:
			i, err := n.Int64()
			if err == nil {
				return i
			}
		}
	}
	return 0
}

func init() {
	RegisterEnricher("lmstudio", &lmstudioEnricher{})
}

// lmstudioModelsResponse mirrors the response from LM Studio's native
// GET /api/v1/models endpoint. The model array is returned under the
// "models" key (not "data" like the OpenAI-compatible endpoint). Only
// the fields we care about are decoded.
type lmstudioModelsResponse struct {
	Models []lmstudioModelEntry `json:"models"`
}

// lmstudioModelEntry is a single entry from /api/v1/models.
type lmstudioModelEntry struct {
	Key              string             `json:"key"`
	DisplayName      string             `json:"display_name"`
	MaxContextLength int64              `json:"max_context_length"`
	LoadedInstances  []lmstudioInstance `json:"loaded_instances"`
}

// lmstudioInstance is a currently loaded model instance with its
// runtime config.
type lmstudioInstance struct {
	Config lmstudioInstanceConfig `json:"config"`
}

// lmstudioInstanceConfig holds per-instance runtime settings.
type lmstudioInstanceConfig struct {
	ContextLength int64 `json:"context_length"`
}

// lmstudioEnricher fetches model metadata from LM Studio's native
// /api/v1/models endpoint and populates context window and display
// name on discovered models.
type lmstudioEnricher struct{}

func (e *lmstudioEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) ([]catwalk.Model, error) {
	resp, err := doRequest(ctx, cfg.HTTPClient, http.MethodGet, stripV1Suffix(cfg.BaseURL), "/api/v1/models", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return models, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return models, nil
	}

	var modelsResp lmstudioModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return models, nil
	}

	// Index by key for O(1) lookup.
	metaByKey := make(map[string]lmstudioModelEntry, len(modelsResp.Models))
	for _, m := range modelsResp.Models {
		metaByKey[m.Key] = m
	}

	for i := range models {
		meta, ok := metaByKey[models[i].ID]
		if !ok {
			continue
		}

		// Context window: prefer loaded instance config, fall back
		// to the model-level max.
		if models[i].ContextWindow == 0 {
			if len(meta.LoadedInstances) > 0 && meta.LoadedInstances[0].Config.ContextLength > 0 {
				models[i].ContextWindow = meta.LoadedInstances[0].Config.ContextLength
			} else if meta.MaxContextLength > 0 {
				models[i].ContextWindow = meta.MaxContextLength
			}
		}

		// Display name if not already set by user.
		if models[i].Name == models[i].ID && meta.DisplayName != "" {
			models[i].Name = meta.DisplayName
		}

		// TODO: populate vision/tool-use flags when catwalk.Model
		// gains dedicated fields for them.
	}

	return models, nil
}

func init() {
	RegisterEnricher("llamacpp", &llamacppEnricher{})
}

// llamacppModelsResponse mirrors the response from llama-server's
// GET /v1/models endpoint. Unlike the standard OpenAI listing,
// llama-server embeds a meta block with model architecture details.
type llamacppModelsResponse struct {
	Data []llamacppModelEntry `json:"data"`
}

// llamacppModelEntry is a single entry from /v1/models.
type llamacppModelEntry struct {
	ID   string       `json:"id"`
	Meta llamacppMeta `json:"meta"`
}

// llamacppMeta holds per-model architecture metadata exposed by
// llama-server in the /v1/models response.
type llamacppMeta struct {
	NCtx      int64 `json:"n_ctx"`
	NCtxTrain int64 `json:"n_ctx_train"`
	NParams   int64 `json:"n_params"`
	Size      int64 `json:"size"`
}

// llamacppEnricher fetches model metadata from llama-server's
// /v1/models endpoint and populates context window on discovered
// models. It prefers n_ctx (the configured runtime context) and
// falls back to n_ctx_train (the model's trained maximum).
type llamacppEnricher struct{}

func (e *llamacppEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) ([]catwalk.Model, error) {
	// llama-server serves /v1/models under the OpenAI-compatible /v1
	// namespace, so the path is relative to the configured base URL
	// (which already includes /v1) rather than the server root.
	resp, err := doRequest(ctx, cfg.HTTPClient, http.MethodGet, cfg.BaseURL, "/models", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return models, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return models, nil
	}

	var modelsResp llamacppModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return models, nil
	}

	// Index by ID for O(1) lookup.
	metaByID := make(map[string]llamacppMeta, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		metaByID[m.ID] = m.Meta
	}

	for i := range models {
		meta, ok := metaByID[models[i].ID]
		if !ok {
			continue
		}

		// Context window: prefer configured n_ctx, fall back to
		// the model's trained maximum.
		if models[i].ContextWindow == 0 {
			if meta.NCtx > 0 {
				models[i].ContextWindow = meta.NCtx
			} else if meta.NCtxTrain > 0 {
				models[i].ContextWindow = meta.NCtxTrain
			}
		}
	}

	return models, nil
}

// litellmModelInfoResponse mirrors the response from LiteLLM's
// /model/info endpoint, which returns rich metadata including context
// windows, max tokens, and pricing.
type litellmModelInfoResponse struct {
	Data []litellmModelInfo `json:"data"`
}

// litellmModelInfo is a single entry from /model/info.
type litellmModelInfo struct {
	ModelName string           `json:"model_name"`
	ModelInfo litellmModelMeta `json:"model_info"`
}

// litellmModelMeta holds the metadata fields we care about from
// LiteLLM's model_info block.
type litellmModelMeta struct {
	MaxInputTokens     *int64   `json:"max_input_tokens"`
	MaxOutputTokens    *int64   `json:"max_output_tokens"`
	InputCostPerToken  *float64 `json:"input_cost_per_token"`
	OutputCostPerToken *float64 `json:"output_cost_per_token"`
	Mode               string   `json:"mode"`
}

func init() {
	RegisterEnricher("litellm", &litellmEnricher{})
}

// litellmEnricher fetches model metadata from LiteLLM's /model/info
// endpoint and populates context window, max tokens, and pricing on
// discovered models.
type litellmEnricher struct{}

func (e *litellmEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) ([]catwalk.Model, error) {
	resp, err := doRequest(ctx, cfg.HTTPClient, http.MethodGet, stripV1Suffix(cfg.BaseURL), "/model/info", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return models, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return models, nil
	}

	var infoResp litellmModelInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&infoResp); err != nil {
		return models, nil
	}

	// Index metadata by model name for O(1) lookup.
	metaByID := make(map[string]litellmModelMeta, len(infoResp.Data))
	for _, entry := range infoResp.Data {
		metaByID[entry.ModelName] = entry.ModelInfo
	}

	// Apply metadata to discovered models, preserving existing
	// non-zero values (user overrides win).
	for i := range models {
		meta, ok := metaByID[models[i].ID]
		if !ok {
			continue
		}
		if models[i].ContextWindow == 0 && meta.MaxInputTokens != nil {
			models[i].ContextWindow = *meta.MaxInputTokens
		}
		if models[i].DefaultMaxTokens == 0 && meta.MaxOutputTokens != nil {
			models[i].DefaultMaxTokens = *meta.MaxOutputTokens
		}
		if models[i].CostPer1MIn == 0 && meta.InputCostPerToken != nil {
			models[i].CostPer1MIn = *meta.InputCostPerToken * 1_000_000
		}
		if models[i].CostPer1MOut == 0 && meta.OutputCostPerToken != nil {
			models[i].CostPer1MOut = *meta.OutputCostPerToken * 1_000_000
		}
	}

	return models, nil
}

func init() {
	RegisterEnricher("omlx", &omlxEnricher{})
}

// omlxModelsStatusResponse mirrors the response from oMLX's
// GET /models/status endpoint, which returns model metadata
// including max_context_window and max_tokens.
type omlxModelsStatusResponse struct {
	Models []omlxModelStatus `json:"models"`
}

// omlxModelStatus is a single entry from /v1/models/status.
type omlxModelStatus struct {
	ID               string `json:"id"`
	MaxContextWindow *int64 `json:"max_context_window"`
	MaxTokens        *int64 `json:"max_tokens"`
}

// omlxEnricher fetches model metadata from oMLX's /models/status
// endpoint and populates context window and max tokens on discovered
// models.
type omlxEnricher struct{}

func (e *omlxEnricher) EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) ([]catwalk.Model, error) {
	// oMLX serves /models/status under the OpenAI-compatible /v1
	// namespace, so the path is relative to the configured base URL
	// (which already includes /v1) rather than the server root.
	resp, err := doRequest(ctx, cfg.HTTPClient, http.MethodGet, cfg.BaseURL, "/models/status", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return models, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return models, nil
	}

	var statusResp omlxModelsStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return models, nil
	}

	// Index by model ID for O(1) lookup.
	metaByID := make(map[string]omlxModelStatus, len(statusResp.Models))
	for _, m := range statusResp.Models {
		metaByID[m.ID] = m
	}

	for i := range models {
		meta, ok := metaByID[models[i].ID]
		if !ok {
			continue
		}
		if models[i].ContextWindow == 0 && meta.MaxContextWindow != nil {
			models[i].ContextWindow = *meta.MaxContextWindow
		}
		if models[i].DefaultMaxTokens == 0 && meta.MaxTokens != nil {
			models[i].DefaultMaxTokens = *meta.MaxTokens
		}
	}

	return models, nil
}
