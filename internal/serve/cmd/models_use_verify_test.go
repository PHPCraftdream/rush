package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyEnv isolates the config like every models test; online lets the
// verification (not the provider catalog) reach the httptest server.
func verifyEnv(t *testing.T, online bool) string {
	t.Helper()
	globalPath := isolatedModelsEnv(t)
	if online {
		t.Cleanup(config.SetModelVerifyOnlineForTests(true))
	}
	return globalPath
}

func seedCommandVerificationProvider(t *testing.T, globalPath, endpoint string, withKey bool) {
	t.Helper()
	provider := map[string]any{"type": "openai-compat", "base_url": endpoint, "models": []map[string]string{{"id": "seed-model", "name": "Seed Model"}}}
	if withKey {
		provider["api_key"] = "test-key"
	}
	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, json.Unmarshal(data, &cfg))
	cfg["providers"].(map[string]any)["custom"] = provider
	data, err = json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(globalPath, data, 0o600))
}

func savedFastModel(t *testing.T, globalPath string) string {
	t.Helper()
	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var saved struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &saved))
	return string(saved.Models["fast"])
}

func confirmedCustom(globalPath, endpoint string) ([]string, bool) {
	models, fresh := config.CachedConfirmedModels(
		config.ProviderConfig{Type: "openai-compat", BaseURL: endpoint, APIKey: "test-key"}, "custom", config.IdentityResolver())
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids, fresh
}

func TestModelsUse_UnknownModelVerificationMessages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		listed bool
		want   string
		note   bool
	}{
		{name: "listed", listed: true, note: true, want: "note: fast model custom/model-x is set; it was not in the local catalog"},
		{name: "not listed", want: "warning: fast model custom/model-x is set, but custom's model list does not contain it"},
		{name: "unavailable", status: http.StatusInternalServerError, want: "warning: fast model custom/model-x is set but could not be verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalPath := verifyEnv(t, true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				models := []map[string]any{{"id": "other-model", "name": "Other Model", "context_window": 32000}}
				if tc.listed {
					models = append(models, map[string]any{"id": "model-x", "name": "Model X", "max_input_tokens": 64000})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": models})
			}))
			defer server.Close()
			seedCommandVerificationProvider(t, globalPath, server.URL, true)

			resetModelsUseFlags(t)
			var commandErr error
			stderr := captureStderr(t, func() { _, commandErr = runModelsCmd(t, modelsUseCmd, "--fast", "custom/model-x") })
			require.NoError(t, commandErr)
			assert.Contains(t, stderr, tc.want)
			if tc.note {
				assert.Contains(t, stderr, "note:")
				assert.NotContains(t, stderr, "warning:")
			} else {
				assert.Contains(t, stderr, "warning:")
			}
			assert.Contains(t, stderr, "set fast = custom/model-x")
			assert.Contains(t, savedFastModel(t, globalPath), `"model-x"`)
			ids, fresh := confirmedCustom(globalPath, server.URL)
			if tc.listed {
				assert.True(t, fresh)
				assert.Contains(t, ids, "model-x")
				assert.Contains(t, ids, "other-model", "the whole roster is cached, not just the requested model")
			} else if tc.status == 0 {
				assert.True(t, fresh, "a successful roster fetch is cached even when the model is not in it")
			} else {
				assert.False(t, fresh)
			}
		})
	}
}

