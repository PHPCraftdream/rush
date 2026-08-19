package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/update"
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

// drainClientPayloads is drainClient but also captures the raw payload for
// each occurrence of wantType, in receive order. Used where a test needs to
// tell WHICH generation of a repeated event type a client ended up with, not
// just whether/how many times the type appeared.
func drainClientPayloads(t *testing.T, c *Client, wantType string) (types []string, payloads []json.RawMessage) {
	t.Helper()
	for {
		select {
		case raw, ok := <-c.send:
			if !ok {
				return types, payloads
			}
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			types = append(types, env.Type)
			if env.Type == wantType {
				payloads = append(payloads, env.Payload)
			}
		case <-time.After(200 * time.Millisecond):
			return types, payloads
		}
	}
}

// waitHubDrained blocks until the hub has finished processing everything
// producers have queued so far, on both the ordinary broadcast channel and
// the sticky one. Sticky envelopes travel on their own channel
// (h.stickyBroadcast, see BroadcastSticky doc), so a test that only waited
// on h.broadcast could register a client before the hub had processed a
// preceding BroadcastSticky call, making the test ordering assumptions
// unreliable.
func waitHubDrained(t *testing.T, h *Hub) {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(h.broadcast) == 0 && len(h.stickyBroadcast) == 0
	}, 5*time.Second, 10*time.Millisecond)
}

// TestHub_StickyEventSurvivesReplayBufferEviction covers task #547.
//
// The update-available notice is sent once, at server start, and then the
// replay ring evicts it: it is a bounded 2000-event buffer and a single
// streaming turn pushes thousands of deltas. An operator who starts the
// server, lets an agent run, and only then opens the browser would never see
// the badge -- which is the ordinary way people use a UI.
//
// This uses newClient(), the SAME constructor production code uses (see
// server.go's handleWS), so the client's send channel has the real
// production capacity (sendBufSize == 512), not a hand-sized channel that
// can never fill. That distinction is the whole bug: with a channel sized to
// outlast the replay (as the original version of this test used,
// maxBufferSize+200 == 2200), the sticky send at the end of the register
// case always finds room and the test passes whether or not sticky is
// prioritized over replay. With the real 512-slot channel, maxBufferSize+50
// (2050) noise events alone are enough to fill it before the sticky send
// gets a turn -- reproducing exactly the drop the reviewer's probe found.
//
// Revert-check: send sticky AFTER the replay in Run's register case (the
// original ordering) and this fails -- the late client's 512-slot channel is
// already full of noise by the time the sticky send is attempted, so it
// hits the default drop and the client never receives the update notice.
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

	// Let the hub drain both channels before anyone registers.
	waitHubDrained(t, h)

	// newClient is the production constructor: real sendBufSize (512), the
	// same channel every real WebSocket connection gets in handleWS.
	late := newClient(nil, nil)
	h.register <- late

	types := drainClient(t, late)
	require.Contains(t, types, EventUpdateAvailable,
		"a client connecting after one turn's worth of traffic never received the update notice -- the replay ring had already evicted it, or the sticky send lost the race for the client's bounded channel")
}

// TestHub_StickyEventDeliveredEvenWhenStillInReplayRing checks the other
// side of the ordering trade-off: when the sticky event is recent enough
// that an ordinary (non-sticky) copy of it would still be present in the
// replay ring, a newly registered client must receive the sticky type
// exactly once, not zero times (dropped) and not twice.
//
// Revert-check: make BroadcastSticky send on h.broadcast again (so the ring
// also stores sticky envelopes, as before this fix) instead of
// h.stickyBroadcast, and this fails -- the client receives
// EventUpdateAvailable twice: once from the sticky replay in the register
// case, once again from the ring replay.
func TestHub_StickyEventDeliveredEvenWhenStillInReplayRing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHub()
	go h.Run(ctx)

	// Sent recently -- with no other traffic, an ordinary broadcast of this
	// would still be the newest (and only) entry in the replay ring.
	h.BroadcastSticky(EventUpdateAvailable, UpdateAvailableWire{Current: "1.0.0", Latest: "1.1.0"})
	waitHubDrained(t, h)

	late := newClient(nil, nil)
	h.register <- late

	types := drainClient(t, late)
	count := 0
	for _, ty := range types {
		if ty == EventUpdateAvailable {
			count++
		}
	}
	require.Equal(t, 1, count, "a sticky event must be delivered exactly once, not duplicated via the replay ring")
}

