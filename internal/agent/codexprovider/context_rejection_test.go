package codexprovider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type rejectionTrace struct {
	Method            string
	PathClass         string
	FixtureTokenMatch bool
}

type rejectionDiagnostic struct {
	mu         sync.Mutex
	caseName   string
	requests   int
	status     int
	certified  bool
	eventCount int
	trace      []rejectionTrace
}

func newRejectionDiagnostic(t *testing.T, caseName string) *rejectionDiagnostic {
	t.Helper()
	d := &rejectionDiagnostic{caseName: caseName}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		traces := d.trace
		if len(traces) > 4 {
			traces = traces[:4]
		}
		t.Logf("provider rejection diagnostic case=%q requests=%d status=%d certified=%t events=%d trace=%+v", d.caseName, d.requests, d.status, d.certified, d.eventCount, traces)
	})
	return d
}

func (d *rejectionDiagnostic) request(method, path string, fixtureTokenMatch bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests++
	pathClass := "other"
	if path == "/backend-api/codex/responses" {
		pathClass = "responses"
	}
	if len(d.trace) < 4 {
		d.trace = append(d.trace, rejectionTrace{Method: method, PathClass: pathClass, FixtureTokenMatch: fixtureTokenMatch})
	}
}

func (d *rejectionDiagnostic) result(status int, certified bool, eventCount int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status = status
	d.certified = certified
	d.eventCount = eventCount
}

type contextRejectedBeforeOutput interface {
	CodexContextRejectedBeforeOutput() bool
}

func isolateContextRejectionTestPaths(t *testing.T) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
}

func certifiedBeforeOutput(err error) bool {
	var marker contextRejectedBeforeOutput
	return errors.As(err, &marker) && marker.CodexContextRejectedBeforeOutput()
}