func TestModelsUseVerifiedRosterSurvivesFreshLoadAndSecondModelNeedsNoRequest(t *testing.T) {
	globalPath := verifyEnv(t, true)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"data":[{"id":"model-one"},{"id":"model-two"}]}`))
	}))
	defer server.Close()
	seedCommandVerificationProvider(t, globalPath, server.URL, true)

	resetModelsUseFlags(t)
	_, err := runModelsCmd(t, modelsUseCmd, "--fast", "custom/model-one")
	require.NoError(t, err)
	require.Equal(t, 1, requests)

	config.ResetProviderCacheForTests()
	loaded, err := config.Load("", filepath.Dir(globalPath), false)
	require.NoError(t, err)
	provider, ok := loaded.Config().Providers.Get("custom")
	require.True(t, ok)
	known := map[string]bool{}
	for _, model := range provider.Models {
		known[model.ID] = true
	}
	require.True(t, known["model-one"] && known["model-two"], "a fresh load merges the whole verified roster: %v", known)

	resetModelsUseFlags(t)
	_, err = runModelsCmd(t, modelsUseCmd, "--smart", "custom/model-two")
	require.NoError(t, err)
	require.Equal(t, 1, requests, "the second model from the cached roster requires no request")
}

func TestModelsUse_InvalidReviewerDoesNotVerifyOrPersistUnknown(t *testing.T) {
	globalPath := verifyEnv(t, true)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "model-x"}}})
	}))
	defer server.Close()
	seedCommandVerificationProvider(t, globalPath, server.URL, true)
	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "--smart", "custom/model-x", "--reviewer", "glm5_3-hihg")
	require.Error(t, runErr)
	assert.Zero(t, requests)
	_, fresh := confirmedCustom(globalPath, server.URL)
	assert.False(t, fresh, "no roster is cached when argument validation fails")
	after, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after))
}

func TestModelsUse_UnknownUnverifiableWithoutKeyStillSets(t *testing.T) {
	globalPath := verifyEnv(t, true)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	seedCommandVerificationProvider(t, globalPath, server.URL, false)
	resetModelsUseFlags(t)
	var commandErr error
	stderr := captureStderr(t, func() { _, commandErr = runModelsCmd(t, modelsUseCmd, "--fast", "custom/model-x") })
	require.NoError(t, commandErr)
	assert.Contains(t, stderr, "could not be verified")
	assert.Contains(t, stderr, "warning:")
	assert.Contains(t, stderr, "set fast = custom/model-x")
	assert.Contains(t, savedFastModel(t, globalPath), `"model-x"`)
	assert.Zero(t, requests, "no API key means no request")
}

func TestModelsUseUnknownModelCacheOnlySkipsRequest(t *testing.T) {
	globalPath := verifyEnv(t, false)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"data":[{"id":"model-x"}]}`))
	}))
	defer server.Close()
	seedCommandVerificationProvider(t, globalPath, server.URL, true)
	resetModelsUseFlags(t)
	var commandErr error
	stderr := captureStderr(t, func() { _, commandErr = runModelsCmd(t, modelsUseCmd, "--fast", "custom/model-x") })
	require.NoError(t, commandErr)
	require.Zero(t, requests)
	require.Contains(t, stderr, "could not be verified")
	require.Contains(t, stderr, "cache-only")
	require.Contains(t, savedFastModel(t, globalPath), `"model-x"`)
}

func TestModelsUseAnthropicRosterIsKnownOnFreshLoad(t *testing.T) {
	globalPath := verifyEnv(t, true)
	var requests int
	var gotKey, gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotKey, gotVersion = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-future-9","display_name":"Claude Future 9","max_input_tokens":2000000,"max_tokens":64000}],"has_more":false}`))
	}))
	defer server.Close()
	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, json.Unmarshal(data, &cfg))
	cfg["providers"].(map[string]any)["anthropic"] = map[string]any{"api_key": "anthropic-test-key", "base_url": server.URL}
	data, err = json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(globalPath, data, 0o600))

	resetModelsUseFlags(t)
	var commandErr error
	stderr := captureStderr(t, func() { _, commandErr = runModelsCmd(t, modelsUseCmd, "--fast", "anthropic/claude-future-9") })
	require.NoError(t, commandErr)
	require.Equal(t, 1, requests)
	require.Equal(t, "anthropic-test-key", gotKey)
	require.Equal(t, "2023-06-01", gotVersion)
	require.Contains(t, stderr, "note: fast model anthropic/claude-future-9 is set")
	require.NotContains(t, stderr, "warning:")

	config.ResetProviderCacheForTests()
	loaded, err := config.Load("", filepath.Dir(globalPath), false)
	require.NoError(t, err)
	provider, ok := loaded.Config().Providers.Get("anthropic")
	require.True(t, ok)
	var found bool
	for _, model := range provider.Models {
		if model.ID == "claude-future-9" {
			found = true
			require.EqualValues(t, 2000000, model.ContextWindow)
		}
	}
	require.True(t, found, "the next load knows the model without a warning")

	resetModelsUseFlags(t)
	stderr = captureStderr(t, func() { _, commandErr = runModelsCmd(t, modelsUseCmd, "--fast", "anthropic/claude-future-9") })
	require.NoError(t, commandErr)
	require.Equal(t, 1, requests, "a known model needs no request")
	require.NotContains(t, stderr, "warning:")
	require.NotContains(t, stderr, "note:")
}
