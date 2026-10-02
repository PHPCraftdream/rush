package discover

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverProviderCatalogReadsAccountModelsAndEfforts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "Bearer private-key", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data":[
			{"id":"step-5-preview","display_name":"Step 5 Preview","context_window":1000000,
			 "reasoning_effort_support_list":["low","medium","high"],"default_reasoning_effort":"medium"},
			{"id":"step-next","name":"Step Next"},
			{"id":"step-5-preview","reasoning_effort_support_list":["made-up"]}
		]}`))
	}))
	defer server.Close()

	models, err := DiscoverProviderCatalog(t.Context(), Config{ID: "stepfun", BaseURL: server.URL + "/v1", APIKey: "private-key"}, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, "step-5-preview", models[0].ID)
	require.Equal(t, []string{"low", "medium", "high"}, models[0].ReasoningLevels)
	require.Equal(t, "medium", models[0].DefaultReasoningEffort)
	require.EqualValues(t, 1000000, models[0].ContextWindow)
	require.Equal(t, "step-next", models[1].ID)
	require.Empty(t, models[1].ReasoningLevels, "unknown capability must not be guessed")
}

func TestDiscoverProviderCatalogPreservesUnknownZAIReasoningCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-next"},{"id":"glm-detailed",
			"supported_reasoning_levels":[{"effort":"none"},{"effort":"high"}],
			"default_reasoning_level":"high"}]}`))
	}))
	defer server.Close()

	models, err := DiscoverProviderCatalog(t.Context(), Config{ID: "zai", BaseURL: server.URL, APIKey: "private-key"}, &mockResolver{})
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.Equal(t, "glm-next", models[0].ID)
	require.Empty(t, models[0].ReasoningLevels)
	require.Equal(t, []string{"none", "high"}, models[1].ReasoningLevels)
	require.Equal(t, "high", models[1].DefaultReasoningEffort)
}
