package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
)

// Hub maintains connected clients and an event replay buffer.
//
// All accesses to clients and buffer happen inside the single Run() goroutine,
// so no mutex is needed for those fields. The broadcast channel serialises
// messages from multiple producer goroutines.
type Hub struct {
	clients    map[*Client]struct{}
	buffer     replayBuffer // ring replay buffer; only touched inside Run()
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client

	// liveWorkTitles caches async-work row titles (command/prompt text) for
	// the live-work panel (#1058). Per-hub, never package-global: it is
	// keyed by (ownerSessionID, toolCallID, inputHash) — see titleCache —
	// and must die with the server so one server's cache can never leak a
	// session's titles into another's panel.
	liveWorkTitles *TitleCache

	// stickyBroadcast carries only coalescing wakeup tokens. When
	// BroadcastSticky updates h.sticky, it non-blocking-sends a token here;
	// Run's stickyBroadcast case drains the channel, locks stickyMu, swaps
	// h.stickyPending with a fresh empty map, and fans out the current
	// envelope for each type in the swapped set. Multiple queued tokens
	// coalesce harmlessly: later tokens find an empty pending set and no-op.
	//
	// Sticky envelopes are NEVER pushed into h.buffer (the replay ring) --
	// see BroadcastSticky's doc for why this is required.
	stickyBroadcast chan struct{}
	configMu        sync.Mutex
	configPending   []byte
	configBroadcast chan struct{}
	deliveryBarrier chan chan struct{}

	// sticky holds the latest envelope for each event type that must reach
	// EVERY client, including ones that connect long after it was sent.
	//
	// The replay buffer cannot carry these. It is a bounded ring (2000
	// events / 16 MiB), and a single streaming turn pushes thousands of
	// delta events -- so an event sent at server start is the FIRST thing
	// evicted. That is exactly what happened to the update-available badge
	// (task #547): start the server, let one agent turn run, then open the
	// browser, and the notice was already gone. Opening the UI when you
	// need it rather than before is the common case, not the edge one.
	//
	// Keyed by event type, so a later send of the same event replaces the
	// earlier one instead of accumulating. Written from BroadcastSticky
	// (any goroutine) and read in Run's register case, hence the mutex --
	// unlike buffer, which Run alone touches.
	stickyMu sync.Mutex
	sticky   map[string][]byte

	// stickyPending is the set of sticky event types changed since Run last
	// drained the stickyBroadcast channel. BroadcastSticky adds a type to
	// this set (under stickyMu) and then non-blocking-sends a wakeup token.
	// Run's stickyBroadcast case swaps this map with a fresh empty one and
	// fans out the current envelope for each type in the swapped set.
	stickyPending map[string]struct{}

	// stopped is closed exactly once, at the top of Run's ctx.Done() exit
	// branch — the hub's commit point. Once closed, the loop will never
	// activate another client: registrations still racing into the register
	// buffer are torn down by the drain that immediately follows the close,
	// and any send that lands after that drain is caught by the sender's own
	// post-send check in tryRegister (the close happens-before the drain, so
	// a send the drain misses necessarily completed after the close and is
	// therefore observable by the re-check). This mirrors the Server-level
	// shutdownSignal style from task #714, but tracks the hub's ACTUAL stop,
	// not a shutdown request.
	stopped chan struct{}
}

func NewHub() *Hub {
	return &Hub{
		clients:         make(map[*Client]struct{}),
		buffer:          newReplayBuffer(),
		broadcast:       make(chan []byte, 1024),
		register:        make(chan *Client, 64),
		unregister:      make(chan *Client, 64),
		sticky:          make(map[string][]byte),
		stickyPending:   make(map[string]struct{}),
		stickyBroadcast: make(chan struct{}, 64),
		configBroadcast: make(chan struct{}, 1),
		deliveryBarrier: make(chan chan struct{}),
		stopped:         make(chan struct{}),
		liveWorkTitles:  NewTitleCache(),
	}
}

