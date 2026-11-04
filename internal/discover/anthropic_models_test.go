package discover

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverAnthropicModelsPaginatesAndIncludesFacts(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "secret", r.Header.Get("x-api-key"))
		require.Equal(t, "2023-06-01", r.Header.Get("anthropic-version"))
		if requests == 1 {
			_, _ = w.Write([]byte(`{"data":[{"id":"a","display_name":"A","max_input_tokens":100,"max_tokens":20}],"has_more":true,"last_id":"a"}`))
			return
		}
		require.Equal(t, "a", r.URL.Query().Get("after_id"))
		_, _ = w.Write([]byte(`{"data":[{"id":"b","display_name":"B"}],"has_more":false}`))
	}))
	defer server.Close()
	models, err := DiscoverAnthropicModels(context.Background(), nil, server.URL+"/v1", "secret")
	require.NoError(t, err)
	require.Len(t, models, 2)
	require.EqualValues(t, 100, models[0].ContextWindow)
	require.EqualValues(t, 20, models[0].DefaultMaxTokens)
	require.Equal(t, 2, requests)
}
