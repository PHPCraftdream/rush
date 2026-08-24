package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 20 * 1024 * 1024 // 20 MB — supports image attachments
	sendBufSize    = 512
	maxBufferSize  = 2000 // max events to replay to new clients

	// maxConcurrentHandlersPerConn bounds how many handleX goroutines a single
	// WebSocket connection may have in flight at once. Each handler can hold a
	// SQLite connection and/or call into the agent coordinator, so an unbounded
	// client (malicious or buggy UI firing thousands of messages) would
	// otherwise be able to exhaust the process's goroutine pool and DB
	// connections. 12 is generous headroom above realistic UI concurrency (a
	// handful of tabs each issuing a couple of in-flight requests) while still
	// capping worst-case blast radius per connection. The cap is enforced by
	// the fixed worker pool (see workerPoolSize/startWorkers): once every
	// worker is busy, further work frames WAIT in the bounded work queue
	// (workQueueDepth) instead of spawning more goroutines, and once that
	// queue is also full, dispatch rejects the frame with an overload error
	// rather than blocking readPump — see workQueueDepth and dispatch's own
	// docs for why the reader must never be the backpressure point (#612).
	maxConcurrentHandlersPerConn = 12

	// maxConcurrentControlHandlersPerConn bounds control-plane dispatches (see
	// dispatchControl) — currently CmdCancelAgent, CmdInterruptAndSend, and
	// CmdShutdownServer. These never call into AgentCoordinator.Run: they look
	// up an in-memory map, invoke a stored context.CancelFunc, and/or do a
	// bounded DB read + local recompute, so they return in well under a second
	// even under load. The cap is much larger than
	// maxConcurrentHandlersPerConn (which gates genuinely long-running,
	// minutes-long agent turns) purely as a sanity backstop against a
	// buggy/malicious client flooding cancel/interrupt/shutdown frames — it
	// is not expected to ever bind in practice.
	maxConcurrentControlHandlersPerConn = 256

	// workQueueDepth bounds how many work-shaped dispatches (see dispatch) may
	// be queued, waiting for a free worker, on top of the
	// maxConcurrentHandlersPerConn already running. Together with the fixed
	// worker pool below this replaces the old "block the caller" backpressure
	// (c.sem <- struct{}{} inside dispatch): that design blocked readPump
	// itself once the cap was hit, which meant a Cancel/Interrupt frame sent
	// right behind a 13th work frame could never even be read off the socket
	// — see #612. The queue gives the same bounded-concurrency guarantee
	// (never more than maxConcurrentHandlersPerConn handlers running at
	// once) without ever blocking the reader: dispatch enqueues
	// non-blockingly and replies with an overload error instead of blocking
	// when the queue is also full.
	workQueueDepth = 64

	// workerPoolSize is the number of long-lived goroutines draining a
	// Client's work queue. It equals maxConcurrentHandlersPerConn so the
	// concurrency cap is unchanged from the old semaphore's behavior — at
	// most this many handleX calls run at once for one connection.
	workerPoolSize = maxConcurrentHandlersPerConn

	// replayByteBudget bounds the total bytes held in the replay buffer across all
	// events. Normal traffic (small JSON deltas) fits ~2000 events well under 8 MiB,
	// so the per-event count limit stays the binding constraint and full history is
	// preserved; this only kicks in for pathological streams (e.g. hundreds of large
	// growing-message snapshots), hard-capping a single hub's replay memory.
	replayByteBudget = 16 * 1024 * 1024 // 16 MiB

	// replayMaxEventSize is the per-event ceiling for STORAGE in the replay buffer.
	// Events larger than this are almost always image attachments or huge tool
	// outputs; a newly-connecting client doesn't need them re-transmitted from the
	// buffer (they're already persisted in the DB/message layer the UI loads on
	// initial fetch). Skipping storage here (1) stops one blob evicting dozens of
	// normal events, (2) bounds the byte-budget eviction loop. Live fan-out to
	// already-connected clients is unaffected.
	replayMaxEventSize = 1024 * 1024 // 1 MiB per event
)

// workItem is one queued work-shaped dispatch waiting for a free worker.
type workItem struct {
	name string
	fn   func()
}

