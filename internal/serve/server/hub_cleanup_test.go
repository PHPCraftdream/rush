package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestReadPumpCleanupReturnsAfterHubStopped(t *testing.T) {
	hub := newHub()
	runCtx, stopHub := context.WithCancel(context.Background())
	defer stopHub()
	go hub.Run(runCtx)
	s := &Server{hub: hub}

	stopHub()
	select {
	case <-hub.stopped:
	case <-time.After(time.Second):
		t.Fatal("hub did not stop")
	}

	for i := 0; i < cap(hub.unregister); i++ {
		select {
		case hub.unregister <- &Client{}:
		default:
			t.Fatal("could not fill hub unregister channel")
		}
	}

	connReady := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		select {
		case connReady <- conn:
		case <-time.After(time.Second):
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	clientConn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	defer clientConn.Close()
	_ = response.Body.Close()

	var serverConn *websocket.Conn
	select {
	case serverConn = <-connReady:
	case <-time.After(time.Second):
		t.Fatal("server connection was not ready")
	}
	client := newClient(hub, serverConn)
	done := make(chan struct{})
	go func() {
		s.readPump(context.Background(), client)
		close(done)
	}()

	require.NoError(t, clientConn.Close())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readPump cleanup blocked after hub stopped")
	}
	require.Len(t, hub.unregister, cap(hub.unregister))
}
