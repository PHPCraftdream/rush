package codexprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func isolateCodexFailureTestPaths(t *testing.T) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
}

func TestContextLengthExceededFailureClassification(t *testing.T) {
	isolateCodexFailureTestPaths(t)
	cases := []struct {
		name, mode, body, message string
		status                    int
		want                      bool
	}{
		{"HTTP code", "http", `{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"too large"}}`, "context_length_exceeded: too large", http.StatusBadRequest, true},
		{"HTTP type", "http", `{"error":{"code":"invalid_request_error","type":"context_length_exceeded","message":"too large"}}`, "invalid_request_error: too large", http.StatusBadRequest, true},
		{"SSE response.failed code", "response.failed", `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"too large"}}}`, "context_length_exceeded: too large", 0, true},
		{"SSE error type", "error", `{"type":"error","error":{"code":"invalid_request_error","type":"context_length_exceeded","message":"too large"}}`, "invalid_request_error: too large", 0, true},
		{"SSE case differs", "error", `{"type":"error","error":{"code":"Context_length_exceeded","message":"too large"}}`, "Context_length_exceeded: too large", 0, false},
		{"SSE whitespace differs", "error", `{"type":"error","error":{"code":" context_length_exceeded","message":"too large"}}`, " context_length_exceeded: too large", 0, false},
		{"HTTP suffix differs", "http", `{"error":{"code":"context_length_exceeded_extra","message":"too large"}}`, "context_length_exceeded_extra: too large", http.StatusBadRequest, false},
		{"SSE unrelated structured code", "error", `{"type":"error","error":{"code":"invalid_request_error","type":"invalid_request_error","message":"context length exceeded"}}`, "invalid_request_error: context length exceeded", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				if tc.mode == "http" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				writeCodexEvent(w, tc.body)
			}))
			t.Cleanup(server.Close)

			model := codexProviderForTest(t, server)
			_, err := model.Generate(t.Context(), fantasy.Call{})
			require.Error(t, err)
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, tc.status, providerErr.StatusCode, "mode=%s requests=%d body_bytes=%d", tc.mode, requests.Load(), len(providerErr.ResponseBody))
			require.Equal(t, tc.body, string(providerErr.ResponseBody), "mode=%s requests=%d body_bytes=%d", tc.mode, requests.Load(), len(providerErr.ResponseBody))
			require.Equal(t, tc.want, providerErr.IsContextTooLarge(), "status=%d body_bytes=%d requests=%d", providerErr.StatusCode, len(providerErr.ResponseBody), requests.Load())
			require.Equal(t, tc.message, providerErr.Message, "status=%d body_bytes=%d requests=%d", providerErr.StatusCode, len(providerErr.ResponseBody), requests.Load())
			require.False(t, providerErr.IsRetryable(), "status=%d requests=%d", providerErr.StatusCode, requests.Load())
			require.EqualValues(t, 1, requests.Load(), "request count=%d mode=%s", requests.Load(), tc.mode)
		})
	}
}

func TestContextLengthExceededFlagSurvivesWrappingAndPartialSSE(t *testing.T) {
	isolateCodexFailureTestPaths(t)
	const failure = `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"}}}`
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexEvent(w, `{"type":"response.output_text.delta","delta":"partial"}`)
		writeCodexEvent(w, failure)
	}))
	t.Cleanup(server.Close)

	model := codexProviderForTest(t, server)
	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	var providerErr *fantasy.ProviderError
	for _, part := range parts {
		if part.Type == fantasy.StreamPartTypeError {
			require.Error(t, part.Error)
			require.ErrorAs(t, fmt.Errorf("wrapped stream failure: %w", part.Error), &providerErr)
		}
		require.NotEqual(t, fantasy.StreamPartTypeFinish, part.Type)
	}
	require.NotNil(t, providerErr, "stream parts=%d requests=%d", len(parts), requests.Load())
	require.True(t, providerErr.IsContextTooLarge(), "status=%d body_bytes=%d requests=%d", providerErr.StatusCode, len(providerErr.ResponseBody), requests.Load())
	require.Zero(t, providerErr.StatusCode, "requests=%d body_bytes=%d", requests.Load(), len(providerErr.ResponseBody))
	require.Equal(t, failure, string(providerErr.ResponseBody), "requests=%d body_bytes=%d", requests.Load(), len(providerErr.ResponseBody))
	require.False(t, providerErr.IsRetryable(), "status=%d requests=%d", providerErr.StatusCode, requests.Load())
	require.EqualValues(t, 1, requests.Load(), "request count=%d stream parts=%d", requests.Load(), len(parts))
	var sawPartial bool
	for _, part := range parts {
		if part.Type == fantasy.StreamPartTypeTextDelta && part.Delta == "partial" {
			sawPartial = true
		}
	}
	require.True(t, sawPartial)
}

func TestContextLengthExceededMalformedFailureDoesNotClassify(t *testing.T) {
	isolateCodexFailureTestPaths(t)
	for _, body := range []string{
		`{"error":{"code":"context_length_exceeded_extra","message":"too large"}}`,
		`{"error":{"type":"Context_Length_Exceeded","message":"too large"}}`,
		`{"error":{"message":"context_length_exceeded"}}`,
	} {
		err := codexStreamFailure([]byte(body))
		var providerErr *fantasy.ProviderError
		require.ErrorAs(t, err, &providerErr)
		require.False(t, providerErr.IsContextTooLarge())
		require.Zero(t, providerErr.StatusCode)
		require.Equal(t, body, string(providerErr.ResponseBody))
	}
}

func TestContextLengthExceededCancellationRemainsCancellation(t *testing.T) {
	isolateCodexFailureTestPaths(t)
	started := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": waiting\n\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			http.Error(w, "bounded cancellation fixture timeout", http.StatusRequestTimeout)
		}
	}))
	t.Cleanup(server.Close)
	model := codexProviderForTest(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := model.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)
	parts := make(chan fantasy.StreamPart, 1)
	go func() {
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeError {
				parts <- part
				return
			}
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Codex request did not reach fake server")
	}
	cancel()
	select {
	case part := <-parts:
		require.Error(t, part.Error, "requests=%d", requests.Load())
		require.True(t, errors.Is(part.Error, context.Canceled), "requests=%d error=%v", requests.Load(), part.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("Codex stream did not stop after cancellation")
	}
	require.EqualValues(t, 1, requests.Load())
}