// Run is the hub's single event loop. It must be called in its own goroutine.
func (h *Hub) Run(ctx context.Context) {
	var pendingBarriers []chan struct{}
	for {
		select {
		case <-ctx.Done():
			// Commit to exit before anything else: closing stopped tells
			// tryRegister this hub will never activate another client. It MUST
			// precede the register drain below — a registration landing in the
			// buffer after the drain can then only be one whose send completed
			// after this close, which the sender's post-send check is guaranteed
			// to observe (see tryRegister).
			close(h.stopped)
			for _, done := range pendingBarriers {
				close(done)
			}
			// Registrations that won the shutdown race into the buffer still
			// get the same teardown as active clients: close(c.send) so their
			// writePump — possibly not even started yet — shuts the connection
			// down cleanly instead of parking on a channel nobody drains.
			for {
				select {
				case c := <-h.register:
					c.closeSend()
				default:
					for c := range h.clients {
						c.closeSend()
					}
					return
				}
			}

		case c := <-h.register:
			// Add to active set first so no broadcasts are lost after this point.
			h.clients[c] = struct{}{}
			// Sticky events go FIRST, before the replay. c.send is a bounded
			// channel (sendBufSize == 512 in production) and the replay below
			// can be thousands of events for a single streaming turn -- if
			// sticky went last, the replay alone fills every slot and the
			// sticky send hits the `default:` drop. That is precisely the
			// traffic pattern sticky exists to survive (see the sticky
			// field's doc), so ordering it after the replay made the
			// guarantee false under its own motivating case. Sending sticky
			// first means it only competes with itself for the first few
			// slots -- today that's a single event type
			// (EventUpdateAvailable), so delivery is effectively guaranteed
			// as long as the number of distinct sticky types stays small
			// relative to sendBufSize.
			//
			// INVARIANT: a client must never end up with a superseded copy of
			// a sticky event as the last one it received. This holds because
			// (1) h.sticky keeps at most one -- the newest -- envelope per
			// event type (see BroadcastSticky), and (2) sticky envelopes are
			// NEVER stored in the replay ring (BroadcastSticky sends on
			// stickyBroadcast, a channel Run drains WITHOUT calling
			// buffer.push -- see that case below). So there is no older
			// generation of a sticky event anywhere for the replay step to
			// hand the client after this loop already sent the current one.
			// No dedup bookkeeping is needed, unlike an earlier version of
			// this code that sent sticky first but still stored every
			// generation in the ring: that let a stale copy ride the replay
			// in AFTER the current one and become "last received", which is
			// exactly the bug this invariant rules out.
			//
			// Trade-off accepted: a late-joining client now sees the sticky
			// event (e.g. the update badge) before the replayed history
			// instead of after. That is the right UX for this feature --
			// "you have an update" is a standing banner, not something that
			// needs to slot into turn-by-turn history order -- and it is the
			// only ordering that can be made reliable against a bounded
			// channel without blocking or growing it unboundedly.
			for _, msg := range h.stickyEvents() {
				meta := newLazyMeta(msg)
				if c.auth != nil {
					msg = c.auth.filter(msg, meta)
					if msg == nil {
						continue
					}
				}
				select {
				case c.send <- msg:
				default:
					// Client buffer full already (e.g. many sticky types).
				}
			}
			// Then replay buffered events, applying the same per-client filter
			// as live delivery so retained sensitive state cannot leak.
			h.buffer.forEach(func(msg []byte) {
				meta := newLazyMeta(msg)
				if c.auth != nil {
					msg = c.auth.filter(msg, meta)
					if msg == nil {
						return
					}
				}
				select {
				case c.send <- msg:
				default:
					// Client buffer full; skip older replayed events rather than block.
				}
			})

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				c.closeSend()
			}

		case <-h.configBroadcast:
			h.configMu.Lock()
			msg := h.configPending
			h.configPending = nil
			h.configMu.Unlock()
			if msg != nil {
				h.deliver(msg)
			}
			if len(pendingBarriers) > 0 {
				h.configMu.Lock()
				pending := h.configPending != nil
				h.configMu.Unlock()
				if !pending {
					for _, done := range pendingBarriers {
						close(done)
					}
					pendingBarriers = nil
				}
			}
		case done := <-h.deliveryBarrier:
			for {
				select {
				case msg := <-h.broadcast:
					h.deliver(msg)
				case <-h.stickyBroadcast:
					h.fanOutSticky()
				default:
					goto broadcastsDrained
				}
			}
		broadcastsDrained:
			h.configMu.Lock()
			pending := h.configPending != nil
			h.configMu.Unlock()
			if pending {
				pendingBarriers = append(pendingBarriers, done)
			} else {
				close(done)
			}
		case msg := <-h.broadcast:
			h.deliver(msg)

		case <-h.stickyBroadcast:
			// Deliberately NOT h.buffer.push: sticky envelopes must never
			// enter the replay ring. h.sticky (updated synchronously in
			// BroadcastSticky, before this case ever runs) already keeps the
			// one, newest copy for late-joining clients -- see the register case
			// above. If a superseded copy also lived in the ring, a client
			// connecting while both generations are still within the ring's
			// window could receive the sticky send (newest) followed by the
			// ring's older copy during replay, ending up with the stale value
			// as "last received". Keeping sticky envelopes out of the ring
			// entirely is what makes that impossible, without per-event
			// bookkeeping on every replay.
			//
			// Coalescing wakeup signal: this channel only carries empty tokens.
			// We swap h.stickyPending with a fresh empty map under stickyMu,
			// then fan out the current envelope for each type in the swapped
			// set. Multiple queued tokens coalesce harmlessly: later tokens
			// find an empty pending set and no-op. A dropped token is harmless:
			// it only occurs when the channel is full, meaning at least one
			// token is already queued, and any drain reads stickyPending which
			// already contains the changed types.
			h.fanOutSticky()
		}
	}
}

