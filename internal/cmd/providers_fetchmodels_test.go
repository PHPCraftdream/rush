// fetchModels* tests: OpenAI-compat and Anthropic model fetching
// against httptest servers (auth headers, error status, trailing
// slash, context windows) and the static CLI provider model list.
package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

	models, warnings, err := fetchModelsOpenAICompat(srv.URL, "test-key")
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

	_, _, err := fetchModelsOpenAICompat(srv.URL, "bad-key")
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

	models, _, err := fetchModelsOpenAICompat(srv.URL, "")
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

	models, _, err := fetchModelsAnthropic(srv.URL, "test-key")
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

	_, _, err := fetchModelsAnthropic(srv.URL, "bad-key")
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

	_, _, err := fetchModelsOpenAICompat(srv.URL+"/", "key")
	require.NoError(t, err)
	assert.Equal(t, "/models", receivedPath)
}
