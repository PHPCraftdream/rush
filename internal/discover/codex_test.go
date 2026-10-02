package discover

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
	require.Equal(t, []string{"low", "high"}, model.ReasoningLevels)
	require.Equal(t, "high", model.DefaultReasoningEffort)
	require.False(t, model.SupportsImages)
}
func TestCodexModelCatalogAppliesContextFallbacks(t *testing.T) {
	tests := []struct {
		name          string
		payload       string
		contextWindow int64
	}{
		{
			name:          "GPT-5.6 family omitted context",
			payload:       `{"slug":"gpt-5.6-orion"}`,
			contextWindow: codexGPT56ContextWindow,
		},
		{
			name:          "GPT-5.6 one-million-token model floors stale catalog value",
			payload:       `{"slug":"gpt-5.6-luna","context_window":272000}`,
			contextWindow: codexGPT56OneMContextWindow,
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
