package mcpdial

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func signalStarted(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func waitForRequest(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP initialization did not reach the blocked request")
	}
}

func TestHeaderRoundTripperKeepsOwnerCancellationUntilBodyClose(t *testing.T) {
	serverCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(serverCanceled)
	}))
	defer server.Close()

	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	transport, err := cloneHTTPTransport()
	require.NoError(t, err)
	rt := &headerRoundTripper{ctx: ownerCtx, transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Body)

	ownerCancel()
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not reach the open response body")
	}
	require.NoError(t, resp.Body.Close())
}

type closeAfterContextBody struct {
	ctx     context.Context
	started chan struct{}
}

func (*closeAfterContextBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (b *closeAfterContextBody) Close() error {
	close(b.started)
	<-b.ctx.Done()
	return nil
}

func TestOwnerResponseBodyCloseCancelsBeforeUnderlyingClose(t *testing.T) {
	requestCtx, requestCancel := context.WithCancel(context.Background())
	defer requestCancel()
	underlying := &closeAfterContextBody{ctx: requestCtx, started: make(chan struct{})}
	stopCalls := 0
	body := &ownerResponseBody{
		ReadCloser: underlying,
		stop: func() bool {
			stopCalls++
			return true
		},
		cancel: requestCancel,
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- body.Close() }()
	<-underlying.started
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		requestCancel()
		<-closeDone
		t.Fatal("response body Close did not cancel the request before closing the underlying body")
	}
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	require.Equal(t, 1, stopCalls)
}

func TestHeaderRoundTripperCancelsBlockedRequestBeforeResponse(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalStarted(started)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		signalStarted(canceled)
	}))
	defer server.Close()

	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	transport, err := cloneHTTPTransport()
	require.NoError(t, err)
	rt := &headerRoundTripper{ctx: ownerCtx, transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, bytes.NewReader([]byte(`{"jsonrpc":"2.0"}`)))
	require.NoError(t, err)
	requestDone := make(chan error, 1)
	go func() {
		// The point of this test is that ownerCancel unblocks RoundTrip, so
		// the response is normally nil here. Close it on the path where the
		// race resolves the other way, rather than leaking a connection.
		resp, err := rt.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()
	waitForRequest(t, started)

	ownerCancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("transport cancellation did not reach the blocked HTTP request")
	}
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("transport RoundTrip did not return after cancellation")
	}
}