// fanOutSticky delivers the pending sticky envelopes to the connected clients.
// Called from Run only.
func (h *Hub) fanOutSticky() {
	h.stickyMu.Lock()
	pending := h.stickyPending
	h.stickyPending = make(map[string]struct{})
	h.stickyMu.Unlock()

	for msgType := range pending {
		h.stickyMu.Lock()
		env, ok := h.sticky[msgType]
		h.stickyMu.Unlock()
		if !ok {
			continue
		}
		// Fan-out to already-connected clients only; identical to the
		// broadcast case's fan-out.
		meta := newLazyMeta(env)
		for c := range h.clients {
			send := env
			if c.auth != nil {
				send = c.auth.filter(env, meta)
				if send == nil {
					continue
				}
			}
			select {
			case c.send <- send:
			default:
				slog.Debug("ws: slow client, dropping sticky message")
			}
		}
	}
}

// tryRegister hands c to the hub's event loop, returning false when the
// caller must tear the client down itself (close the socket, stop the
// worker pool) because the hub has stopped — either before the attempt,
// or concurrently while the send was in flight. A bare racy select
// between the register send and ctx.Done() cannot close that window:
// when the buffer has space and ctx is cancelled both cases are ready
// and Go picks randomly, so the send can succeed into a channel Run has
// already stopped draining. The stopped channel closes before Run's
// final register drain, which makes checking it on BOTH sides of the
// send airtight: a send the drain never saw necessarily completed after
// the close, so the post-send check observes it. The ctx fallback only
// remains to unblock a send parked on a full buffer during shutdown.
func (h *Hub) TryRegister(ctx context.Context, c *Client) bool {
	select {
	case <-h.stopped:
		return false
	default:
	}
	select {
	case h.register <- c:
		select {
		case <-h.stopped:
			return false
		default:
		}
		return true
	case <-ctx.Done():
		return false
	}
}

func (h *Hub) deliver(msg []byte) {
	_ = h.buffer.push(msg)
	meta := newLazyMeta(msg)
	for c := range h.clients {
		send := msg
		if c.auth != nil {
			send = c.auth.filter(msg, meta)
			if send == nil {
				continue
			}
		}
		select {
		case c.send <- send:
		default:
			slog.Debug("ws: slow client, dropping message")
		}
	}
}

func (h *Hub) WaitDelivery() <-chan struct{} {
	done := make(chan struct{})
	select {
	case h.deliveryBarrier <- done:
	case <-h.stopped:
		close(done)
	}
	return done
}

func (h *Hub) BroadcastConfig(payload any) bool {
	select {
	case <-h.stopped:
		return false
	default:
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Error("ws: marshal config broadcast payload", "err", err)
		return false
	}
	env, err := json.Marshal(wire.WSMessage{Type: wire.EventConfig, Payload: raw})
	if err != nil {
		slog.Error("ws: marshal config broadcast envelope", "err", err)
		return false
	}
	h.configMu.Lock()
	h.configPending = env
	h.configMu.Unlock()
	select {
	case h.configBroadcast <- struct{}{}:
	default:
	}
	return true
}

