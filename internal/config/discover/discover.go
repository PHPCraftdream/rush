package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

// httpClient is shared across all discovery and enrichment calls. It
// has a reasonable timeout so individual requests cannot block forever
// even if the caller forgets to set a context deadline.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// clientOrDefault returns client, or the shared default when it is nil.
func clientOrDefault(client *http.Client) *http.Client {
	if client == nil {
		return httpClient
	}
	return client
}

// stripV1Suffix removes a trailing /v1 from a base URL. Enricher
// endpoints (e.g. Ollama's /api/show, LM Studio's /api/v1/models) are
// served at the server root, not under the OpenAI-compatible /v1
// prefix. Since provider configs typically include /v1 in the base URL
// for chat completions, enrichers must strip it before constructing
// their own request paths.
func stripV1Suffix(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// doRequest builds and executes an authenticated HTTP request using the
// shared client. It resolves variable references in the base URL, API
// key, and extra headers via the provided Resolver. The path is joined
// to the base URL with proper slash handling.
func doRequest(ctx context.Context, client *http.Client, method, baseURL, path, apiKey string, extraHeaders map[string]string, resolver Resolver, body any) (*http.Response, error) {
	resolvedBase, _ := resolver.ResolveValue(baseURL)
	resolvedKey, _ := resolver.ResolveValue(apiKey)

	url := strings.TrimRight(resolvedBase, "/") + "/" + strings.TrimLeft(path, "/")

	var reqBody *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	var req *http.Request
	var err error
	if reqBody != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, reqBody)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return nil, err
	}

	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if resolvedKey != "" {
		req.Header.Set("Authorization", "Bearer "+resolvedKey)
	}
	for k, v := range extraHeaders {
		resolved, err := resolver.ResolveValue(v)
		if err != nil || resolved == "" {
			continue
		}
		req.Header.Set(k, resolved)
	}

	return clientOrDefault(client).Do(req)
}

// Config holds the provider configuration needed for model discovery.
type Config struct {
	ID           string
	BaseURL      string
	APIKey       string
	ExtraHeaders map[string]string
	// HTTPClient carries the provider's outbound network policy (proxy, DNS)
	// resolved by the caller; nil uses the shared default client.
	HTTPClient *http.Client
	// Existing models from config — IDs present in this list are skipped
	// during discovery (user-specified models win).
	ExistingModels []catwalk.Model
}

