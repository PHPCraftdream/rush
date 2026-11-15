package nettransport

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type responsesWSDeadlineWire struct {
	responsesWSTestWire
	readDeadline  time.Time
	writeDeadline time.Time
}

func (w *responsesWSDeadlineWire) SetReadDeadline(value time.Time) error {
	w.readDeadline = value
	return nil
}

func (w *responsesWSDeadlineWire) SetWriteDeadline(value time.Time) error {
	w.writeDeadline = value
	return nil
}

func TestResponsesWSRequestContextReadDoesNotInheritWriteBudget(t *testing.T) {
	for _, withDeadline := range []bool{false, true} {
		wire := &responsesWSDeadlineWire{responsesWSTestWire: responsesWSTestWire{read: [][]byte{[]byte(`{}`)}}}
		conn := newResponsesWSTestConn(wire, 64, 128)
		conn.readPolicy = ResponsesWSReadRequestContext
		conn.ioTimeout = 30 * time.Second
		ctx := context.Background()
		if withDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
		}
		require.NoError(t, conn.BeginExchange())
		before := time.Now()
		require.NoError(t, conn.SendRaw(ctx, []byte(`{}`)))
		require.WithinDuration(t, before.Add(30*time.Second), wire.writeDeadline, time.Second)
		_, err := conn.ReceiveRaw(ctx)
		require.NoError(t, err)
		deadline, _ := ctx.Deadline()
		require.Equal(t, deadline, wire.readDeadline)
		require.NoError(t, conn.Close())
	}
	options := responsesWSTestOptions("http://127.0.0.1/")
	options.ReadPolicy = ResponsesWSReadPolicy(255)
	require.Error(t, validateResponsesWSOptions(&options))
}

func TestResponsesWSQuietReadFollowsRequestWatchdogContext(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn := responsesWSUpgrade(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		close(entered)
		<-release
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	options := responsesWSTestOptions(server.URL)
	options.IOTimeout = 200 * time.Millisecond
	options.ReadPolicy = ResponsesWSReadRequestContext
	conn, err := DialResponsesWebSocket(ctx, options)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.BeginExchange())
	require.NoError(t, conn.SendRaw(ctx, []byte(`{"type":"response.create"}`)))
	done := make(chan error, 1)
	go func() { _, err := conn.ReceiveRaw(ctx); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("request did not reach peer")
	}
	// Scale the old 30-second read/write coupling down: quiet inference must
	// survive two write budgets, while the request watchdog remains live.
	select {
	case err := <-done:
		t.Fatalf("read ended before request watchdog: %v", err)
	case <-time.After(2 * options.IOTimeout):
	}
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("released inference did not return")
	}
	require.NoError(t, conn.MarkCompleted())
}

func TestResponsesWSRequestContextCancellationStillRetiresPossiblySentRead(t *testing.T) {
	entered := make(chan struct{})
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn := responsesWSUpgrade(t, w, r)
		if conn == nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		close(entered)
		_, _, _ = conn.ReadMessage()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	options := responsesWSTestOptions(server.URL)
	options.ReadPolicy = ResponsesWSReadRequestContext
	conn, err := DialResponsesWebSocket(ctx, options)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.BeginExchange())
	require.NoError(t, conn.SendRaw(ctx, []byte(`{}`)))
	done := make(chan error, 1)
	go func() { _, err := conn.ReceiveRaw(ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach peer")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		var terminal *ResponsesWSError
		require.ErrorAs(t, err, &terminal)
		require.True(t, terminal.NoReplay())
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog cancellation did not retire read")
	}
}
