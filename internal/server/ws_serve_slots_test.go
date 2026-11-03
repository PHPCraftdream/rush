package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
)

func newServeWSFixture(t *testing.T, max int, beforeBuild ...func()) (string, string, *ServeAPI, *Server) {
	return newServeWSFixtureWithOrigins(t, max, nil, beforeBuild...)
}

func newServeWSFixtureWithOrigins(t *testing.T, max int, origins []string, beforeBuild ...func()) (string, string, *ServeAPI, *Server) {
	t.Helper()
	isolateAPIGlobals(t)
	previousReleased := serveAuthSlotReleased
	serveAuthSlotReleased = nil
	t.Cleanup(func() { serveAuthSlotReleased = previousReleased })
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("ws integration", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS}, AllowedOrigins: origins, Limits: ListenerLimits{MaxConnsPerToken: max, RPS: 1000, Burst: 1000}}
	for _, before := range beforeBuild {
		before()
	}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	server := NewWithPolicy(nil, policy, nil)
	base := startPolicyServer(t, server)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop serve API: %v", err)
		}
	})
	return "ws" + strings.TrimPrefix(base, "http"), token, api, server
}

func dialServeWS(t *testing.T, endpoint, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	return dialer.Dial(endpoint+"/ws", http.Header{"Authorization": []string{"Bearer " + token}})
}

func TestServeWSHelloFrame(t *testing.T) {
	endpoint, token, _, _ := newServeWSFixture(t, 2)
	conn, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("dial failed: %v, response=%v", err, response)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var message WSMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	tokenRecord, ok, err := config.GlobalAPITokenStore().VerifyAPIToken(token, time.Now())
	if err != nil || !ok {
		t.Fatalf("verify fixture token: ok=%v err=%v", ok, err)
	}
	var payload helloPayload
	if err := json.Unmarshal(message.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(message.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["protocol"] == nil || fields["token_id"] == nil || fields["scopes"] == nil || payload.TokenID != tokenRecord.ID {
		t.Fatalf("hello payload fields/id = %v/%q, want protocol, token_id, scopes and id %q", fields, payload.TokenID, tokenRecord.ID)
	}
	if message.Type != EventHello || payload.Protocol != "rush.v1" || payload.TokenID == "" || len(payload.Scopes) != 1 || payload.Scopes[0] != "read" || strings.Contains(string(raw), token) {
		t.Fatalf("unexpected hello frame: %s", raw)
	}
}

func TestServeWSConnectionSlotsLifetime(t *testing.T) {
	endpoint, token, _, _ := newServeWSFixture(t, 2)
	first, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("first dial: %v (%v)", err, response)
	}
	defer first.Close()
	second, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("second dial: %v (%v)", err, response)
	}
	defer second.Close()
	third, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err == nil {
		third.Close()
		t.Fatal("third connection unexpectedly upgraded")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") == "" {
		t.Fatalf("third response = %v, want 429 with Retry-After", response)
	}
}

func TestServeWSAbruptDisconnectReleasesSlot(t *testing.T) {
	released := make(chan struct{}, 1)
	endpoint, token, _, _ := newServeWSFixture(t, 1, func() { serveAuthSlotReleased = released })
	conn, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, response)
	}
	if tcp, ok := conn.UnderlyingConn().(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for abrupt disconnect to release slot")
	}
	freed, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("abrupt disconnect did not free slot: %v (%v)", err, response)
	}
	freed.Close()
}

func TestServeWSConnectionSlotsSharedWithREST(t *testing.T) {
	endpoint, token, _, _ := newServeWSFixture(t, 2)
	first, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("first dial: %v (%v)", err, response)
	}
	defer first.Close()
	second, response, err := dialServeWS(t, endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("second dial: %v (%v)", err, response)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(endpoint, "ws://", "http://", 1)+"/api/v1/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("REST response while WS slots full = %d headers=%v, want 429 with Retry-After", resp.StatusCode, resp.Header)
	}
}