// Client represents a single WebSocket connection.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte

	// workQueue is the bounded queue work-shaped dispatches (see dispatch)
	// are handed to. A fixed pool of workerPoolSize worker goroutines (see
	// startWorkers) drains it, so at most workerPoolSize handleX calls run
	// concurrently for this connection — the same cap
	// maxConcurrentHandlersPerConn enforced before. Unlike the old semaphore,
	// enqueueing here is always non-blocking: dispatch does a `select` with
	// a `default` and replies with an overload error instead of blocking
	// the caller (readPump) when the queue is also full. See #612: blocking
	// readPump itself meant a Cancel/Interrupt frame sent right behind a
	// 13th work frame could never even be read off the socket.
	workQueue chan workItem

	// controlSem is the analogous semaphore for control-plane dispatches
	// (see dispatchControl) — a separate, much larger pool so a control-plane
	// frame (cancel/interrupt/shutdown) is never queued behind long-running
	// work slots. Keeping it a distinct resource (rather than sharing the work
	// queue/pool) is the whole point of the fix: control-plane commands must
	// not compete with long-running handlers for the same bounded resource.
	// Admission is non-blocking (see dispatchControl): it never blocks
	// readPump either, it just has enough headroom that overload should
	// never be observed in practice.
	controlSem chan struct{}

	// workersOnce guards starting the work-queue worker pool exactly once
	// per Client (see startWorkers).
	workersOnce sync.Once
}

// newClient constructs a Client with its per-connection handler queue/
// semaphore initialised. Always use this instead of a bare &Client{}
// literal so they are never nil.
func newClient(hub *Hub, conn *websocket.Conn) *Client {
	c := &Client{
		hub:        hub,
		conn:       conn,
		send:       make(chan []byte, sendBufSize),
		workQueue:  make(chan workItem, workQueueDepth),
		controlSem: make(chan struct{}, maxConcurrentControlHandlersPerConn),
	}
	c.startWorkers()
	return c
}

// startWorkers launches the fixed pool of goroutines that drain workQueue.
// Called once from newClient. Each worker loops for the connection's
// lifetime, running one workItem at a time with the same panic-recovery
// safety net dispatch always provided; the pool itself (rather than a
// per-item goroutine) is what bounds concurrency to workerPoolSize without
// ever blocking the producer (dispatch/readPump).
func (c *Client) startWorkers() {
	c.workersOnce.Do(func() {
		for i := 0; i < workerPoolSize; i++ {
			go func() {
				for item := range c.workQueue {
					runRecovered(item.name, func() {}, item.fn)
				}
			}()
		}
	})
}

// dispatch hands fn to the connection's bounded work queue, subject to two
// protections:
//
//   - Concurrency limit: at most workerPoolSize (== maxConcurrentHandlersPerConn)
//     work items run at once for this connection, enforced by the fixed
//     worker pool started in startWorkers. Once all workers are busy,
//     further items simply wait IN THE QUEUE (not by blocking the caller —
//     see below) — same bounded-concurrency guarantee as before.
//   - Non-blocking admission: enqueueing is a `select` with a `default`
//     branch. If the queue is also full (workQueueDepth items already
//     waiting on top of workerPoolSize running), dispatch does NOT block
//     the caller (readPump, via handleIncoming) — it replies with an
//     overload error and drops the item. Blocking the caller here was the
//     root cause of #612: while readPump was blocked acquiring a slot, it
//     never called conn.ReadMessage() again, so a Cancel/Interrupt frame
//     sent right behind the frame that filled the cap could never even be
//     read off the socket, let alone dispatched via the separate
//     control-plane path.
//   - Panic isolation: recovers any panic inside fn, logs it (with a stack
//     trace) instead of propagating it. Without this, a nil-deref or
//     out-of-range index triggered by an unexpected payload in ANY handler
//     would crash the entire rush web process, taking down every other
//     connection and in-flight agent session with it.
//
// name is a short identifier (typically the WS command type or handler
// function name) used only for logging. msgID is the originating message's
// correlation ID, used only to shape the overload reply.
func (c *Client) dispatch(name, msgID string, fn func()) {
	select {
	case c.workQueue <- workItem{name: name, fn: fn}:
	default:
		slog.Warn("ws: work queue full, rejecting", "handler", name)
		c.reply(msgID, EventError, nil, "server busy, please retry")
	}
}