// Broadcast encodes a typed event and queues it for all clients + the replay buffer.
// wire.EventConfig is instead routed through BroadcastConfig's coalescing slot.
// Safe to call from any goroutine; never blocks (drops on full channel).
func (h *Hub) Broadcast(msgType string, payload any) {
	if msgType == wire.EventConfig {
		h.BroadcastConfig(payload)
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Error("ws: marshal broadcast payload", "type", msgType, "err", err)
		return
	}
	env, err := json.Marshal(wire.WSMessage{Type: msgType, Payload: raw})
	if err != nil {
		slog.Error("ws: marshal broadcast envelope", "err", err)
		return
	}
	select {
	case h.broadcast <- env:
	default:
		slog.Warn("ws: broadcast channel full, dropping", "type", msgType)
	}
}

// BroadcastSticky is Broadcast plus a promise: every client that connects
// later also receives this event, regardless of how much traffic has passed
// through the replay ring in between, AND every already-connected client's
// LAST received copy of the type is always the newest one sent -- never a
// generation BroadcastSticky has since superseded. The "always newest last"
// guarantee now holds even when the stickyBroadcast channel overflows:
// every already-connected client's last received copy is the newest one at
// drain time, because the envelope is read from h.sticky at drain (not
// carried in the channel), and a dropped wakeup token is harmless (it only
// occurs when the channel is full, meaning at least one token is already
// queued, and any drain reads stickyPending which already contains the
// changed types). Run's register case delivers sticky envelopes to a newly
// connected client BEFORE replaying the ring, precisely so a large replay
// (thousands of deltas from one streaming turn) cannot fill the client's
// bounded send channel first and starve the sticky send under a `default:`
// drop -- see the register case in Run for the full reasoning. The promise
// is bounded by sendBufSize: it holds as long as the number of distinct
// sticky event types stays small relative to it, since sticky sends still
// compete with each other (not with the replay) for the same non-blocking
// channel.
//
// Use it only for state a late-joining client genuinely needs and cannot ask
// for -- there is no polling and no request/response path for it. The update
// badge is the motivating case. Ordinary per-turn traffic must NOT be sticky:
// each event type keeps exactly one entry, so a high-frequency event would
// just churn the map while telling a new client something already stale.
func (h *Hub) BroadcastSticky(msgType string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Error("ws: marshal sticky broadcast payload", "type", msgType, "err", err)
		return
	}
	env, err := json.Marshal(wire.WSMessage{Type: msgType, Payload: raw})
	if err != nil {
		slog.Error("ws: marshal sticky broadcast envelope", "err", err)
		return
	}

	// Recorded BEFORE the send, so a client registering concurrently gets it
	// from one path or the other -- never neither. Getting it twice is
	// harmless (the same envelope, and the UI renders on receipt); getting it
	// zero times is the bug this exists to close. The envelope is stored in
	// h.sticky under stickyMu, and the type is added to h.stickyPending.
	h.stickyMu.Lock()
	h.sticky[msgType] = env
	h.stickyPending[msgType] = struct{}{}
	h.stickyMu.Unlock()

	// Sent on stickyBroadcast, NOT broadcast: Run's stickyBroadcast case
	// fans this out to already-connected clients exactly like an ordinary
	// broadcast, but skips buffer.push -- sticky envelopes must never enter
	// the replay ring (see the register case and the stickyBroadcast case
	// in Run for why: a superseded copy in the ring could otherwise reach a
	// late client AFTER the current one and win as "last received"). Only
	// an empty wakeup token is sent; the envelope is read from h.sticky at
	// drain time. A dropped token is harmless: it only occurs when the
	// channel is full, meaning at least one token is already queued, and any
	// drain reads stickyPending which already contains this type.
	select {
	case h.stickyBroadcast <- struct{}{}:
	default:
		slog.Debug("ws: sticky broadcast channel full, dropping harmless wakeup token", "type", msgType)
	}
}

// stickyEvents returns a snapshot of the sticky envelopes for replay to a
// newly registered client. Copied under the mutex so Run's loop never holds
// it while writing to a client's channel.
func (h *Hub) stickyEvents() [][]byte {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	if len(h.sticky) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(h.sticky))
	for _, env := range h.sticky {
		out = append(out, env)
	}
	return out
}

