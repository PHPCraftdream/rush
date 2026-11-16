package hub

import (
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
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
	hub         *Hub
	conn        *websocket.Conn
	send        chan []byte
	auth        *ClientAuth
	beforeWrite func([]byte)

	// trustedSettingsWrite marks a client that arrived through the
	// UI-authenticated listener transport. It is set ONLY in Server.handleWS
	// when s.uiAuth is true — i.e. after Auth.Middleware verified the
	// session cookie or token on the uiAuth listener. No wire input can set
	// or influence it: not a JSON payload field, not a header, not the
	// absence of a Principal, not the WebSocket transport itself. Policy
	// listeners (serve/API), synthetic Clients, and any non-UI connection
	// keep it false and inherit the guarded settings-write behavior.
	trustedSettingsWrite bool

	// sendMu orders close(send) (hub goroutine) against replies sent from
	// handler goroutines, so a reply racing shutdown is dropped instead of
	// panicking into a recover.
	sendMu     sync.RWMutex
	sendClosed bool

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
	workers     sync.WaitGroup
	ackMu       sync.Mutex
	ackWaiters  map[*byte]chan struct{}
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
			c.workers.Add(1)
			go func() {
				defer c.workers.Done()
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
func (c *Client) Dispatch(name, msgID string, fn func()) {
	select {
	case c.workQueue <- workItem{name: name, fn: fn}:
	default:
		slog.Warn("ws: work queue full, rejecting", "handler", name)
		c.Reply(msgID, wire.EventError, nil, "server busy, please retry")
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
func (c *Client) DispatchControl(name, msgID string, fn func()) {
	select {
	case c.controlSem <- struct{}{}:
		c.workers.Add(1)
		go func() {
			defer c.workers.Done()
			runRecovered(name, func() { <-c.controlSem }, fn)
		}()
	default:
		slog.Warn("ws: control queue full, rejecting", "handler", name)
		c.Reply(msgID, wire.EventError, nil, "server busy, please retry")
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

// reply sends a response directly to one client (request/response pattern).
// Safe to call from any goroutine; recovers from send-to-closed-channel.
func (c *Client) Reply(id, msgType string, payload any, errMsg string) {
	raw, _ := json.Marshal(payload)
	env, err := json.Marshal(wire.WSMessage{ID: id, Type: msgType, Payload: raw, Error: errMsg})
	if err != nil {
		return
	}
	if c.auth != nil && !c.auth.replyBypassesView(id, msgType) {
		meta := newLazyMeta(env)
		env = c.auth.filter(env, meta)
		if env == nil {
			return
		}
	}
	if !c.trySend(env) {
		slog.Warn("ws: client send buffer full or closed, dropping reply")
	}
}

// closeSend closes the outbound channel once; replies racing it are dropped.
func (c *Client) closeSend() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if !c.sendClosed {
		c.sendClosed = true
		close(c.send)
	}
}

// trySend queues env without blocking and reports whether it was queued.
func (c *Client) trySend(env []byte) bool {
	c.sendMu.RLock()
	defer c.sendMu.RUnlock()
	if c.sendClosed {
		return false
	}
	select {
	case c.send <- env:
		return true
	default:
		return false
	}
}

// replyAndWait queues a reply and waits until the write pump confirms it reached the socket.
func (c *Client) ReplyAndWait(id, msgType string, payload any, errMsg string, timeout time.Duration) bool {
	raw, _ := json.Marshal(payload)
	env, err := json.Marshal(wire.WSMessage{ID: id, Type: msgType, Payload: raw, Error: errMsg})
	if err != nil {
		return false
	}
	done := make(chan struct{})
	identity := &env[0]
	c.ackMu.Lock()
	if c.ackWaiters == nil {
		c.ackWaiters = make(map[*byte]chan struct{})
	}
	c.ackWaiters[identity] = done
	c.ackMu.Unlock()
	defer func() {
		c.ackMu.Lock()
		delete(c.ackWaiters, identity)
		c.ackMu.Unlock()
	}()
	if !c.trySend(env) {
		c.ackMu.Lock()
		delete(c.ackWaiters, identity)
		c.ackMu.Unlock()
		slog.Warn("ws: client send buffer full or closed, dropping reply")
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	case <-c.hub.stopped:
		return false
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
			if ok && c.beforeWrite != nil {
				c.beforeWrite(msg)
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				slog.Debug("ws: write error", "err", err)
				return
			}
			c.ackMu.Lock()
			if done := c.ackWaiters[&msg[0]]; done != nil {
				close(done)
				delete(c.ackWaiters, &msg[0])
			}
			c.ackMu.Unlock()
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// NewClient is the exported constructor seam (sprint 0 S4): package-server
// tests and fixtures constructed clients via the unexported newClient before
// the split (provenance: readpump_control_starvation_test.go dialTestWS and
// ~30 test files called newClient(hub, conn) at HEAD). Production callers are
// in-package (gate).
func NewClient(h *Hub, conn *websocket.Conn) *Client { return newClient(h, conn) }

// Hub returns the hub this client is attached to. Exported for root call sites
// that accessed c.hub before the split (handlers_config_persistence_test.go:163,
// handlers_config_lock_trusted_test.go:101 at HEAD); production code reaches
// the hub through gate.
func (c *Client) Hub() *Hub { return c.hub }

// SendChan returns the outbound channel in receive-only form. Exported for
// root call sites that read c.send directly before the split (~40 sites at
// HEAD, e.g. handlers_config_persistence_test.go:193 readTestReply).
func (c *Client) SendChan() <-chan []byte { return c.send }

// ResizeSendChan replaces the outbound channel with one of n slots. Exported
// for root call sites that sized c.send directly before the split (60+ sites,
// e.g. p460_model_scope_test.go:147 newTestClient). Never used by production
// code: production clients are built by newClient only.
func (c *Client) ResizeSendChan(n int) { c.send = make(chan []byte, n) }

// SetTrustedSettingsWrite marks the client as arriving through the
// UI-authenticated listener transport. Production caller: gate.handleWS
// (g.uiAuth path) only. Exported for root call sites that write the
// unexported field before the split (handlers_config_lock_trusted_test.go:448,
// peak_hours_authority_test.go:59 at HEAD); L5/L7 move those tests and remove
// the need.
func (c *Client) SetTrustedSettingsWrite(trusted bool) { c.trustedSettingsWrite = trusted }

// SetAuth installs the connection's command authorization. Production caller:
// gate.handleWS via NewClientAuth only. Exported for root call sites that set
// the unexported c.auth field before the split (handlers_config_lock_trusted_test.go:575,
// handlers_custom_roles_test.go:95 at HEAD); L5/L7 move those tests and remove
// the need.
func (c *Client) SetAuth(a *ClientAuth) { c.auth = a }

// HasAuth reports whether the client carries command authorization.
// Exported for its root consumer handlers_sessions.go:154 (c.auth != nil at
// HEAD), which stays in package server until L5.
func (c *Client) HasAuth() bool { return c.auth != nil }

// SettingsWriteAuthority maps this connection to its config write authority
// (§2.2 delta, exported rename of settingsWriteAuthority; its consumers are
// the config/custom-roles handlers, which stay in package server until L5;
// body verbatim from hub.go:169-174 at HEAD).
func (c *Client) SettingsWriteAuthority() config.SettingsWriteAuthority {
	if c != nil && c.trustedSettingsWrite {
		return config.SettingsAuthorityTrustedUI
	}
	return config.SettingsAuthorityGuarded
}

// WorkerPoolSize and MaxMessageSize re-export the production limits for root
// call sites that size bursts and frames until L5/L7 (workerPoolSize:
// readpump_control_starvation_test.go:242,255; maxMessageSize:
// api_matrix_limits_test.go:57).
const (
	WorkerPoolSize = workerPoolSize
	MaxMessageSize = maxMessageSize
)

// MaxBufferSize re-exports the replay ring capacity for root callers that
// size ring overruns until L7 (p547_sticky_update_notice_test.go).
const MaxBufferSize = maxBufferSize

// sendBuf slots for root tests that previously constructed
// &Client{send: ...} literals before the split; construction-time sizing is
// the only way to keep the field unexported. Production callers never use it
// (gate builds clients via newClient).
func NewDetachedClient(h *Hub, sendBuf int) *Client {
	return &Client{hub: h, send: make(chan []byte, sendBuf)}
}
