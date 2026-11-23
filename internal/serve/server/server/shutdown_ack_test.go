package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestShutdownListenerAlreadyClosedIsSuccessful(t *testing.T) {
	workingDir, dataDir := t.TempDir(), t.TempDir()
	setIsolatedGlobalPaths(t)
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	for range 50 {
		s := New(a, "127.0.0.1:0", nil)
		listenCtx, listenCancel := context.WithTimeout(t.Context(), 10*time.Second)
		ln, err := (&net.ListenConfig{}).Listen(listenCtx, "tcp", "127.0.0.1:0")
		listenCancel()
		require.NoError(t, err)
		wrapped := &closeTwiceListener{
			Listener:      ln,
			acceptEntered: make(chan struct{}),
			release:       make(chan struct{}),
			secondClose:   make(chan struct{}),
		}
		releaseOnce := sync.Once{}
		releaseAccept := func() { releaseOnce.Do(func() { close(wrapped.release) }) }
		t.Cleanup(releaseAccept)
		ctx, cancel := context.WithCancel(context.Background())
		ready := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() { done <- s.ServeListener(ctx, wrapped, func(string) { ready <- struct{}{} }) }()
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			t.Fatal("ServeListener did not become ready")
		}
		select {
		case <-wrapped.acceptEntered:
		case <-time.After(10 * time.Second):
			t.Fatal("HTTP server did not start accepting")
		}
		cancel()
		select {
		case <-wrapped.secondClose:
			require.GreaterOrEqual(t, wrapped.closeCalls.Load(), int32(2))
		case <-time.After(10 * time.Second):
			releaseAccept()
			t.Fatal("shutdown did not close the registered listener twice")
		}
		releaseAccept()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("ServeListener did not stop")
		}
		cancel()
	}
}

type closeTwiceListener struct {
	net.Listener
	closeCalls    atomic.Int32
	acceptEntered chan struct{}
	release       chan struct{}
	secondClose   chan struct{}
}

func (l *closeTwiceListener) Accept() (net.Conn, error) {
	select {
	case <-l.acceptEntered:
	default:
		close(l.acceptEntered)
	}
	<-l.release
	return nil, net.ErrClosed
}

func (l *closeTwiceListener) Close() error {
	if l.closeCalls.Add(1) > 1 {
		select {
		case <-l.secondClose:
		default:
			close(l.secondClose)
		}
		return net.ErrClosed
	}
	return l.Listener.Close()
}

func TestShutdownWebSocketPeerGoneDoesNotBlockShutdown(t *testing.T) {
	workingDir, dataDir := t.TempDir(), t.TempDir()
	setIsolatedGlobalPaths(t)
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(a, "127.0.0.1:0", nil)
	ready, started := make(chan string, 1), make(chan error, 1)
	go func() { started <- s.Start(ctx, func(addr string) { ready <- addr }) }()
	var addr string
	select {
	case addr = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not become ready")
	}
	conn, response, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws?token="+s.Token(), nil)
	require.NoError(t, err)
	_ = response.Body.Close()
	oldLimit := shutdownAckWriteLimit
	shutdownAckWriteLimit = 100 * time.Millisecond
	t.Cleanup(func() { shutdownAckWriteLimit = oldLimit })
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	testHookMu.Lock()
	oldHook := testBeforeWrite
	testBeforeWrite = func(msg []byte) {
		var envelope WSMessage
		if json.Unmarshal(msg, &envelope) != nil || envelope.ID != "shutdown-gone" {
			return
		}
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	testHookMu.Unlock()
	t.Cleanup(func() {
		testHookMu.Lock()
		testBeforeWrite = oldHook
		testHookMu.Unlock()
	})
	require.NoError(t, conn.WriteJSON(WSMessage{ID: "shutdown-gone", Type: CmdShutdownServer}))
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("write pump did not reach acknowledgement")
	}
	require.NoError(t, conn.Close())
	startedAt := time.Now()
	select {
	case err := <-started:
		require.NoError(t, err)
		require.GreaterOrEqual(t, time.Since(startedAt), 50*time.Millisecond)
		require.Less(t, time.Since(startedAt), time.Second)
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete after peer-gone acknowledgement timeout")
	}
	releaseOnce.Do(func() { close(release) })
}

func TestShutdownWebSocketAckWrittenBeforeShutdown(t *testing.T) {
	workingDir, dataDir := t.TempDir(), t.TempDir()
	setIsolatedGlobalPaths(t)
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	var logs syncBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(a, "127.0.0.1:0", nil)
	ready, started := make(chan string, 1), make(chan error, 1)
	go func() { started <- s.Start(ctx, func(addr string) { ready <- addr }) }()
	var addr string
	select {
	case addr = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not become ready")
	}
	conn, response, err := websocket.DefaultDialer.Dial("ws://"+addr+"/ws?token="+s.Token(), nil)
	require.NoError(t, err)
	_ = response.Body.Close()
	defer conn.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	oldHook := func(msg []byte) {
		var envelope WSMessage
		if json.Unmarshal(msg, &envelope) != nil || envelope.ID != "shutdown" {
			return
		}
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	var previousHook func([]byte)
	testHookMu.Lock()
	previousHook = testBeforeWrite
	testBeforeWrite = oldHook
	testHookMu.Unlock()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		testHookMu.Lock()
		testBeforeWrite = previousHook
		testHookMu.Unlock()
	})
	require.NoError(t, conn.WriteJSON(WSMessage{ID: "shutdown", Type: CmdShutdownServer}))
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("write pump did not reach gated acknowledgement")
	}
	select {
	case err := <-started:
		t.Fatalf("server stopped before acknowledgement write: %v", err)
	case <-s.shutdownSignal():
		t.Fatal("shutdown signal raised before acknowledgement write")
	case <-time.After(300 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, raw, err := conn.ReadMessage()
		require.NoError(t, err)
		var msg WSMessage
		require.NoError(t, json.Unmarshal(raw, &msg))
		if msg.ID == "shutdown" && msg.Type == EventResponse {
			break
		}
	}
	select {
	case err := <-started:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after acknowledgement")
	}
	require.NotContains(t, logs.String(), s.Token())
}
