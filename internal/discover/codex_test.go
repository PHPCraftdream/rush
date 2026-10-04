package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

func TestDiscoverCodexModelsUsesAccountAndNormalizesMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/models" ||
			r.URL.Query().Get("client_version") != CodexClientVersion ||
			r.Header.Get("Authorization") != "Bearer rush-access-token" ||
			r.Header.Get("chatgpt-account-id") != "chatgpt-account" ||
			r.Header.Get("OpenAI-Beta") != "responses=experimental" ||
			r.Header.Get("originator") != "omp" ||
			r.Header.Get("version") != CodexClientVersion {
			http.Error(w, "invalid Codex account request", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": [
				{
					"slug": "codex-model",
					"display_name": "Codex Model",
					"context_window": 96000,
					"default_reasoning_level": "high",
					"supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}, {"effort": "none"}],
					"input_modalities": ["text"]
				},
				{"slug": "hidden-model", "visibility": "hidden"},
				{"display_name": "missing model id"}
			]
		}`))
	}))
	defer server.Close()

	models, err := discoverCodexModels(context.Background(), server.Client(), server.URL, "rush-access-token", "chatgpt-account")
	require.NoError(t, err)
	require.Len(t, models, 1)
	model := models[0]
	require.Equal(t, "codex-model", model.ID)
	require.Equal(t, "Codex Model", model.Name)
	require.Equal(t, int64(96000), model.ContextWindow)
	require.Equal(t, int64(96000), model.DefaultMaxTokens)
	require.True(t, model.CanReason)
	require.Equal(t, []string{"low", "high", "none"}, model.ReasoningLevels)
	require.Equal(t, "high", model.DefaultReasoningEffort)
	require.False(t, model.SupportsImages)
}

func TestCodexModelCatalogKeepsProviderDefinedEffortDefaults(t *testing.T) {
	model, ok := parseCodexModel(json.RawMessage(`{
		"slug":"account-specific-model",
		"default_reasoning_level":"none",
		"supported_reasoning_levels":[{"effort":"none"},{"effort":"minimal"},{"effort":"ultra"}]
	}`))
	require.True(t, ok)
	require.Equal(t, "none", model.DefaultReasoningEffort)
	require.Equal(t, []string{"none", "minimal", "ultra"}, model.ReasoningLevels)
}

func TestCodexModelCatalogAppliesContextFallbacks(t *testing.T) {
	tests := []struct {
		name          string
		payload       string
		contextWindow int64
	}{
		{
			name:          "GPT-6 Luna floors a stale catalog value to the documented window",
			payload:       `{"slug":"gpt-6-luna","context_window":272000}`,
			contextWindow: 1_050_000,
		},
		{
			name:          "GPT-6 Sol floors a stale catalog value to the documented window",
			payload:       `{"slug":"gpt-6-sol","context_window":372000}`,
			contextWindow: 1_050_000,
		},
		{
			name:          "GPT-6 Astra omitted context uses the documented window",
			payload:       `{"slug":"gpt-6-astra"}`,
			contextWindow: 1_050_000,
		},
		{
			name:          "GPT-6 model with a larger catalog value keeps it",
			payload:       `{"slug":"gpt-6-luna","context_window":2000000}`,
			contextWindow: 2_000_000,
		},
		{
			name:          "later GPT-6 family member omitted context uses the documented window",
			payload:       `{"slug":"gpt-6-orion"}`,
			contextWindow: 1_050_000,
		},
		{
			name:          "GPT-6 Luna floors a stale catalog value to the documented window",
			payload:       `{"slug":"gpt-6-luna","context_window":272000}`,
			contextWindow: codexGPT6ContextWindow,
		},
		{
			name:          "GPT-6 Sol floors a stale catalog value to the documented window",
			payload:       `{"slug":"gpt-6-sol","context_window":372000}`,
			contextWindow: codexGPT6ContextWindow,
		},
		{
			name:          "GPT-6 Astra omitted context uses the documented window",
			payload:       `{"slug":"gpt-6-astra"}`,
			contextWindow: codexGPT6ContextWindow,
		},
		{
			name:          "GPT-6 model with a larger catalog value keeps it",
			payload:       `{"slug":"gpt-6-luna","context_window":2000000}`,
			contextWindow: 2_000_000,
		},
		{
			name:          "later GPT-6 family member omitted context uses the documented window",
			payload:       `{"slug":"gpt-6-orion"}`,
			contextWindow: codexGPT6ContextWindow,
		},
		{
			name:          "other models use general default",
			payload:       `{"slug":"codex-model"}`,
			contextWindow: codexDefaultContextWindow,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model, ok := parseCodexModel(json.RawMessage(test.payload))
			require.True(t, ok)
			require.Equal(t, test.contextWindow, model.ContextWindow)
		})
	}
}

// TestCodexModelCatalogDropsSupersededIds is the rewritten form of the
// former gpt-5.6 context-fallback rows: since #1171 the pre-6 families are
// filtered at parse time, so their context branches are unreachable through
// the account catalog and the observable behavior is the drop itself.
func TestCodexModelCatalogDropsSupersededIds(t *testing.T) {
	for _, id := range []string{"gpt-5.6-orion", "gpt-5.6-luna", "gpt-5.5", "o3", "codex-mini-latest"} {
		_, ok := parseCodexModel(json.RawMessage(`{"slug":"` + id + `"}`))
		require.False(t, ok, "superseded id %q must not enter the Codex catalog", id)
	}
}

func TestDiscoverCodexModelsFallsBackToModelsRoute(t *testing.T) {
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if r.URL.Path == "/codex/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fallback-model"}]}`))
	}))
	defer server.Close()

	models, err := discoverCodexModels(context.Background(), server.Client(), server.URL, "rush-access-token", "")
	require.NoError(t, err)
	require.Equal(t, []string{"/codex/models", "/models"}, []string{<-requests, <-requests})
	require.Len(t, models, 1)
	require.Equal(t, "fallback-model", models[0].ID)
	require.Equal(t, codexDefaultContextWindow, models[0].ContextWindow)
}

