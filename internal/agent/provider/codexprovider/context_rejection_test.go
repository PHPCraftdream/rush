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

func TestCodexStreamFailureCertifiesOnlyExactPreOutputContextRejections(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	failureForms := []struct {
		name  string
		event string
	}{
		{name: "response.failed", event: `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"}}}`},
		{name: "nested error", event: `{"type":"error","sequence_number":1,"error":{"type":"context_length_exceeded","message":"too large"}}`},
		{name: "flat Responses error", event: `{"type":"error","code":"context_length_exceeded","message":"too large","param":null,"sequence_number":1}`},
	}
	beforeCases := []struct {
		name        string
		events      []string
		certified   bool
		contextSize bool
	}{
		{name: "no preceding event", certified: true, contextSize: true},
		{name: "created then queued", events: []string{`{"type":"response.created"}`, `{"type":"response.queued"}`}, certified: true, contextSize: true},
		{name: "supported lifecycle metadata", events: []string{`{"type":"response.in_progress","response":{"reasoning":{"effort":"high","summary":"auto","generate_summary":"concise"},"text":{"format":{"type":"text"},"verbosity":"medium"},"output":[],"usage":null}}`}, certified: true, contextSize: true},
		{name: "nullable lifecycle options", events: []string{`{"type":"response.in_progress","response":{"reasoning":{"effort":null,"summary":null,"generate_summary":null},"text":{"format":{"type":"text"},"verbosity":null},"output":[],"usage":null}}`}, certified: true, contextSize: true},
		{name: "lifecycle metadata malformed", events: []string{`{"type":"response.in_progress","response":{"reasoning":{"effort":"mystery"},"output":[],"usage":null}}`}, contextSize: true},
		{name: "lifecycle metadata unknown", events: []string{`{"type":"response.in_progress","response":{"text":{"future_option":"x"},"output":[],"usage":null}}`}, contextSize: true},
		{name: "lifecycle contains item", events: []string{`{"type":"response.created","item":{"type":"message"}}`}, contextSize: true},
		{name: "lifecycle response has usage", events: []string{`{"type":"response.in_progress","response":{"usage":{"input_tokens":0}}}`}, contextSize: true},
		{name: "partial text", events: []string{`{"type":"response.output_text.delta","delta":"partial"}`}, contextSize: true},
		{name: "reasoning", events: []string{`{"type":"response.reasoning_summary_text.delta","delta":"private reasoning"}`}, contextSize: true},
		{name: "reasoning item", events: []string{`{"type":"response.output_item.added","item":{"type":"reasoning"}}`}, contextSize: true},
		{name: "tool item", events: []string{`{"type":"response.output_item.added","item":{"id":"fc","type":"function_call","call_id":"call_fc","name":"lookup"}}`}, contextSize: true},
		{name: "encrypted content item", events: []string{`{"type":"response.output_item.done","item":{"type":"encrypted_content"}}`}, contextSize: true},
		{name: "native item", events: []string{`{"type":"response.output_item.added","item":{"type":"image_generation_call"}}`}, contextSize: true},
		{name: "empty function arguments delta", events: []string{`{"type":"response.function_call_arguments.delta","item_id":"fc","delta":""}`}, contextSize: true},
		{name: "empty function arguments done", events: []string{`{"type":"response.function_call_arguments.done","item_id":"fc","arguments":""}`}, contextSize: true},
		{name: "unknown event", events: []string{`{"type":"response.future_event"}`}, contextSize: true},
		{name: "empty completed", events: []string{`{"type":"response.completed","response":{}}`}, contextSize: true},
		{name: "empty incomplete", events: []string{`{"type":"response.incomplete","response":{}}`}, contextSize: true},
	}
	for _, form := range failureForms {
		for _, tc := range beforeCases {
			t.Run(form.name+"/"+tc.name, func(t *testing.T) {
				state := newStreamState()
				for _, event := range tc.events {
					_ = state.consume([]byte(event), func(fantasy.StreamPart) bool { return true })
				}
				err := state.consume([]byte(form.event), func(fantasy.StreamPart) bool { return true })
				require.Error(t, err)
				require.Equal(t, tc.certified, certifiedBeforeOutput(err), "accepted_output=%t", state.acceptedOutput)
				var providerErr *fantasy.ProviderError
				require.ErrorAs(t, err, &providerErr)
				require.Equal(t, tc.contextSize, providerErr.IsContextTooLarge())
			})
		}
	}
}