// TestHub_StickyEventReplaySurvivesSupersededRingCopy is the regression test
// for the stale-wins bug found in review of the first version of this fix.
//
// Sequence:
//  1. BroadcastSticky(v1) -- an earlier fix stored this in the replay ring
//     too, alongside updating h.sticky["update_available"] = v1.
//  2. BroadcastSticky(v2) -- likewise stored v2 in the ring, and
//     h.sticky["update_available"] is now v2.
//  3. A client connects while BOTH v1 and v2 are still within the ring's
//     window (nothing has evicted them).
//
// The earlier fix sent the sticky map's newest entry (v2) first, then
// replayed the ring skipping only byte-identical entries. v1's envelope is
// NOT byte-identical to v2's (different payload), so replay was not
// skipping it: the client received v2 (sticky) then v1 (replay), leaving v1
// -- the SUPERSEDED value -- as the last update_available the client saw.
// The UI renders on receipt, so the stale copy would have won.
//
// This test asserts on the PAYLOAD of the last received copy, not just the
// count, because a count-only assertion (exactly one, or "contains") would
// have passed even with v1 surviving as long as only one generation reached
// the client -- it would not have caught which generation.
//
// Revert-check: make BroadcastSticky send on h.broadcast again (so v1 and v2
// both enter the replay ring) and this fails -- the last update_available
// payload the client receives is Latest 1.1.0 (v1), not 1.2.0 (v2).
func TestHub_StickyEventReplaySurvivesSupersededRingCopy(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHub()
	go h.Run(ctx)

	h.BroadcastSticky(EventUpdateAvailable, UpdateAvailableWire{Current: "1.0.0", Latest: "1.1.0"}) // v1
	h.BroadcastSticky(EventUpdateAvailable, UpdateAvailableWire{Current: "1.0.0", Latest: "1.2.0"}) // v2
	waitHubDrained(t, h)

	late := newClient(nil, nil)
	h.register <- late

	_, payloads := drainClientPayloads(t, late, EventUpdateAvailable)
	require.Len(t, payloads, 1, "the client must receive exactly one update_available event, not one per generation")

	var wire UpdateAvailableWire
	require.NoError(t, json.Unmarshal(payloads[len(payloads)-1], &wire))
	require.Equal(t, "1.2.0", wire.Latest,
		"the last update_available payload the client received must be the newest generation (v2), not a superseded copy replayed from the ring afterwards")
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
	waitHubDrained(t, h)

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
// arbitrary stale per-turn traffic. Uses newClient() (real production
// capacity) for consistency with the other tests in this file, though the
// assertion here doesn't depend on the buffer being large or small.
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
	waitHubDrained(t, h)

	require.Empty(t, h.stickyEvents(), "a plain Broadcast must never be retained as sticky")

	late := newClient(nil, nil)
	h.register <- late
	types := drainClient(t, late)
	require.NotContains(t, types, "ordinary",
		"an evicted ordinary event must stay evicted -- stickiness is opt-in")
}

// fakeUpdateClient is a minimal update.Client stub so this test can drive
// broadcastUpdateNoticeWithClientVersion without any real network call,
// and deterministically report a fixed "latest" tag.
type fakeUpdateClient struct{ latestTag string }

// Latest implements update.Client.
func (c fakeUpdateClient) Latest(_ context.Context) (*update.Release, error) {
	return &update.Release{TagName: c.latestTag, HTMLURL: "https://example.invalid/release"}, nil
}

// TestBroadcastUpdateNotice_DeliversThroughTheProductionEntryPoint closes the
// gap the other tests in this file leave open: every test above calls
// h.BroadcastSticky directly, so none of them actually exercises
// broadcastUpdateNotice / broadcastUpdateNoticeWithClientVersion in
// internal/server/update_notice.go -- the code that decides whether the
// update notice is sent sticky AT ALL (update_notice.go's h.BroadcastSticky
// call). A prior version of this file carried a comment claiming a
// revert-check against exactly that line, but no test here ever called
// through it, so the claim did not hold: switching update_notice.go's
// h.BroadcastSticky back to h.Broadcast left every test in this file green.
//
// This test drives the real broadcastUpdateNoticeWithClientVersion (the
// injectable core broadcastUpdateNotice itself is a thin wrapper around),
// with a fake update.Client so it needs no network access and no dependency
// on this fork's actual current version string, then asserts a late client
// still receives the notice -- the same replay-ring-eviction shape the
// other tests in this file cover for a hand-constructed BroadcastSticky
// call.
//
// Revert-check: change update_notice.go's h.BroadcastSticky(EventUpdateAvailable, ...)
// call back to h.Broadcast(EventUpdateAvailable, ...) and this test fails --
// the late client's replay-ring-overrun means the ordinary broadcast is long
// gone by the time it registers, so EventUpdateAvailable never arrives.
// Verified by literally doing that revert and running this test; see the
// task notes for the captured failure output.
func TestBroadcastUpdateNotice_DeliversThroughTheProductionEntryPoint(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHub()
	go h.Run(ctx)

	// "1.0.0" -> "1.1.0": both stable (no "-" prerelease marker), current !=
	// latest, and neither is a devel/pseudo-version -- so IsDevelopment() is
	// false and Available() is true, the same shape every other test in this
	// file drives directly via BroadcastSticky's UpdateAvailableWire.
	client := fakeUpdateClient{latestTag: "v1.1.0"}
	broadcastUpdateNoticeWithClientVersion(ctx, h, client, "1.0.0")

	// Overrun the replay ring exactly as the other sticky-survival tests in
	// this file do, so a plain (non-sticky) broadcast of this same event
	// could not possibly still be sitting in the ring by the time the late
	// client registers.
	for i := 0; i < maxBufferSize+50; i++ {
		h.Broadcast("noise", map[string]int{"i": i})
	}
	waitHubDrained(t, h)

	late := newClient(nil, nil)
	h.register <- late

	_, payloads := drainClientPayloads(t, late, EventUpdateAvailable)
	require.Len(t, payloads, 1,
		"a client connecting after one turn's worth of traffic must receive exactly one update_available event via the real broadcastUpdateNotice entry point, not zero (dropped) and not more than one")

	var wire UpdateAvailableWire
	require.NoError(t, json.Unmarshal(payloads[0], &wire))
	require.Equal(t, "1.0.0", wire.Current)
	require.Equal(t, "1.1.0", wire.Latest)
}
