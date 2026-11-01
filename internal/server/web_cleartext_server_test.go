package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type cleartextTestListener struct {
	closed     chan struct{}
	accepting  chan struct{}
	acceptOnce sync.Once
	once       sync.Once
	addr       net.Addr
}

func (l *cleartextTestListener) Accept() (net.Conn, error) {
	l.acceptOnce.Do(func() { close(l.accepting) })
	<-l.closed
	return nil, net.ErrClosed
}

func (l *cleartextTestListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *cleartextTestListener) Addr() net.Addr { return l.addr }

func TestServeListenerRejectsUnexpectedNonLoopbackBind(t *testing.T) {
	ln := &cleartextTestListener{closed: make(chan struct{}), accepting: make(chan struct{}), addr: &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 1234}}
	s := New(nil, "localhost:0", nil)
	errCh := make(chan error, 1)
	go func() { errCh <- s.ServeListener(context.Background(), ln, nil) }()
	select {
	case <-ln.accepting:
		t.Fatal("Accept ran despite the post-bind refusal")
	case err := <-errCh:
		if !errors.Is(err, ErrUnexpectedNonLoopbackBind) {
			t.Fatalf("ServeListener() error = %v; want ErrUnexpectedNonLoopbackBind", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeListener neither refused nor entered Accept")
	}
	select {
	case <-ln.closed:
	default:
		t.Fatal("listener was not closed after refusal")
	}
}

func TestServeListenerCleartextOptInServesUntilShutdown(t *testing.T) {
	ln := &cleartextTestListener{closed: make(chan struct{}), accepting: make(chan struct{}), addr: &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 1234}}
	s := NewWithCleartext(nil, "localhost:0", nil, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan string, 1)
	go func() { done <- s.ServeListener(ctx, ln, func(addr string) { ready <- addr }) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("ServeListener did not start")
	}
	select {
	case <-ln.accepting:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Accept was not reached")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeListener() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeListener did not stop")
	}
}

func TestNewServerLoopbackDefaultStillStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	s := New(nil, "127.0.0.1:0", nil)
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx, func(string) { close(ready) }) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}
