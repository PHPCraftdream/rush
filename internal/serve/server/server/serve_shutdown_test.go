package server

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type closeNotifyingListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *closeNotifyingListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func TestServeListenerCancelsAndJoinsHandlersBeforeReturn(t *testing.T) {
	entered := make(chan struct{})
	listenerClosed := make(chan struct{})
	var orderMu sync.Mutex
	var order []string
	auth := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			select {
			case <-listenerClosed:
			default:
				t.Error("request context canceled before listener closed")
			}
			orderMu.Lock()
			order = append(order, "handler")
			orderMu.Unlock()
		})
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}, Auth: auth}
	server := NewWithPolicy(nil, policy, nil)
	listenCtx, listenCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer listenCancel()
	listener, err := (&net.ListenConfig{}).Listen(listenCtx, "tcp", policy.Address)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &closeNotifyingListener{Listener: listener, closed: listenerClosed}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(ctx, wrapped, nil) }()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		client := http.Client{Timeout: 5 * time.Second}
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+"/api/v1/wait", nil)
		if requestErr != nil {
			return
		}
		response, _ := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeListener() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ServeListener did not join handler")
	}
	select {
	case <-clientDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client request did not finish")
	}
	orderMu.Lock()
	order = append(order, "App shutdown")
	if len(order) != 2 || order[0] != "handler" || order[1] != "App shutdown" {
		t.Fatalf("shutdown order = %v, want [handler App shutdown]", order)
	}
	orderMu.Unlock()
}

func TestServeListenerClosesHijackedConnectionOnShutdown(t *testing.T) {
	upgraded := make(chan struct{})
	var connection net.Conn
	var connMu sync.Mutex
	auth := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			connMu.Lock()
			connection = conn
			connMu.Unlock()
			close(upgraded)
		})
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}, Auth: auth}
	server := NewWithPolicy(nil, policy, nil)
	listenCtx, listenCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer listenCancel()
	listener, err := (&net.ListenConfig{}).Listen(listenCtx, "tcp", policy.Address)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(ctx, listener, nil) }()
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	client, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, _ = client.Write([]byte("GET /api/v1/hijack HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	select {
	case <-upgraded:
	case <-time.After(3 * time.Second):
		t.Fatal("connection was not hijacked")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeListener() error = %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("ServeListener did not close hijacked connection")
	}
	connMu.Lock()
	defer connMu.Unlock()
	if _, err := connection.Write([]byte("x")); err == nil {
		t.Fatal("hijacked connection remained writable")
	}
}
