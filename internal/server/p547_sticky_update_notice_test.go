package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// drainClient collects every event type a client received, up to a short
// quiet period.
func drainClient(t *testing.T, c *Client) []string {
	t.Helper()
	var types []string
	for {
		select {
		case raw, ok := <-c.send:
			if !ok {
				return types
			}
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			types = append(types, env.Type)
		case <-time.After(200 * time.Millisecond):
			return types
		}
	}
}

// TestHub_StickyEventSurvivesReplayBufferEviction covers task #547.
//
// The update-available notice is sent once, at server start, and then the
// replay ring evicts it: it is a bounded 2000-event buffer and a single
// streaming turn pushes thousands of deltas. An operator who starts the
// server, lets an agent run, and only then opens the browser would never see
// the badge -- which is the ordinary way people use a UI.
//
// Revert-check: switch broadcastUpdateNotice back to h.Broadcast (or drop
// the sticky replay from Run's register case) and this fails: the late
// client receives only the flood, never the notice.
func TestHub_StickyEventSurvivesReplayBufferEviction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHub()
	go h.Run(ctx)

	// Sent at "server start", before any client connects.
	h.BroadcastSticky(EventUpdateAvailable, UpdateAvailableWire{Current: "1.0.0", Latest: "1.1.0"})

	// Now overrun the replay ring the way one streaming turn does. The
	// notice cannot possibly still be in the buffer afterwards.
	for i := 0; i < maxBufferSize+50; i++ {
		h.Broadcast("noise", map[string]int{"i": i})
	}

	// Let the hub drain its broadcast channel before anyone registers.
	require.Eventually(t, func() bool { return len(h.broadcast) == 0 }, 5*time.Second, 10*time.Millisecond)

	late := &Client{send: make(chan []byte, maxBufferSize+200)}
	h.register <- late

	types := drainClient(t, late)
	require.Contains(t, types, EventUpdateAvailable,
		"a client connecting after one turn's worth of traffic never received the update notice -- the replay ring had already evicted it")
}

// TestHub_StickyEventKeepsOnlyTheLatestPerType pins the "one entry per event
// type" rule. Without it a sticky event sent repeatedly would accumulate and
// every new client would be handed a pile of stale copies.
func TestHub_StickyEventKeepsOnlyTheLatestPerType(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHub()
	go h.Run(ctx)

	h.BroadcastSticky(EventUpdateAvailable, UpdateAvailableWire{Current: "1.0.0", Latest: "1.1.0"})
	h.BroadcastSticky(EventUpdateAvailable, UpdateAvailableWire{Current: "1.0.0", Latest: "1.2.0"})
	require.Eventually(t, func() bool { return len(h.broadcast) == 0 }, 5*time.Second, 10*time.Millisecond)

	envs := h.stickyEvents()
	require.Len(t, envs, 1, "one entry per event type, not one per send")

	var env WSMessage
	require.NoError(t, json.Unmarshal(envs[0], &env))
	var wire UpdateAvailableWire
	require.NoError(t, json.Unmarshal(env.Payload, &wire))
	require.Equal(t, "1.2.0", wire.Latest, "the retained entry must be the latest send, not the first")
}

// TestHub_NonStickyEventIsNotReplayedToLateClients is the control: ordinary
// broadcasts must NOT become sticky, or every new client would be handed
// arbitrary stale per-turn traffic.
func TestHub_NonStickyEventIsNotReplayedToLateClients(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHub()
	go h.Run(ctx)

	h.Broadcast("ordinary", map[string]string{"k": "v"})
	for i := 0; i < maxBufferSize+50; i++ {
		h.Broadcast("noise", map[string]int{"i": i})
	}
	require.Eventually(t, func() bool { return len(h.broadcast) == 0 }, 5*time.Second, 10*time.Millisecond)

	require.Empty(t, h.stickyEvents(), "a plain Broadcast must never be retained as sticky")

	late := &Client{send: make(chan []byte, maxBufferSize+200)}
	h.register <- late
	types := drainClient(t, late)
	require.NotContains(t, types, "ordinary",
		"an evicted ordinary event must stay evicted -- stickiness is opt-in")
}