func TestCodexStreamFailurePayloadGuardsAndExactCodes(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	cases := []struct {
		name        string
		event       string
		certified   bool
		contextSize bool
	}{
		{name: "response failed output item", event: `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"},"output":[{"type":"message"}]}}`, certified: false, contextSize: true},
		{name: "response failed response text", event: `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"},"output_text":"partial"}}`, certified: false, contextSize: true},
		{name: "response failed direct delta", event: `{"type":"response.failed","delta":"partial","response":{"error":{"code":"context_length_exceeded","message":"too large"}}}`, certified: false, contextSize: true},
		{name: "nested error output item", event: `{"type":"error","error":{"type":"context_length_exceeded","message":"too large"},"output":[{"type":"message"}]}`, certified: false, contextSize: true},
		{name: "flat error usage", event: `{"type":"error","code":"context_length_exceeded","message":"too large","usage":{"input_tokens":0}}`, certified: false, contextSize: true},
		{name: "flat error unknown field", event: `{"type":"error","code":"context_length_exceeded","message":"too large","future_field":true}`, certified: false, contextSize: true},
		{name: "nested error unknown field", event: `{"type":"error","error":{"type":"context_length_exceeded","message":"too large","details":"opaque"}}`, certified: false, contextSize: true},
		{name: "nested and flat fields ambiguous", event: `{"type":"error","code":"context_length_exceeded","message":"too large","error":{"code":"context_length_exceeded"}}`, certified: false, contextSize: true},
		{name: "flat error missing message", event: `{"type":"error","code":"context_length_exceeded"}`, certified: true, contextSize: true},
		{name: "response.failed nested error wrong code and message", event: `{"type":"response.failed","response":{"error":{"code":"invalid_prompt","message":"context_length_exceeded"}}}`, certified: false, contextSize: false},
		{name: "wrong exact code", event: `{"type":"error","code":"context_length_exceeded_extra","message":"too large"}`, certified: false, contextSize: false},
		{name: "response.failed nested error wrong type", event: `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":42}}}`, certified: false, contextSize: true},
		{name: "case mismatch", event: `{"type":"response.failed","response":{"error":{"code":"Context_length_exceeded","message":"too large"}}}`, certified: false, contextSize: false},
		{name: "whitespace mismatch", event: `{"type":"response.failed","response":{"error":{"code":" context_length_exceeded","message":"too large"}}}`, certified: false, contextSize: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := newStreamState()
			err := state.consume([]byte(tc.event), func(fantasy.StreamPart) bool { return true })
			require.Error(t, err)
			require.Equal(t, tc.certified, certifiedBeforeOutput(err))
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			wantContextSize := tc.contextSize
			require.Equal(t, wantContextSize, providerErr.IsContextTooLarge())
		})
	}
}

func TestCodexTextResponseMetadataCertificationIsNarrow(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	for _, tc := range []struct {
		name      string
		text      string
		certified bool
	}{
		{name: "plain text config", text: `{"format":{"type":"text"},"verbosity":"medium"}`, certified: true},
		{name: "json schema config refused", text: `{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}`},
		{name: "unknown format refused", text: `{"format":{"type":"json_object"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"too large"},"text":` + tc.text + `}}`
			err := newStreamState().consume([]byte(event), func(fantasy.StreamPart) bool { return true })
			require.Error(t, err)
			require.Equal(t, tc.certified, certifiedBeforeOutput(err))
		})
	}
}

func TestCodexStreamErrorEventSSEEnvelopesCertifyExactContextRejection(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	cases := []struct {
		name          string
		before        string
		event         string
		wantCertified bool
	}{
		{
			name:          "response.failed nested error with lifecycle metadata",
			event:         `{"type":"response.failed","sequence_number":2,"response":{"id":"resp","object":"response","status":"failed","model":"gpt-5-codex","reasoning":{"effort":"high","summary":"auto","generate_summary":"concise"},"text":{"format":{"type":"text"},"verbosity":"medium"},"output":[],"usage":null,"error":{"code":"context_length_exceeded","message":"too large"}}}`,
			wantCertified: true,
		},
		{
			name:          "nested error type envelope",
			event:         `{"type":"error","sequence_number":1,"error":{"type":"context_length_exceeded","message":"too large"}}`,
			wantCertified: true,
		},
		{
			name:          "error flat Responses envelope",
			event:         `{"type":"error","code":"context_length_exceeded","message":"too large","param":null,"sequence_number":1}`,
			wantCertified: true,
		},
		{
			name:  "nested error output",
			event: `{"type":"error","error":{"type":"context_length_exceeded","message":"too large"},"output":[{"type":"message"}]}`,
		},
		{
			name:  "error flat usage",
			event: `{"type":"error","code":"context_length_exceeded","message":"too large","usage":{"input_tokens":0}}`,
		},
		{
			name:  "error flat unknown field",
			event: `{"type":"error","code":"context_length_exceeded","message":"too large","future_field":true}`,
		},
		{
			name:   "error flat after accepted output",
			before: `{"type":"response.output_text.delta","delta":"partial"}`,
			event:  `{"type":"error","code":"context_length_exceeded","message":"too large"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.before != "" {
					writeCodexEvent(w, tc.before)
				}
				writeCodexEvent(w, tc.event)
			}))
			t.Cleanup(server.Close)

			model := codexProviderForTest(t, server)
			_, err := model.Generate(t.Context(), fantasy.Call{})
			require.Error(t, err)
			require.Equal(t, tc.wantCertified, certifiedBeforeOutput(err))
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			require.True(t, providerErr.IsContextTooLarge())
			require.Zero(t, providerErr.StatusCode)
			require.Equal(t, tc.event, string(providerErr.ResponseBody))
			require.EqualValues(t, 1, requests.Load())
		})
	}
}

func TestCodexCancelledStreamDoesNotCertifyContextRejection(t *testing.T) {
	isolateContextRejectionTestPaths(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	model := codexProviderForTest(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := model.Generate(ctx, fantasy.Call{})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, certifiedBeforeOutput(err))
	require.Zero(t, requests.Load())
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

var (
	_ contextRejectedBeforeOutput = (*safeContextRejection)(nil)
	_ error                       = (*safeContextRejection)(nil)
)