func TestCodexStreamFailureRequiresPreOutputResponseFailed(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	const failure = `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"}}}`
	type failureCase struct {
		name            string
		before          []string
		certified       bool
		contextTooLarge bool
		failure         string
	}
	cases := []failureCase{
		{name: "no output", certified: true, contextTooLarge: true},
		{name: "created then queued", before: []string{`{"type":"response.created"}`, `{"type":"response.queued"}`}, certified: true, contextTooLarge: true},
		{name: "lifecycle contains item", before: []string{`{"type":"response.created","item":{"type":"message"}}`}, contextTooLarge: true},
		{name: "lifecycle response has output", before: []string{`{"type":"response.in_progress","response":{"output":[{"type":"message"}]}}`}, contextTooLarge: true},
		{name: "partial text", before: []string{`{"type":"response.output_text.delta","delta":"partial"}`}, contextTooLarge: true},
		{name: "reasoning", before: []string{`{"type":"response.reasoning_summary_text.delta","delta":"private reasoning"}`}, contextTooLarge: true},
		{name: "reasoning item", before: []string{`{"type":"response.output_item.added","item":{"type":"reasoning"}}`}, contextTooLarge: true},
		{name: "tool item", before: []string{`{"type":"response.output_item.added","item":{"id":"fc","type":"function_call","call_id":"call_fc","name":"lookup"}}`}, contextTooLarge: true},
		{name: "encrypted content item", before: []string{`{"type":"response.output_item.done","item":{"type":"encrypted_content"}}`}, contextTooLarge: true},
		{name: "native item", before: []string{`{"type":"response.output_item.added","item":{"type":"image_generation_call"}}`}, contextTooLarge: true},
		{name: "empty function arguments delta", before: []string{`{"type":"response.function_call_arguments.delta","item_id":"fc","delta":""}`}, contextTooLarge: true},
		{name: "empty function arguments done", before: []string{`{"type":"response.function_call_arguments.done","item_id":"fc","arguments":""}`}, contextTooLarge: true},
		{name: "unknown event", before: []string{`{"type":"response.future_event"}`}, contextTooLarge: true},
		{name: "empty completed", before: []string{`{"type":"response.completed","response":{}}`}, contextTooLarge: true},
		{name: "empty incomplete", before: []string{`{"type":"response.incomplete","response":{}}`}, contextTooLarge: true},
		{name: "generic error event is not certified", before: []string{`{"type":"response.created"}`}, failure: `{"type":"error","error":{"code":"context_length_exceeded","message":"too large"}}`, contextTooLarge: true},
		{name: "failure embeds output item", failure: `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"},"output":[{"type":"message"}]}}`, contextTooLarge: true},
		{name: "failure embeds response text", failure: `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"},"output_text":"partial"}}`, contextTooLarge: true},
		{name: "failure embeds direct delta", failure: `{"type":"response.failed","delta":"partial","response":{"error":{"code":"context_length_exceeded","message":"too large"}}}`, contextTooLarge: true},
		{name: "suffix code mismatch", failure: `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded_extra","message":"too large"}}}`},
		{name: "case mismatch", failure: `{"type":"response.failed","response":{"error":{"code":"Context_length_exceeded","message":"too large"}}}`},
		{name: "whitespace mismatch", failure: `{"type":"response.failed","response":{"error":{"code":" context_length_exceeded","message":"too large"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diagnostic := newRejectionDiagnostic(t, tc.name)
			state := newStreamState()
			for _, event := range tc.before {
				_ = state.consume([]byte(event), func(fantasy.StreamPart) bool { return true })
			}
			failureEvent := failure
			if tc.failure != "" {
				failureEvent = tc.failure
			}
			err := state.consume([]byte(failureEvent), func(fantasy.StreamPart) bool { return true })
			require.Error(t, err)
			certified := certifiedBeforeOutput(err)
			diagnostic.result(0, certified, len(tc.before)+1)
			require.Equal(t, tc.certified, certified, "accepted_output=%t", state.acceptedOutput)
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, fmt.Errorf("wrapped failure: %w", err), &providerErr)
			require.Equal(t, tc.contextTooLarge, providerErr.IsContextTooLarge())
		})
	}
}

func TestSafeContextRejectionUnwrapsProviderError(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	const failure = `{"type":"response.failed","response":{"error":{"type":"context_length_exceeded","message":"too large"}}}`
	err := codexStreamFailure([]byte(failure), true)
	require.True(t, certifiedBeforeOutput(err))
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.True(t, providerErr.IsContextTooLarge())
	require.Equal(t, failure, string(providerErr.ResponseBody))
}

func TestHTTPContextRejectionCertificationAndSingleRequest(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	cases := []struct {
		name            string
		status          int
		body            string
		certified       bool
		contextTooLarge bool
	}{
		{name: "exact code at bad request", status: http.StatusBadRequest, body: `{"error":{"code":"context_length_exceeded","message":"too large"}}`, certified: true, contextTooLarge: true},
		{name: "exact type at bad request", status: http.StatusBadRequest, body: `{"error":{"type":"context_length_exceeded","message":"too large"}}`, certified: true, contextTooLarge: true},
		{name: "wrong code at bad request", status: http.StatusBadRequest, body: `{"error":{"code":"context_length_exceeded_extra","message":"too large"}}`},
		{name: "output alongside exact error at bad request", status: http.StatusBadRequest, body: `{"error":{"code":"context_length_exceeded","message":"too large"},"output":[{"type":"message"}]}`, contextTooLarge: true},
		{name: "usage alongside exact error at bad request", status: http.StatusBadRequest, body: `{"error":{"code":"context_length_exceeded","message":"too large"},"usage":{"input_tokens":0}}`, contextTooLarge: true},
		{name: "exact code at another status", status: http.StatusUnprocessableEntity, body: `{"error":{"code":"context_length_exceeded","message":"too large"}}`, contextTooLarge: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diagnostic := newRejectionDiagnostic(t, tc.name)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				diagnostic.request(r.Method, r.URL.Path, r.Header.Get("Authorization") == "Bearer chatgpt-oauth-access-token")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(server.Close)
			model := codexProviderForTest(t, server)
			_, err := model.Generate(t.Context(), fantasy.Call{})
			require.Error(t, err)
			certified := certifiedBeforeOutput(err)
			diagnostic.result(tc.status, certified, 1)
			require.Equal(t, tc.certified, certified, "requests=%d", requests.Load())
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, tc.status, providerErr.StatusCode)
			require.Equal(t, tc.body, string(providerErr.ResponseBody))
			require.Equal(t, tc.contextTooLarge, providerErr.IsContextTooLarge())
			require.EqualValues(t, 1, requests.Load(), "status=%d body_bytes=%d", providerErr.StatusCode, len(providerErr.ResponseBody))
		})
	}
}

func TestHTTPContextRejectionCancellationTakesPrecedence(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	diagnostic := newRejectionDiagnostic(t, "HTTP 400 cancelled")
	started := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		diagnostic.request(r.Method, r.URL.Path, r.Header.Get("Authorization") == "Bearer chatgpt-oauth-access-token")
		w.WriteHeader(http.StatusBadRequest)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	model := codexProviderForTest(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := model.Generate(ctx, fantasy.Call{})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		diagnostic.result(http.StatusBadRequest, false, 0)
		t.Fatalf("request did not reach fake server; requests=%d", requests.Load())
	}
	cancel()
	select {
	case err := <-done:
		diagnostic.result(http.StatusBadRequest, certifiedBeforeOutput(err), 0)
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, certifiedBeforeOutput(err), "requests=%d", requests.Load())
	case <-time.After(2 * time.Second):
		diagnostic.result(http.StatusBadRequest, false, 0)
		t.Fatalf("request did not stop after cancellation; requests=%d", requests.Load())
	}
	require.EqualValues(t, 1, requests.Load())
}

func TestCodexStreamFailureOneArgumentRemainsUncertified(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	err := codexStreamFailure([]byte(`{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"}}}`))
	require.False(t, certifiedBeforeOutput(err))
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.True(t, providerErr.IsContextTooLarge())
}

var _ contextRejectedBeforeOutput = (*safeContextRejection)(nil)
var _ error = (*safeContextRejection)(nil)
