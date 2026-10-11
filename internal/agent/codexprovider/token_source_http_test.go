package codexprovider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type goodTokenEndpoint struct {
	good         atomic.Pointer[oauth.Token]
	requests     atomic.Int32
	unauthorized atomic.Int32
}

func (e *goodTokenEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.requests.Add(1)
	good := e.good.Load()
	if good == nil || r.Header.Get("Authorization") != "Bearer "+good.AccessToken || r.Header.Get("chatgpt-account-id") != good.AccountID {
		e.unauthorized.Add(1)
		http.Error(w, "rejected credentials", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(successfulSSE))
}

func TestSourceCurrentGoodTokenLocalEndpoint(t *testing.T) {
	// Revert-check: removing acquireResponse's per-send snapshot sends stale credentials to the endpoint.
	source := newTestSource()
	endpoint := &goodTokenEndpoint{}
	old, _ := source.Current()
	endpoint.good.Store(&old)
	server := httptest.NewServer(endpoint)
	defer server.Close()
	provider, err := NewWithSource(newCodexTestClient(server), source)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	good := rotatedToken()
	endpoint.good.Store(&good)
	source.set(good, true)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	require.EqualValues(t, 2, endpoint.requests.Load())
	require.Zero(t, endpoint.unauthorized.Load())
	require.Zero(t, source.refreshes.Load())
}

func TestSourceLocalEndpoint401RefreshSuccess(t *testing.T) {
	// Revert-check: removing acquireResponse's successful-refresh retry leaves the endpoint's initial 401 terminal.
	source := newTestSource()
	endpoint := &goodTokenEndpoint{}
	good := rotatedToken()
	endpoint.good.Store(&good)
	source.refreshHook = func(context.Context, oauth.Token) error { source.set(good, true); return nil }
	server := httptest.NewServer(endpoint)
	defer server.Close()
	provider, err := NewWithSource(newCodexTestClient(server), source)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	parts, err := collectSourceStream(t.Context(), model)
	require.NoError(t, err)
	deltas, finishes := 0, 0
	for _, part := range parts {
		if part.Type == fantasy.StreamPartTypeTextDelta {
			deltas++
		}
		if part.Type == fantasy.StreamPartTypeFinish {
			finishes++
		}
	}
	require.Equal(t, 1, deltas)
	require.Equal(t, 1, finishes)
	require.EqualValues(t, 2, endpoint.requests.Load())
	require.EqualValues(t, 1, endpoint.unauthorized.Load())
	require.EqualValues(t, 1, source.refreshes.Load())
}

func TestSourceErrorBodyDrainIsBounded(t *testing.T) {
	// Revert-check: an unbounded io.Copy in drainErrorResponse hangs on a never-ending non-2xx body.
	source := newTestSource()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		chunk := make([]byte, 32<<10)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	provider, err := NewWithSource(newCodexTestClient(server), source)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := model.Generate(ctx, fantasy.Call{})
		done <- err
	}()
	select {
	case err := <-done:
		var providerErr *fantasy.ProviderError
		require.ErrorAs(t, err, &providerErr)
		require.Equal(t, http.StatusInternalServerError, providerErr.StatusCode)
	case <-time.After(15 * time.Second):
		t.Fatal("error response drain did not stop at its bound")
	}
}