// LiveWorkTitles exposes the hub's per-hub live-work title cache. Designed
// seam (sprint 0 S4, design §2.2/§3): handlers read the cache through this
// method instead of the unexported field (provenance: handlers_livework.go
// read c.hub.liveWorkTitles / p.h.liveWorkTitles at HEAD).
func (h *Hub) LiveWorkTitles() *TitleCache { return h.liveWorkTitles }

// The accessors below are exported for root call sites that still need to
// interact with hub state before their migration; production code uses internal
// fields directly.

// Register hands a client to the hub's register channel (readpump-style
// fixtures; provenance: tests wrote h.register <- c at HEAD, e.g.
// handlers_models_test.go:84). Admission goes through TryRegister in
// production (gate.handleWS).
func (h *Hub) Register(c *Client) { h.register <- c }

// Unregister detaches a client via the hub's unregister channel (provenance:
// tests wrote h.unregister <- c at HEAD, e.g.
// readpump_control_starvation_test.go:366).
func (h *Hub) Unregister(c *Client) { h.unregister <- c }

// BroadcastChan exposes the broadcast channel in receive-only form
// (provenance: handlers_config_persistence_test.go:168,168 read
// c.hub.broadcast at HEAD).
func (h *Hub) BroadcastChan() <-chan []byte { return h.broadcast }

// ConfigBroadcastChan exposes the config coalescing channel in receive-only
// form (provenance: handlers_config_persistence_test.go:163 read
// c.hub.configBroadcast at HEAD).
func (h *Hub) ConfigBroadcastChan() <-chan struct{} { return h.configBroadcast }

func (h *Hub) DeliveryBarrierChan() chan<- chan struct{} { return h.deliveryBarrier }

// StoppedChan exposes the stopped channel in receive-only form (provenance:
// handlers_shutdown_test.go read s.hub.stopped at HEAD; the production
// close is Run's ctx.Done() branch only).
func (h *Hub) StoppedChan() <-chan struct{} { return h.stopped }

// TestHookCloseStopped closes the stopped channel exactly once — test seam
// for events_config_counters_test.go:28, which simulated an already-stopped
// hub without running Run. Never call from production code.
func (h *Hub) TestHookCloseStopped() { close(h.stopped) }

// StickyBroadcastChan exposes the sticky wakeup channel in receive-only form
// (exported for root call sites that still inspect the queue until L7;
// provenance: p547_sticky_update_notice_test.go reads its length).
// p547_sticky_update_notice_test.go used h.stickyBroadcast at HEAD).
func (h *Hub) StickyBroadcastChan() <-chan struct{} { return h.stickyBroadcast }

// BroadcastBlocking performs a blocking send on the hub broadcast channel —
// exported for the staying server test
// p547_sticky_update_notice_test.go, whose broadcastBlocking helper must
// saturate the replay ring without the lossy non-blocking Broadcast admission
// path (provenance: `h.broadcast <- env` at HEAD). Production code never
// blocks on broadcast.
func (h *Hub) BroadcastBlocking(msg []byte) { h.broadcast <- msg }

// BroadcastBlockingChan exposes the broadcast channel's capacity check form
// for the staying test events_config_counters_test.go:23, which fills the
// channel to capacity to drive the coalescing queue (exported for the staying
// server test). Returns the same channel as BroadcastChan; named separately so
// call sites document intent.
func (h *Hub) BroadcastBlockingChan() chan []byte { return h.broadcast }

// PendingConfigEnvelope snapshots the coalescing config slot for staying
// root callers that inspect the pending envelope
// under configMu (handlers_config_lock_trusted_test.go pendingConfigEnvelope,
// handlers_config_persistence_test.go at HEAD). Production readers drain the
// slot in Run only.
func (h *Hub) PendingConfigEnvelope() []byte {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	return h.configPending
}

// TakePendingConfigEnvelope reads and clears the coalescing config slot under
// configMu (exported for the staying server test
// handlers_config_mapping_test.go readConfigEvent drained the slot directly
// (hub.configMu/configPending at HEAD). Run drains the slot in production.
func (h *Hub) TakePendingConfigEnvelope() []byte {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	raw := h.configPending
	h.configPending = nil
	return raw
}
