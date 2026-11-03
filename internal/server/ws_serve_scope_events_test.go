package server

import (
	"context"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// sentinelType is a non-sensitive event broadcast AFTER the event under test;
// hub delivery is FIFO, so every frame the hub will deliver for the earlier
// broadcast has been seen once the sentinel arrives.
const sentinelType = EventSessionCreated

func TestServeWSMCPStateLiveBroadcastIsAdminOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allowAdmin bool
		token      string
		want       bool
	}{
		{"read token", true, "read", false},
		{"run token", true, "run", false},
		{"admin token on a listener without allow_admin", false, "admin", false},
		{"admin token on an allow_admin listener", true, "admin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServeWSScopeFixture(t, tc.allowAdmin, map[string][]string{"read": {"read"}, "run": {"run"}, "admin": {"admin"}})
			conn := f.dial(t, tc.token)
			reply, _ := exchange(t, conn, WSMessage{ID: "sync", Type: CmdListSessions})
			require.Equal(t, EventSessionsList, reply.Type)

			f.server.hub.Broadcast(EventMCPState, map[string]string{"env": "SECRET_VALUE"})
			f.server.hub.Broadcast(sentinelType, map[string]string{"id": "sentinel"})
			seen := awaitType(t, conn, sentinelType)
			require.Equal(t, tc.want, containsType(seen, EventMCPState), "frames before the sentinel: %v", seen)
		})
	}
}

func TestServeWSMCPStateReplayIsFilteredOnRegistration(t *testing.T) {
	for _, mode := range []string{"sticky", "buffered"} {
		t.Run(mode, func(t *testing.T) {
			f := newServeWSScopeFixture(t, true, map[string][]string{"read": {"read"}, "admin": {"admin"}})
			if mode == "sticky" {
				f.server.hub.BroadcastSticky(EventMCPState, map[string]string{"env": "SECRET_VALUE"})
			} else {
				f.server.hub.Broadcast(EventMCPState, map[string]string{"env": "SECRET_VALUE"})
				select {
				case <-f.server.hub.WaitDelivery():
				case <-time.After(5 * time.Second):
					t.Fatal("hub did not deliver the broadcast")
				}
			}
			for name, want := range map[string]bool{"read": false, "admin": true} {
				conn := f.dial(t, name)
				reply, skipped := exchange(t, conn, WSMessage{ID: "sync-" + name, Type: CmdListSessions})
				require.Equal(t, EventSessionsList, reply.Type)
				f.server.hub.Broadcast(sentinelType, map[string]string{"id": name})
				before := append(skipped, awaitType(t, conn, sentinelType)...)
				require.Equal(t, want, containsType(before, EventMCPState), "%s replay for %s: %v", mode, name, before)
			}
		})
	}
}

// TestServeWSNonSensitiveEventsReachEveryClient guards against an over-broad
// filter: only mcp_state is withheld.
func TestServeWSNonSensitiveEventsReachEveryClient(t *testing.T) {
	f := newServeWSScopeFixture(t, true, map[string][]string{"read": {"read"}, "admin": {"admin"}})
	for _, name := range []string{"read", "admin"} {
		conn := f.dial(t, name)
		reply, _ := exchange(t, conn, WSMessage{ID: "sync", Type: CmdListSessions})
		require.Equal(t, EventSessionsList, reply.Type)
		f.server.hub.Broadcast(sentinelType, map[string]string{"id": name})
		awaitType(t, conn, sentinelType)
	}
}

// TestHubMCPStateStillReachesUnauthenticatedClients runs the real hub loop
// with a legacy client (no serve identity), as the loopback UI server uses
// it: live delivery, sticky replay and buffered replay all keep mcp_state.
func TestHubMCPStateStillReachesUnauthenticatedClients(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hub := newHub()
	go hub.Run(ctx)

	register := func() *Client {
		c := newClient(hub, nil)
		c.send = make(chan []byte, 64)
		require.True(t, hub.tryRegister(ctx, c))
		return c
	}
	drain := func(c *Client) []string {
		select {
		case <-hub.WaitDelivery():
		case <-time.After(5 * time.Second):
			t.Fatal("hub did not drain")
		}
		var types []string
		for len(c.send) > 0 {
			types = append(types, envelopeType(<-c.send))
		}
		return types
	}

	hub.BroadcastSticky(EventMCPState, map[string]string{"v": "sticky"})
	hub.Broadcast(EventMCPState, map[string]string{"v": "live"})
	select {
	case <-hub.WaitDelivery():
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not deliver")
	}
	late := register() // replay path
	types := drain(late)
	require.Contains(t, types, EventMCPState, "replay for a legacy client")

	hub.Broadcast(EventMCPState, map[string]string{"v": "live2"})
	require.Contains(t, drain(late), EventMCPState, "live delivery to a legacy client")
}

// TestServeWSMCPStateStickyBroadcastToConnectedClients covers the live
// fan-out of a sticky event (a separate hub path from ordinary broadcasts).
func TestServeWSMCPStateStickyBroadcastToConnectedClients(t *testing.T) {
	f := newServeWSScopeFixture(t, true, map[string][]string{"read": {"read"}, "admin": {"admin"}})
	conns := map[string]*websocket.Conn{}
	for _, name := range []string{"read", "admin"} {
		conns[name] = f.dial(t, name)
		reply, _ := exchange(t, conns[name], WSMessage{ID: "sync-" + name, Type: CmdListSessions})
		require.Equal(t, EventSessionsList, reply.Type)
	}
	f.server.hub.BroadcastSticky(EventMCPState, map[string]string{"env": "SECRET_VALUE"})
	select {
	case <-f.server.hub.WaitDelivery():
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not deliver the sticky broadcast")
	}
	f.server.hub.Broadcast(sentinelType, map[string]string{"id": "sentinel"})
	awaitType(t, conns["admin"], EventMCPState) // fails on timeout if the admin never gets it
	seen := awaitType(t, conns["read"], sentinelType)
	require.False(t, containsType(seen, EventMCPState), "a read token received mcp_state: %v", seen)
}
