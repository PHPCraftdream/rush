package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestShutdownServerHandlerRepliesThenSignals verifies that the
// shutdown_server handler (1) sends a clean ack before (2) firing the
// shutdown signal channel. The test never calls Start, so nothing actually
// shuts down — we only verify the handler's effect on the channel.
func TestShutdownServerHandlerRepliesThenSignals(t *testing.T) {
	// Cannot use t.Parallel(): newAttachmentsTestApp calls t.Setenv.

	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	s := &Server{app: a, hub: newHub()}
	go s.hub.Run(t.Context())

	// IMPORTANT: grab the signal channel BEFORE dialing so it's already
	// created — shutdownSignal() lazily creates the channel, and we need
	// it ready before the select below can race a nil-channel read.
	sig := s.shutdownSignal()

	conn, _, readPumpDone := dialTestWS(t, s)

	// Start a background reader that watches for the expected ack frame.
	acked := make(chan struct{})
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m WSMessage
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			// We're looking for a response to "sd-1" with the expected
			// payload and no error field.
			if m.ID == "sd-1" && m.Type == EventResponse && m.Error == "" {
				var p map[string]string
				if json.Unmarshal(m.Payload, &p) == nil && p["status"] == "shutting_down" {
					close(acked)
				}
			}
		}
	}()

	// Send the shutdown_server command — no payload required.
	sendWS(t, conn, WSMessage{ID: "sd-1", Type: CmdShutdownServer})

	// Assert the ack is flushed to the socket within 5s.
	select {
	case <-acked:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for shutdown_server ack")
	}

	// Assert the shutdown signal fired within 5s.
	select {
	case <-sig:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for shutdown signal")
	}

	// Note: we intentionally never call Start() in this test, so nothing
	// actually shuts down. Closing the channel is the full extent of the
	// handler's effect — the Start() watcher would do the rest in production.
	_ = readPumpDone
}

// TestStartReturnsAfterWSShutdownSignal verifies that Start's shutdown
// watcher goroutine selects on s.shutdownSignal() (the WS-triggered path)
// and returns nil once s.requestShutdown() closes that channel. In
// production, this return is what lets runWebMode return and its
// deferred app.Shutdown run — the test deliberately stops short of exercising
// app.Shutdown itself (already covered by app package tests).
func TestStartReturnsAfterWSShutdownSignal(t *testing.T) {
	// Cannot use t.Parallel(): newAttachmentsTestApp calls t.Setenv.

	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	// Use New() instead of a bare literal so shutdownSig is eagerly
	// initialized, exactly as in production.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	s := New(a, "127.0.0.1:0", nil)

	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		err := s.Start(ctx, func(addr string) { ready <- addr })
		errCh <- err
	}()

	// Wait for the server to be ready.
	select {
	case addr := <-ready:
		t.Logf("Server started at %s", addr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server to be ready")
	}

	// Request shutdown via the WS-triggered path (same path the handler uses).
	s.requestShutdown()

	// Assert Start returns nil within 5s.
	select {
	case err := <-errCh:
		require.NoError(t, err, "Start should return nil after WS-triggered shutdown")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Start to return after shutdown signal")
	}
}