// dispatchControl is dispatch's counterpart for control-plane commands —
// currently CmdCancelAgent, CmdInterruptAndSend, and CmdShutdownServer. It
// runs fn in a new goroutine gated by controlSem (a separate, generously-sized
// semaphore) instead of the work queue, and provides the SAME panic-recovery
// safety net as dispatch.
//
// Why this needs to exist at all: handleIncoming (handlers.go) calls dispatch
// synchronously from readPump's read loop (server.go). If a cancel/interrupt
// command went through the same bounded work queue as long-running handlers
// (e.g. handleSendMessage, which can hold a worker for an entire
// multi-minute agent turn), it would queue behind however many work items
// are already ahead of it — exactly when the user most needs it to go
// through immediately, e.g. to cancel one of the stuck turns.
//
// Admission here is ALSO non-blocking (select/default), for the same reason
// dispatch's is: readPump must never block acquiring a slot, or it stops
// calling conn.ReadMessage(), the pong handler never runs to extend the read
// deadline, and the connection is torn down by an i/o timeout ~60s later
// (pongWait) — during which the user had no way to cancel anything on this
// connection. controlSem's generous size (256) means the `default` branch
// is not expected to ever be taken in practice; it exists as a bounded
// worst case against a buggy/malicious client flooding control-plane
// frames, consistent with never spawning unbounded goroutines.
func (c *Client) dispatchControl(name, msgID string, fn func()) {
	select {
	case c.controlSem <- struct{}{}:
		go runRecovered(name, func() { <-c.controlSem }, fn)
	default:
		slog.Warn("ws: control queue full, rejecting", "handler", name)
		c.reply(msgID, EventError, nil, "server busy, please retry")
	}
}

// runRecovered runs fn with panic isolation identical to dispatch's original
// behaviour: any panic is recovered and logged (with a stack trace) instead
// of propagating and crashing the process. release is always called first,
// via defer, so the calling semaphore slot is freed even if fn panics.
func runRecovered(name string, release func(), fn func()) {
	defer release()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("ws: handler panic",
				"handler", name,
				"panic", r,
				"stack", string(debug.Stack()))
		}
	}()
	fn()
}

// replayBuffer is a fixed-capacity ring buffer of broadcast events.
//
// It enforces three independent bounds:
//   - count:     at most cap entries (legacy maxBufferSize semantic),
//   - bytes:     total stored bytes <= replayByteBudget,
//   - per-event: events larger than replayMaxEventSize are not stored at all.
//
// All ops are O(1): the backing slice is pre-allocated once and never re-sliced
// or appended to, so eviction never copies the underlying array.
//
// NOTE: a cleaner long-term design would store only the latest snapshot per
// entity (session/message/tool) and serve it to new clients via REST/DB instead
// of replaying intermediate events. That is a larger architectural change and is
// intentionally out of scope here; see task #130.
type replayBuffer struct {
	buf   [][]byte // len == cap, pre-allocated; entries recycled in place
	head  int      // index of the oldest stored entry
	tail  int      // index where the next push writes
	count int      // number of valid entries, count <= len(buf)
	bytes int      // sum of len() of valid entries
}

// newReplayBuffer returns an empty ring buffer with capacity maxBufferSize.
//
// This always allocates a maxBufferSize-length []byte slice up front (2000
// nil pointers, ~16 KiB on 64-bit) regardless of how much traffic the hub
// ever sees. That's a deliberate, negligible cost today because there is
// exactly one Hub (and therefore one replayBuffer) per server process — see
// the single newHub() call in server.go. If the hub model ever changes to
// one-per-session (many hubs alive concurrently), this eager allocation
// should be revisited (e.g. lazy/smaller initial capacity that grows), since
// the per-hub cost would then multiply by the number of live sessions.
func newReplayBuffer() replayBuffer {
	return replayBuffer{buf: make([][]byte, maxBufferSize)}
}