func TestDiscoverCodexModelsStopsOnAccountAccessRejection(t *testing.T) {
	var fallbackRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fallbackRequests.Add(1)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	models, err := discoverCodexModels(context.Background(), server.Client(), server.URL, "invalid-token", "no-access")
	require.ErrorContains(t, err, "has Codex access")
	require.Nil(t, models)
	require.Zero(t, fallbackRequests.Load(), "a rejected ChatGPT account must not be masked by trying the fallback route")
}

// TestDiscoverCodexModelsHidesSupersededFamilies verifies the account
// catalog itself never contains the hidden families (#1171): pre-6 GPT
// models, o3 and codex-mini are dropped at parse time, gpt-6 models and
// unknown ids survive, and surviving gpt-6 entries carry the documented
// context window with max tokens clamped to it.
func TestDiscoverCodexModelsHidesSupersededFamilies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"models": [
				{"slug": "gpt-6-luna", "context_window": 272000},
				{"slug": "gpt-6-sol"},
				{"slug": "gpt-5.6-luna"},
				{"slug": "gpt-5.6"},
				{"slug": "o3"},
				{"slug": "codex-mini-latest"},
				{"slug": "brand-new-family"}
			]
		}`))
	}))
	defer server.Close()

	models, err := discoverCodexModels(context.Background(), server.Client(), server.URL, "rush-access-token", "")
	require.NoError(t, err)

	ids := make([]string, 0, len(models))
	byID := make(map[string]catwalk.Model, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
		byID[model.ID] = model
	}
	require.Equal(t, []string{"gpt-6-luna", "gpt-6-sol", "brand-new-family"}, ids)

	luna := byID["gpt-6-luna"]
	require.Equal(t, int64(1_050_000), luna.ContextWindow,
		"the documented GPT-6 window is asserted as a literal so a constant edit cannot pass silently")
	require.Equal(t, codexDefaultMaxTokens, luna.DefaultMaxTokens)
	require.LessOrEqual(t, luna.DefaultMaxTokens, luna.ContextWindow)
	sol := byID["gpt-6-sol"]
	require.Equal(t, int64(1_050_000), sol.ContextWindow)
	require.LessOrEqual(t, sol.DefaultMaxTokens, sol.ContextWindow)
}
