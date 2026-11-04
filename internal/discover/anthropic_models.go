package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

const anthropicModelsMaxPages = 4

// DiscoverAnthropicModels fetches the complete Anthropic model list, bounded
// to four pages so malformed pagination cannot cause unbounded requests.
func DiscoverAnthropicModels(ctx context.Context, baseURL, apiKey string) ([]catwalk.Model, error) {
	base := strings.TrimRight(baseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	endpoint := base + "/v1/models"
	models := make([]catwalk.Model, 0)
	lastID := ""
	for page := 0; page < anthropicModelsMaxPages; page++ {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("parse Anthropic models URL: %w", err)
		}
		query := parsed.Query()
		query.Set("limit", "100")
		if lastID != "" {
			query.Set("after_id", lastID)
		}
		parsed.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch Anthropic models: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, providerCatalogMaxBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch Anthropic models: HTTP %d", resp.StatusCode)
		}
		if len(body) > providerCatalogMaxBytes {
			return nil, fmt.Errorf("anthropic model catalog exceeds size limit")
		}
		var payload struct {
			Data []struct {
				ID             string `json:"id"`
				DisplayName    string `json:"display_name"`
				MaxInputTokens int64  `json:"max_input_tokens"`
				MaxTokens      int64  `json:"max_tokens"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode Anthropic model catalog: %w", err)
		}
		for _, item := range payload.Data {
			if item.ID == "" {
				continue
			}
			name := item.DisplayName
			if name == "" {
				name = item.ID
			}
			model := catwalk.Model{ID: item.ID, Name: name, ContextWindow: item.MaxInputTokens, DefaultMaxTokens: item.MaxTokens}
			models = append(models, model)
		}
		if !payload.HasMore {
			return models, nil
		}
		if payload.LastID == "" || payload.LastID == lastID || page == anthropicModelsMaxPages-1 {
			return nil, fmt.Errorf("anthropic model pagination did not complete")
		}
		lastID = payload.LastID
	}
	return models, nil
}