// push appends msg, evicting oldest entries as needed to satisfy the count and
// byte-budget bounds. Events exceeding replayMaxEventSize are rejected (not
// stored) and reported via slog at debug level. Returns true if stored.
func (r *replayBuffer) push(msg []byte) bool {
	if len(msg) > replayMaxEventSize {
		slog.Debug("ws: replay event exceeds per-event limit, not buffering",
			"len", len(msg), "limit", replayMaxEventSize)
		return false
	}
	capN := len(r.buf)
	if r.count == capN { // count bound: full -> evict oldest
		r.evictHead()
	}
	r.buf[r.tail] = msg
	r.tail = (r.tail + 1) % capN
	r.count++
	r.bytes += len(msg)
	// Byte-budget bound: evict oldest until within budget. Guard count > 1 so
	// the just-pushed event (<= replayMaxEventSize <= replayByteBudget) is kept.
	for r.bytes > replayByteBudget && r.count > 1 {
		r.evictHead()
	}
	return true
}

// evictHead removes the oldest entry. Caller guarantees count >= 1.
func (r *replayBuffer) evictHead() {
	capN := len(r.buf)
	r.bytes -= len(r.buf[r.head])
	r.buf[r.head] = nil // drop reference so the slice can be GC'd
	r.head = (r.head + 1) % capN
	r.count--
}

// forEach applies fn to each stored entry in oldest-to-newest order.
func (r *replayBuffer) forEach(fn func([]byte)) {
	capN := len(r.buf)
	for i := 0; i < r.count; i++ {
		fn(r.buf[(r.head+i)%capN])
	}
}

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
}

func newHub() *Hub {
	return &Hub{
		clients:         make(map[*Client]struct{}),
		buffer:          newReplayBuffer(),
		broadcast:       make(chan []byte, 1024),
		register:        make(chan *Client, 64),
		unregister:      make(chan *Client, 64),
		sticky:          make(map[string][]byte),
		stickyPending:   make(map[string]struct{}),
		stickyBroadcast: make(chan struct{}, 64),
	}
}

// Run is the hub's single event loop. It must be called in its own goroutine.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			for c := range h.clients {
				close(c.send)
			}
			return

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
				select {
				case c.send <- msg:
				default:
					// Client buffer full already (e.g. many sticky types).
				}
			}
			// Then replay buffered events. Sticky envelopes are never in
			// here (see above), so no filtering is needed.
			h.buffer.forEach(func(msg []byte) {
				select {
				case c.send <- msg:
				default:
					// Client buffer full; skip older replayed events rather than block.
				}
			})

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
			}

		case msg := <-h.broadcast:
			// Return value intentionally discarded: push already logs (at
			// debug level) when an event is rejected for exceeding
			// replayMaxEventSize, which is the only case where it returns
			// false. There's no additional metric consuming that signal
			// today, so explicitly discarding it here documents that the
			// omission is a choice, not an oversight.
			_ = h.buffer.push(msg) // store (skips oversized events; eviction handled inside)

			// Fan-out to all active clients (non-blocking; slow clients drop messages).
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					slog.Debug("ws: slow client, dropping message")
				}
			}

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
				for c := range h.clients {
					select {
					case c.send <- env:
					default:
						slog.Debug("ws: slow client, dropping sticky message")
					}
				}
			}
		}
	}
}

// Broadcast encodes a typed event and queues it for all clients + the replay buffer.
// Safe to call from any goroutine; never blocks (drops on full channel).
func (h *Hub) Broadcast(msgType string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Error("ws: marshal broadcast payload", "type", msgType, "err", err)
		return
	}
	env, err := json.Marshal(WSMessage{Type: msgType, Payload: raw})
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

// reply sends a response directly to one client (request/response pattern).
// Safe to call from any goroutine; recovers from send-to-closed-channel.
func (c *Client) reply(id, msgType string, payload any, errMsg string) {
	raw, _ := json.Marshal(payload)
	env, err := json.Marshal(WSMessage{ID: id, Type: msgType, Payload: raw, Error: errMsg})
	if err != nil {
		return
	}
	// Recover from panic if the client's send channel was closed concurrently.
	defer func() { recover() }() //nolint:errcheck
	select {
	case c.send <- env:
	default:
		slog.Warn("ws: client send buffer full, dropping reply")
	}
}

// writePump pumps messages from the send channel to the WebSocket connection.
// Exactly one writePump goroutine runs per client.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				slog.Debug("ws: write error", "err", err)
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
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
	env, err := json.Marshal(WSMessage{Type: msgType, Payload: raw})
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
