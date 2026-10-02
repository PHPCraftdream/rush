package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

const providerCatalogMaxBytes = 10 << 20

// DiscoverProviderCatalog reads the provider's own model roster and optional
// per-model effort metadata. Missing effort metadata remains unknown.
func DiscoverProviderCatalog(ctx context.Context, cfg Config, resolver Resolver) ([]catwalk.Model, error) {
	resp, err := doRequest(ctx, http.MethodGet, cfg.BaseURL, "/models", cfg.APIKey, cfg.ExtraHeaders, resolver, nil)
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