// Resolver resolves variable references (e.g. $ENV_VAR) in config values.
type Resolver interface {
	ResolveValue(val string) (string, error)
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// DiscoverModels fetches available models from the provider's /models endpoint.
// It uses the provided context for cancellation and timeout; callers should set
// a deadline (e.g. context.WithTimeout) to avoid blocking indefinitely.
// Models whose IDs already appear in cfg.ExistingModels are skipped —
// user-specified models take precedence.
func DiscoverModels(ctx context.Context, cfg Config, resolver Resolver) ([]catwalk.Model, error) {
	resp, err := doRequest(ctx, cfg.HTTPClient, http.MethodGet, cfg.BaseURL, "/models", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return nil, fmt.Errorf("discover models for provider %s: %w", cfg.ID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discover models for provider %s: %s", cfg.ID, resp.Status)
	}

	var modelsResp modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		return nil, fmt.Errorf("discover models for provider %s: %w", cfg.ID, err)
	}

	// Build set of existing model IDs to skip.
	existing := make(map[string]struct{}, len(cfg.ExistingModels))
	for _, m := range cfg.ExistingModels {
		existing[m.ID] = struct{}{}
	}

	// Start with user-specified models.
	result := make([]catwalk.Model, len(cfg.ExistingModels))
	copy(result, cfg.ExistingModels)

	// Append discovered models not already in the list.
	for _, e := range modelsResp.Data {
		if _, ok := existing[e.ID]; ok {
			continue
		}
		result = append(result, catwalk.Model{
			ID:   e.ID,
			Name: e.ID,
		})
	}

	return result, nil
}

// Enricher fills in model metadata (context window, max tokens, pricing,
// etc.) for discovered models. Providers that expose richer endpoints
// beyond /v1/models can register an Enricher to populate fields that the
// standard listing endpoint omits.
type Enricher interface {
	// EnrichModels takes a slice of bare discovered models and returns
	// them with metadata populated. Implementations should preserve
	// existing non-zero fields (user overrides take precedence).
	EnrichModels(ctx context.Context, cfg Config, resolver Resolver, models []catwalk.Model) ([]catwalk.Model, error)
}

// enrichers maps provider type strings to their enrichment
// implementations. Each enricher self-registers via init() so that
// adding a new provider requires only a new file — no changes to
// load.go or any existing enricher.
var enrichers = map[string]Enricher{}

// RegisterEnricher registers an Enricher for the given provider type.
// Called from init() in each enricher implementation file.
func RegisterEnricher(providerType string, e Enricher) {
	enrichers[providerType] = e
}

// GetEnricher returns the Enricher for the given provider type, or nil
// if no enricher is registered.
func GetEnricher(providerType string) Enricher {
	return enrichers[providerType]
}

// IsKnownCustomProvider reports whether the given provider type has a
// registered enricher. This signals that the provider type is
// recognized and speaks OpenAI-compat protocol, without exposing the
// enricher itself to callers that only need the compatibility check.
func IsKnownCustomProvider(providerType string) bool {
	return enrichers[providerType] != nil
}

// RegisteredProviderTypes returns the provider type strings that have a
// registered enricher, sorted for stable output. These are the custom,
// locally-discovered providers (e.g. ollama, omlx) that Rush accepts as
// a `type` value even though they are not catwalk provider types. The
// schema generator uses this so the published enum stays in sync with the
// registry instead of drifting from a hand-maintained list.
func RegisteredProviderTypes() []string {
	types := make([]string, 0, len(enrichers))
	for providerType := range enrichers {
		types = append(types, providerType)
	}
	sort.Strings(types)
	return types
}

const providerCatalogMaxBytes = 10 << 20

// DiscoverProviderCatalog reads the provider's own model roster and optional
// per-model effort metadata. Missing effort metadata remains unknown.
func DiscoverProviderCatalog(ctx context.Context, cfg Config, resolver Resolver) ([]catwalk.Model, error) {
	resp, err := doRequest(ctx, cfg.HTTPClient, http.MethodGet, cfg.BaseURL, "/models", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch %s model catalog: %w", cfg.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s model catalog: HTTP %d", cfg.ID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, providerCatalogMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s model catalog: %w", cfg.ID, err)
	}
	if len(body) > providerCatalogMaxBytes {
		return nil, fmt.Errorf("%s model catalog exceeds size limit", cfg.ID)
	}
	var payload struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode %s model catalog: %w", cfg.ID, err)
	}
	entries := payload.Data
	if entries == nil {
		entries = payload.Models
	}
	models := make([]catwalk.Model, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil {
			continue
		}
		id := codexString(fields["id"])
		if id == "" {
			id = codexString(fields["slug"])
		}
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		name := codexString(fields["display_name"])
		if name == "" {
			name = codexString(fields["name"])
		}
		if name == "" {
			name = id
		}
		levels := codexReasoningLevels(fields["reasoning_effort_support_list"])
		if len(levels) == 0 {
			levels = codexReasoningLevels(fields["supported_reasoning_levels"])
		}
		defaultEffort := codexString(fields["default_reasoning_effort"])
		if defaultEffort == "" {
			defaultEffort = codexString(fields["default_reasoning_level"])
		}
		defaultEffort = strings.ToLower(defaultEffort)
		if len(levels) > 0 && !containsLevel(levels, defaultEffort) {
			defaultEffort = ""
		}
		contextWindow := codexPositiveInt(fields["context_window"])
		models = append(models, catwalk.Model{
			ID:                     id,
			Name:                   name,
			ContextWindow:          contextWindow,
			CanReason:              len(levels) > 0,
			ReasoningLevels:        levels,
			DefaultReasoningEffort: defaultEffort,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%s model catalog returned no models", cfg.ID)
	}
	return models, nil
}

func containsLevel(levels []string, value string) bool {
	for _, level := range levels {
		if level == value {
			return true
		}
	}
	return false
}
