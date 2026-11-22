package server

// Sprint 0 L4 (S4): this file is the package-server remainder of the hub-row
// sticky-update-notice tests (design §2.5). The seven hub-white tests moved to
// internal/serve/server/hub/p547_sticky_update_notice_test.go; this test
// stays in package server because its subject is broadcastUpdateNotice
// WithClientVersion (update_notice.go, composer/entry-point code with
// internal/update + internal/app dependencies that §2.6 forbids for hub).
// The helpers below are small verbatim local copies of the hub-side helpers
// this test needs (hub/p547_sticky_update_notice_test.go at sprint-0 L4,
// originally p547_sticky_update_notice_test.go:30-91,563-573 at HEAD), readapted
// to the exports for root call sites (SendChan/Register/BroadcastChan/…). servertest
// (L6/L7) absorbs them.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/ops/update"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/stretchr/testify/require"
)

// hubDrainFailAfter is the purely-defensive upper bound on waitHubDrained.
// It exists ONLY to turn "Hub.Run never drained at all" (a real regression:
// Run not started, or stopped) into a fast, clearly-labelled test failure
// instead of an infinitely spinning test that hangs CI. It is a var, not a
// const, solely so the bound's own regression test can shrink it — 60s is
// far beyond any legitimate drain (the drain itself completes in
// microseconds once Run is scheduled; #632's original flake was a 5s
// WALL-CLOCK bound racing transient machine stalls, which is why this
// bound is deliberately an order of magnitude larger and is a floor on
// failure, not a ceiling on success).
// Stored as int64 nanoseconds for atomic access.
var hubDrainFailAfter = int64((60 * time.Second).Nanoseconds())

// waitHubDrained blocks until the hub has finished processing everything
// producers have queued so far, on both the ordinary broadcast channel and
// the sticky one. Sticky envelopes travel on their own channel
// (h.stickyBroadcast, see BroadcastSticky doc), so a test that only waited
// on h.broadcast could register a client before the hub had processed a
// preceding BroadcastSticky call, making the test ordering assumptions
// unreliable. The wait is condition-based, not wall-clock-based: it returns
// as soon as both channels are empty, so transient process-wide stalls that
// outlast any small timer (#632: eight parallel tests failed together on a
// 5s require.Eventually bound) cannot make it fail while the drain still
// completes. The deadline below is an order-of-magnitude-larger safety net
// that fires only if the drain GENUINELY never happens.
func waitHubDrained(t testing.TB, h *Hub) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(atomic.LoadInt64(&hubDrainFailAfter)))
	for len(h.BroadcastChan()) != 0 || len(h.StickyBroadcastChan()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("hub drain never completed within %s: len(broadcast)=%d len(stickyBroadcast)=%d — Hub.Run is not draining (never started, or stopped). This is a real drain regression, NOT the old #632 wall-clock flake: the wait is condition-based and cannot fire merely because the process stalled.",
				time.Duration(atomic.LoadInt64(&hubDrainFailAfter)), len(h.BroadcastChan()), len(h.StickyBroadcastChan()))
		}
		time.Sleep(time.Millisecond)
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
		case raw, ok := <-c.SendChan():
			if !ok {
				return types, payloads
			}
			var env wire.WSMessage
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

// broadcastBlocking is Broadcast minus the drop-on-full admission: it sends
// the marshalled envelope on h.broadcast with a BLOCKING send, so the event
// is guaranteed to reach Hub.Run's broadcast case (Run drains the channel,
// so the send always completes). Tests that assert on what the replay ring
// retained must use this, not Broadcast: Broadcast's select/default drops
// frames once the 1024-slot channel fills, and under parallel-test load Run
// can fall behind enough that the producer overruns it. #621: the loop below
// needs >= maxBufferSize noise events to actually reach the ring for
// "ordinary" to be evicted; Broadcast's admission dropped 52 of them on a
// loaded machine, leaving "ordinary" alive in the ring and failing the test
// on exactly the frames it was asserting about.
func broadcastBlocking(t *testing.T, h *Hub, msgType string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	env, err := json.Marshal(wire.WSMessage{Type: msgType, Payload: raw})
	require.NoError(t, err)
	h.BroadcastBlocking(env)
}

// fakeUpdateClient local copy (verbatim; provenance
// hub/p547_sticky_update_notice_test.go at sprint-0 L4).
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

	h := NewHub()
	go h.Run(ctx)

	// "1.0.0" -> "1.1.0": both stable (no "-" prerelease marker), current !=
	// latest, and neither is a devel/pseudo-version -- so IsDevelopment() is
	// false and Available() is true, the same shape every other test in this
	// file drives directly via BroadcastSticky's wire.UpdateAvailableWire.
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
	h.Register(late)

	_, payloads := drainClientPayloads(t, late, wire.EventUpdateAvailable)
	require.Len(t, payloads, 1,
		"a client connecting after one turn's worth of traffic must receive exactly one update_available event via the real broadcastUpdateNotice entry point, not zero (dropped) and not more than one")

	var env wire.UpdateAvailableWire
	require.NoError(t, json.Unmarshal(payloads[0], &env))
	require.Equal(t, "1.0.0", env.Current)
	require.Equal(t, "1.1.0", env.Latest)
}
