// fetchModels* tests: OpenAI-compat and Anthropic model fetching
// against httptest servers (auth headers, error status, trailing
// slash, context windows) and the static CLI provider model list.
package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchModelsOpenAICompat(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		resp := struct {
			Data []struct {
				ID            string `json:"id"`
				ContextWindow int64  `json:"context_window,omitempty"`
			} `json:"data"`
		}{
			Data: []struct {
				ID            string `json:"id"`
				ContextWindow int64  `json:"context_window,omitempty"`
			}{
				{ID: "model-a", ContextWindow: 8192},
				{ID: "model-b"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	models, warnings, err := fetchModelsOpenAICompat(nil, srv.URL, "test-key")
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "model-a", models[0].ID)
	assert.Equal(t, int64(8192), models[0].ContextWindow)
	assert.Equal(t, "model-b", models[1].ID)
	assert.Equal(t, int64(0), models[1].ContextWindow)
	assert.NotEmpty(t, warnings)
}

func TestFetchModelsOpenAICompat_Unauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("invalid api key"))
	}))
	defer srv.Close()

	_, _, err := fetchModelsOpenAICompat(nil, srv.URL, "bad-key")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
}

func TestFetchModelsOpenAICompat_NoAuth(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		resp := struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}{
			Data: []struct {
				ID string `json:"id"`
			}{{ID: "model-x"}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	models, _, err := fetchModelsOpenAICompat(nil, srv.URL, "")
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "model-x", models[0].ID)
}

func TestFetchModelsAnthropic(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/models", r.URL.Path)
		assert.Equal(t, "test-key", r.Header.Get("x-api-key"))
		assert.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))

		resp := struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}{
			Data: []struct {
				ID string `json:"id"`
			}{
				{ID: "claude-sonnet-4"},
				{ID: "claude-opus-4"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	models, _, err := fetchModelsAnthropic(nil, srv.URL, "test-key")
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "claude-sonnet-4", models[0].ID)
	assert.Equal(t, "claude-opus-4", models[1].ID)
}

func TestFetchModelsAnthropic_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("forbidden"))
	}))
	defer srv.Close()

	_, _, err := fetchModelsAnthropic(nil, srv.URL, "bad-key")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 403")
}

func TestFetchModelsCLI(t *testing.T) {
	t.Parallel()
	models := fetchModelsCLI()
	for _, m := range models {
		assert.NotEmpty(t, m.ID, "CLI model should have an ID")
		assert.NotEmpty(t, m.Name, "CLI model should have a Name")
	}
}

func TestFetchModelsOpenAICompat_TrailingSlash(t *testing.T) {
	t.Parallel()

	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		resp := struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}{}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	_, _, err := fetchModelsOpenAICompat(nil, srv.URL+"/", "key")
	require.NoError(t, err)
	assert.Equal(t, "/models", receivedPath)
}

// TestProvidersFetchModelsHelpDocumentsVisibilityFilter is the help
// contract for the `providers fetch-models` surface of the same filter.
func TestProvidersFetchModelsHelpDocumentsVisibilityFilter(t *testing.T) {
	t.Parallel()

	assert.Contains(t, providersFetchModelsCmd.Long, "gpt-6",
		"fetch-models help must name the openai-codex minimum")
	assert.Contains(t, providersFetchModelsCmd.Long, "5.3",
		"fetch-models help must name the glm minimum version")
}

// TestFetchModelsHidesSupersededFamilies covers the `providers fetch-models` /
// `providers update` path: a provider that serves old GLM ids next to a current
// one returns only the visible ones, and an unparseable id is kept.
//
// Revert-check: make fetchModels return the unfiltered list (drop the
// discover.ModelVisible check) and the glm-5.2 / glm-4.5v ids come back.
func TestFetchModelsHidesSupersededFamilies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-4.5v"},{"id":"glm-5.2"},{"id":"glm-5.3"},{"id":"glm-x"}]}`))
	}))
	defer server.Close()

	// a is only needed to resolve "$VAR" templates; none are used here.
	models, _, err := fetchModels(nil, config.ProviderConfig{
		ID: "zai", Type: "openai-compat", BaseURL: server.URL,
	})
	require.NoError(t, err)

	var ids []string
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	require.Equal(t, []string{"glm-5.3", "glm-x"}, ids)
}
