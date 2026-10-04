package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

const (
	// CodexBaseURL is the authenticated ChatGPT backend root used for Codex.
	CodexBaseURL = "https://chatgpt.com/backend-api"
	// CodexClientVersion selects the model catalog version supported by Rush.
	CodexClientVersion                = "0.159.0"
	codexDefaultContextWindow   int64 = 272_000
	codexGPT56ContextWindow     int64 = 372_000
	codexGPT56OneMContextWindow int64 = 1_000_000
	// codexGPT6ContextWindow is the documented window of the GPT-6 family
	// (Astra, Sol, Luna: 1,050,000 context, 922,000 max input, 128,000 max
	// output -- developers.openai.com/api/docs/models/gpt-6-*).
	codexGPT6ContextWindow int64 = 1_050_000
	codexDefaultMaxTokens  int64 = 128_000
	codexMaxResponseBytes        = 10 << 20
)

// DiscoverCodexModels reads the authenticated account's ChatGPT Codex model
// catalog. The access token is Rush's own OAuth credential, not a Codex CLI
// token. Callers should provide a bounded context.
func DiscoverCodexModels(ctx context.Context, accessToken, accountID string) ([]catwalk.Model, error) {
	return discoverCodexModels(ctx, httpClient, CodexBaseURL, accessToken, accountID)
}

func discoverCodexModels(ctx context.Context, client *http.Client, baseURL, accessToken, accountID string) ([]catwalk.Model, error) {
	var lastErr error
	for _, path := range [...]string{"/codex/models", "/models"} {
		u, err := url.Parse(strings.TrimRight(baseURL, "/") + path)
		if err != nil {
			return nil, fmt.Errorf("build Codex model-list URL: %w", err)
		}
		query := u.Query()
		query.Set("client_version", CodexClientVersion)
		u.RawQuery = query.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build Codex model-list request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			req.Header.Set("chatgpt-account-id", accountID)
		}
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("originator", "omp")
		req.Header.Set("version", CodexClientVersion)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request Codex model catalog: %w", err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, codexMaxResponseBytes))
		closeErr := resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read Codex model catalog: %w", readErr)
			continue
		}
		if len(body) == codexMaxResponseBytes {
			lastErr = fmt.Errorf("codex model catalog exceeds %d bytes", codexMaxResponseBytes)
			continue
		}
		if closeErr != nil {
			lastErr = fmt.Errorf("close Codex model catalog response: %w", closeErr)
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("codex model catalog rejected the account (HTTP %d); check that this ChatGPT account has Codex access", resp.StatusCode)
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			lastErr = fmt.Errorf("codex model catalog returned HTTP %d", resp.StatusCode)
			continue
		}

		models, valid, err := parseCodexModelCatalog(body)
		if err != nil {
			lastErr = err
			continue
		}
		if valid {
			return models, nil
		}
		lastErr = fmt.Errorf("codex model catalog response has no models or data array")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("codex model catalog is unavailable")
	}
	return nil, lastErr
}

func parseCodexModelCatalog(body []byte) ([]catwalk.Model, bool, error) {
	var payload struct {
		Models *[]json.RawMessage `json:"models"`
		Data   *[]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("decode Codex model catalog: %w", err)
	}
	entries := payload.Models
	if entries == nil {
		entries = payload.Data
	}
	if entries == nil {
		return nil, false, nil
	}

	models := make([]catwalk.Model, 0, len(*entries))
	for _, raw := range *entries {
		model, ok := parseCodexModel(raw)
		if ok {
			models = append(models, model)
		}
	}
	return models, true, nil
}

func parseCodexModel(raw json.RawMessage) (catwalk.Model, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return catwalk.Model{}, false
	}
	id := codexString(fields["slug"])
	if id == "" {
		id = codexString(fields["id"])
	}
	if id == "" {
		return catwalk.Model{}, false
	}
	// Superseded families (pre-6 GPT, o3, codex-mini) never enter the
	// account catalog; explicitly configured selections keep working
	// without it.
	if !ModelVisible("openai-codex", id) {
		return catwalk.Model{}, false
	}
	visibility := strings.ToLower(codexString(fields["visibility"]))
	if visibility == "hide" || visibility == "hidden" {
		return catwalk.Model{}, false
	}

	contextWindow := codexContextWindow(id, codexPositiveInt(fields["context_window"]))
	maxTokens := codexDefaultMaxTokens
	if contextWindow < maxTokens {
		maxTokens = contextWindow
	}

	defaultEffort := strings.ToLower(codexString(fields["default_reasoning_level"]))
	reasoningLevels := codexReasoningLevels(fields["supported_reasoning_levels"])
	canReason := defaultEffort != "" && defaultEffort != "none" || len(reasoningLevels) > 0

	supportsImages := true
	if rawModalities, ok := fields["input_modalities"]; ok {
		var modalities []string
		if json.Unmarshal(rawModalities, &modalities) == nil {
			supportsImages = false
			for _, modality := range modalities {
				if strings.EqualFold(modality, "image") {
					supportsImages = true
					break
				}
			}
		}
	}

	name := codexString(fields["display_name"])
	if name == "" {
		name = id
	}
	return catwalk.Model{
		ID:                     id,
		Name:                   name,
		ContextWindow:          contextWindow,
		DefaultMaxTokens:       maxTokens,
		CanReason:              canReason,
		ReasoningLevels:        reasoningLevels,
		DefaultReasoningEffort: defaultEffort,
		SupportsImages:         supportsImages,
	}, true
}

func codexContextWindow(id string, reported int64) int64 {
	canonicalID := strings.TrimSuffix(id, "-wm")
	switch canonicalID {
	case "gpt-6-astra", "gpt-6-sol", "gpt-6-luna":
		// A stale or account-reduced catalog value never lowers the
		// documented window of these models.
		if reported < codexGPT6ContextWindow {
			return codexGPT6ContextWindow
		}
	case "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra":
		if reported < codexGPT56OneMContextWindow {
			return codexGPT56OneMContextWindow
		}
	case "gpt-5.6":
		if reported == 0 {
			return codexGPT56ContextWindow
		}
	default:
		if reported == 0 && strings.HasPrefix(canonicalID, "gpt-6") {
			return codexGPT6ContextWindow
		}
		if reported == 0 && strings.HasPrefix(canonicalID, "gpt-5.6-") {
			return codexGPT56ContextWindow
		}
	}
	if reported > 0 {
		return reported
	}
	return codexDefaultContextWindow
}

func codexString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func codexPositiveInt(raw json.RawMessage) int64 {
	var number json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &number) != nil {
		return 0
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || value <= 0 || value >= float64(uint64(1)<<63) {
		return 0
	}
	return int64(value)
}

func codexReasoningLevels(raw json.RawMessage) []string {
	var entries []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	levels := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var effort string
		if json.Unmarshal(entry, &effort) != nil {
			var item struct {
				Effort string `json:"effort"`
			}
			if json.Unmarshal(entry, &item) != nil {
				continue
			}
			effort = item.Effort
		}
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" {
			continue
		}
		if _, ok := seen[effort]; ok {
			continue
		}
		seen[effort] = struct{}{}
		levels = append(levels, effort)
	}
	return levels
}
