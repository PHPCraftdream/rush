package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
)

type manualTicker struct {
	ch   chan time.Time
	once sync.Once
}

func (t *manualTicker) C() <-chan time.Time { return t.ch }
func (t *manualTicker) Stop()               { t.once.Do(func() {}) }

func TestServeRevocationClosesWithPolicyViolation(t *testing.T) {
	isolateAPIGlobals(t)
	verifier := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	ticker := &manualTicker{ch: make(chan time.Time, 1)}
	tickerCreated := make(chan struct{})
	principal := Principal{TokenID: "12345678", verifier: verifier, tickerFactory: func(time.Duration) Ticker {
		close(tickerCreated)
		return ticker
	}}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		startServeRevocationWatch(r.Context(), conn, principal)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _, _ = conn.ReadMessage()
		_ = conn.Close()
	}))
	defer s.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+s.URL[len("http"):], nil) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	select {
	case <-tickerCreated:
	case <-time.After(time.Second):
		t.Fatal("revocation ticker was not created")
	}
	verifier.token.RevokedAt = ptrTime(time.Now())
	ticker.ch <- time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.ClosePolicyViolation || closeErr.Text != "token revoked or expired" {
		t.Fatalf("close error = %v", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestServeAPIRevocationClosesConnectionAndReleasesSlot(t *testing.T) {
	isolateAPIGlobals(t)
	previousTicker, previousReleased := serveAuthTickerFactory, serveAuthSlotReleased
	defer func() { serveAuthTickerFactory, serveAuthSlotReleased = previousTicker, previousReleased }()
	ticker := &manualTicker{ch: make(chan time.Time, 1)}
	tickerCreated := make(chan struct{}, 1)
	serveAuthTickerFactory = func(time.Duration) Ticker {
		tickerCreated <- struct{}{}
		return ticker
	}
	released := make(chan struct{}, 1)
	serveAuthSlotReleased = released
	store := config.GlobalAPITokenStore()
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS}, Limits: ListenerLimits{MaxConnsPerToken: 1, RPS: 1000, Burst: 1000}}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	endpoint := startPolicyServer(t, NewWithPolicy(nil, policy, nil))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop serve API: %v", err)
		}
	})
	token, record, err := store.CreateAPIToken("revoke integration", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, response, err := dialServeWS(t, "ws"+strings.TrimPrefix(endpoint, "http"), token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, response)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	select {
	case <-tickerCreated:
	case <-time.After(3 * time.Second):
		t.Fatal("production auth did not create revocation ticker")
	}
	if err := store.RevokeAPIToken(record.ID); err != nil {
		t.Fatal(err)
	}
	ticker.ch <- time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.ClosePolicyViolation || strings.Contains(closeErr.Text, token) {
		t.Fatalf("close error = %v; want token-free policy violation", err)
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("slot release signal did not follow Chain release")
	}
	freed, response, err := dialServeWS(t, "ws"+strings.TrimPrefix(endpoint, "http"), token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err == nil {
		freed.Close()
		t.Fatal("revoked token unexpectedly upgraded")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked reconnect response = %v", response)
	}
}

func TestServeAPIExpiredTokenClosesConnectionAndReleasesSlot(t *testing.T) {
	isolateAPIGlobals(t)
	previousTicker, previousReleased := serveAuthTickerFactory, serveAuthSlotReleased
	defer func() { serveAuthTickerFactory, serveAuthSlotReleased = previousTicker, previousReleased }()
	ticker := &manualTicker{ch: make(chan time.Time, 1)}
	serveAuthTickerFactory = func(time.Duration) Ticker { return ticker }
	released := make(chan struct{}, 1)
	serveAuthSlotReleased = released
	store := config.GlobalAPITokenStore()
	future := time.Now().Add(time.Minute)
	token, _, err := store.CreateAPIToken("expiring integration", []string{"read"}, "ask", &future)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS}, Limits: ListenerLimits{MaxConnsPerToken: 1, RPS: 1000, Burst: 1000}}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	endpoint := startPolicyServer(t, NewWithPolicy(nil, policy, nil))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop serve API: %v", err)
		}
	})
	conn, response, err := dialServeWS(t, "ws"+strings.TrimPrefix(endpoint, "http"), token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, response)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	past := future.Add(time.Second)
	ticker.ch <- past
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.ClosePolicyViolation || strings.Contains(closeErr.Text, token) {
		t.Fatalf("close error = %v; want token-free policy violation", err)
	}
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("expired token slot was not released")
	}
}

func TestServeAPIShutdownReleasesSlot(t *testing.T) {
	released := make(chan struct{}, 1)
	endpoint, token, _, server := newServeWSFixture(t, 1, func() { serveAuthSlotReleased = released })
	conn, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, response)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	server.requestShutdown()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not release connection slot")
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("connection remained open after shutdown")
	}
}
