package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
)

// scopeFixture is the production serve WebSocket path (NewServeAPI + Configure
// + NewWithPolicy + a real token store) over a real App, with one token per
// requested scope set.
type scopeFixture struct {
	endpoint string
	app      *appPkg.App
	server   *Server
	tokens   map[string]string
}

func newServeWSScopeFixture(t *testing.T, allowAdmin bool, scopes map[string][]string) *scopeFixture {
	t.Helper()
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := config.GlobalAPITokenStore()
	tokens := make(map[string]string, len(scopes))
	for name, granted := range scopes {
		token, _, err := store.CreateAPIToken("ws scope "+name, granted, "ask", nil)
		if err != nil {
			t.Fatal(err)
		}
		tokens[name] = token
	}
	policy := ListenerPolicy{
		Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS},
		AllowAdmin: allowAdmin,
		Limits:     ListenerLimits{MaxConnsPerToken: 50, RPS: 100000, Burst: 100000},
	}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	server := NewWithPolicy(app, policy, nil)
	base := startPolicyServer(t, server)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop serve API: %v", err)
		}
	})
	return &scopeFixture{endpoint: "ws" + strings.TrimPrefix(base, "http"), app: app, server: server, tokens: tokens}
}

// dial connects with the named token and consumes the hello frame.
func (f *scopeFixture) dial(t *testing.T, name string) *websocket.Conn {
	t.Helper()
	token, ok := f.tokens[name]
	if !ok {
		t.Fatalf("fixture has no token named %q", name)
	}
	conn, response, err := dialServeWS(t, f.endpoint, token) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("dial as %s: %v (%v)", name, err, response)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("hello for %s: %v", name, err)
	}
	var hello WSMessage
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Type != EventHello {
		t.Fatalf("first frame for %s = %q (%v), want hello", name, raw, err)
	}
	return conn
}

// exchange sends one command and returns its correlated reply together with
// the types of every uncorrelated frame that arrived before it.
func exchange(t *testing.T, conn *websocket.Conn, message WSMessage) (WSMessage, []string) {
	t.Helper()
	sendCommand(t, conn, message)
	return awaitReply(t, conn, message.ID)
}

func sendCommand(t *testing.T, conn *websocket.Conn, message WSMessage) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(message); err != nil {
		t.Fatalf("write %s: %v", message.Type, err)
	}
}

func awaitReply(t *testing.T, conn *websocket.Conn, id string) (WSMessage, []string) {
	t.Helper()
	var skipped []string
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for reply %q: %v (skipped %v)", id, err, skipped)
		}
		var reply WSMessage
		if err := json.Unmarshal(frame, &reply); err != nil {
			t.Fatalf("decode frame %q: %v", frame, err)
		}
		if reply.ID == id && id != "" {
			return reply, skipped
		}
		skipped = append(skipped, reply.Type)
	}
}

// awaitType reads frames until one of the given type arrives and returns the
// types seen before it.
func awaitType(t *testing.T, conn *websocket.Conn, want string) []string {
	t.Helper()
	var before []string
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for %s frame: %v (saw %v)", want, err, before)
		}
		var message WSMessage
		if err := json.Unmarshal(frame, &message); err != nil {
			t.Fatalf("decode frame %q: %v", frame, err)
		}
		if message.Type == want {
			return before
		}
		before = append(before, message.Type)
	}
}

func containsType(types []string, want string) bool {
	for _, got := range types {
		if got == want {
			return true
		}
	}
	return false
}
