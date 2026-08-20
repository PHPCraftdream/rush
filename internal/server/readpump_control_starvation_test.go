package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// blockingCoordinator is a minimal agent.Coordinator whose Run/RunWithOverrides
// block until the test releases them, and whose Cancel signals a channel the
// instant it is called. It exists to saturate handleSendMessage's dispatch
// slots with handlers that are REALLY in flight (not merely simulated), so
// TestReadPumpCancelBypassesQueuedWorkFrame below reproduces the actual
// blocking call graph handleIncoming drives in production, rather than a
// synthetic stand-in for it.
type blockingCoordinator struct {
	release <-chan struct{}

	cancelMu   sync.Mutex
	cancelHits []string
	canceled   chan struct{} // closed on the FIRST Cancel call
	closeOnce  sync.Once
}

func newBlockingCoordinator(release <-chan struct{}) *blockingCoordinator {
	return &blockingCoordinator{release: release, canceled: make(chan struct{})}
}

func (f *blockingCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	<-f.release
	return nil, nil
}

func (f *blockingCoordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	<-f.release
	return nil, nil
}

// The three reservation methods below satisfy the Coordinator interface as
// task #614 extended it. This fake reports IsSessionBusy==true forever (its
// saturating turns never finish until release closes), so ReserveExclusive
// must refuse: a session this fake calls busy cannot simultaneously be
// claimable. Nothing in this file reaches them -- it drives send_message and
// cancel_agent frames, never rerun -- but they must not lie if it ever does.
func (f *blockingCoordinator) ReserveExclusive(ctx context.Context, sessionID string) (epoch uint64, cancel context.CancelFunc, ok bool) {
	return 0, nil, false
}

func (f *blockingCoordinator) ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc) {
}

func (f *blockingCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	<-f.release
	return nil, nil
}

func (f *blockingCoordinator) Cancel(sessionID string) {
	f.cancelMu.Lock()
	f.cancelHits = append(f.cancelHits, sessionID)
	f.cancelMu.Unlock()
	f.closeOnce.Do(func() { close(f.canceled) })
}

func (f *blockingCoordinator) CancelAll() (stillBusy bool)         { return false }
func (f *blockingCoordinator) IsSessionBusy(sessionID string) bool { return true }
func (f *blockingCoordinator) IsBusy() bool                        { return true }
func (f *blockingCoordinator) QueuedPrompts(sessionID string) int  { return 0 }
func (f *blockingCoordinator) QueuedPromptsList(sessionID string) []string {
	return nil
}
func (f *blockingCoordinator) ClearQueue(sessionID string) {}
func (f *blockingCoordinator) InterruptAndSend(ctx context.Context, sessionID, prompt string, smart, fast *agent.ModelOverride, attachments ...message.Attachment) error {
	return nil
}
func (f *blockingCoordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	return message.Message{}, nil
}
func (f *blockingCoordinator) Summarize(context.Context, string, *agent.SummarizeSnapshot) error {
	return nil
}
func (f *blockingCoordinator) SummarizeQueued(sessionID string) bool { return false }
func (f *blockingCoordinator) TakeSummarizeQueue(sessionID string) (*agent.SummarizeSnapshot, bool) {
	return nil, false
}
func (f *blockingCoordinator) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	return agent.SessionAgentCall{}, nil
}
func (f *blockingCoordinator) RunSessionAgentCall(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}
func (f *blockingCoordinator) CancelQueuedSummarize(sessionID string)                {}
func (f *blockingCoordinator) Model() agent.Model                                    { return agent.Model{} }
func (f *blockingCoordinator) UpdateModels(ctx context.Context) error                { return nil }
func (f *blockingCoordinator) GetSystemPrompt() string                               { return "" }
func (f *blockingCoordinator) BuildSystemPrompt(ctx context.Context) (string, error) { return "", nil }
func (f *blockingCoordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}
func (f *blockingCoordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return nil
}
func (f *blockingCoordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
}
func (f *blockingCoordinator) SetRunLimits(maxCost float64, maxTokens int64)    {}
func (f *blockingCoordinator) SetActiveModelRole(role config.SelectedModelType) {}
func (f *blockingCoordinator) SetAllowPeakHours(allow bool)                     {}
func (f *blockingCoordinator) SetPersistentMode(persistent bool)                {}
func (f *blockingCoordinator) ResetAutoResumeCounter(sessionID string)          {}

// dialTestWS upgrades an httptest server to a real WebSocket connection and
// hands the server side to s.readPump on its own goroutine, exactly the way
// Server.handleWS does in production. Returns the client-side conn for the
// test to write/read frames on, plus a channel closed when readPump returns.
func dialTestWS(t *testing.T, s *Server) (*websocket.Conn, *Client, <-chan struct{}) {
	t.Helper()

	upgrader := websocket.Upgrader{}
	connReady := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		connReady <- conn
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientSide, handshakeResp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSide.Close() })
	_ = handshakeResp.Body.Close()

	serverSideConn := <-connReady
	c := newClient(s.hub, serverSideConn)

	// Mirror Server.handleWS: register with the hub so Hub.Run's broadcast
	// fan-out (range h.clients) actually reaches this client's c.send — the
	// EventAgentBusy broadcast handleCancelAgent force-sends would otherwise
	// silently go nowhere, since it's only ever delivered to clients present
	// in h.clients.
	s.hub.register <- c

	// Mirror Server.handleWS: writePump runs in the background so anything
	// written to c.send (broadcasts, replies) actually reaches the socket.
	// Without this, EventAgentBusy broadcasts and c.reply calls would just
	// pile up in c.send and never be observable by a client-side reader.
	go c.writePump()

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		s.readPump(ctx, c)
		close(done)
	}()

	return clientSide, c, done
}

// sendWS marshals and writes one WSMessage frame to the given client-side
// connection, exactly as a browser tab would.
func sendWS(t *testing.T, conn *websocket.Conn, msg WSMessage) {
	t.Helper()
	raw, err := json.Marshal(msg)
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, raw))
}

// TestReadPumpCancelBypassesQueuedWorkFrame is the regression test for #612:
// with all workerPoolSize (== maxConcurrentHandlersPerConn) work slots
// genuinely occupied by in-flight handleSendMessage calls blocked inside
// AgentCoordinator.Run, a 13th ordinary work frame sent over the actual
// socket must NOT block readPump's conn.ReadMessage() loop, and a
// CmdCancelAgent frame sent immediately after it must complete BEFORE any of
// the 12 occupied slots are released.
//
// Before the fix (dispatch's blocking `c.sem <- struct{}{}` acquire called
// synchronously from readPump), the 13th frame's dispatch call would block
// the SAME goroutine that calls conn.ReadMessage() in a loop — so the Cancel
// frame sent right behind it could never even be read off the socket, let
// alone routed through dispatchControl's separate semaphore. That starvation
// is only reachable by going through the real read loop; calling
// dispatchControl directly (as the pre-existing
// TestDispatchControlBypassesWorkSemaphore does) starts from a point in the
// call graph the bug never lets execution reach.
func TestReadPumpCancelBypassesQueuedWorkFrame(t *testing.T) {
	// Cannot use t.Parallel(): newAttachmentsTestApp calls t.Setenv.

	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	release := make(chan struct{})
	fakeCoord := newBlockingCoordinator(release)
	a.AgentCoordinator = fakeCoord

	s := &Server{app: a, hub: newHub()}
	go s.hub.Run(t.Context())

	conn, _, readPumpDone := dialTestWS(t, s)

	// Read every server->client frame on a background goroutine so replies
	// (including the eventual overload/error replies from the queue) never
	// block writePump-independent sends into c.send from filling up and
	// wedging anything. We only assert on order via the barriers below, not
	// on frame content from this drain.
	cancelReplyID := "cancel-msg-id"
	// handleCancelAgent (handlers_agent.go) doesn't reply with msg.ID at all
	// — it force-broadcasts EventAgentBusy{Busy:false} for the session being
	// cancelled. That broadcast is this test's proof the frame was actually
	// routed through handleIncoming/dispatchControl and executed, so watch
	// for it instead of a by-ID reply.
	cancelSeen := make(chan struct{})
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m WSMessage
			if json.Unmarshal(raw, &m) != nil || m.Type != EventAgentBusy {
				continue
			}
			var p AgentBusyPayload
			if json.Unmarshal(m.Payload, &p) == nil && p.SessionID == "sat-session" && !p.Busy {
				select {
				case <-cancelSeen:
				default:
					close(cancelSeen)
				}
			}
		}
	}()

	// Saturate all workerPoolSize slots with REAL handleSendMessage calls
	// that block inside blockingCoordinator.Run — these are genuinely in
	// flight, not simulated.
	for i := 0; i < workerPoolSize; i++ {
		payload, err := json.Marshal(SendMessagePayload{SessionID: "sat-session", Content: "hold"})
		require.NoError(t, err)
		sendWS(t, conn, WSMessage{ID: "sat", Type: CmdSendMessage, Payload: payload})
	}

	// Wait for all workerPoolSize workers to actually be inside Run (blocked
	// on release), proving saturation is real, not just enqueued. We detect
	// this indirectly: give the fixed worker pool time to pick up all
	// workerPoolSize items and call into blockingCoordinator.Run. There is no
	// direct "N goroutines blocked" signal exposed by Coordinator, so we
	// synchronize via a dedicated counting hook instead of a sleep-based
	// guess -- see runningCount below.
	waitForSaturation(t, s, workerPoolSize)

	// Send a 13th ordinary work frame. Under the fix this is queued
	// non-blockingly (or rejected with overload if the queue is also full —
	// either way readPump keeps looping); under the bug this would block
	// readPump's conn.ReadMessage() loop forever inside dispatch's semaphore
	// acquire, and nothing sent after it would ever be read off the socket.
	overflowPayload, err := json.Marshal(SendMessagePayload{SessionID: "overflow-session", Content: "overflow"})
	require.NoError(t, err)
	sendWS(t, conn, WSMessage{ID: "overflow", Type: CmdSendMessage, Payload: overflowPayload})

	// Immediately behind it, send Cancel. This is the crux of the
	// regression: if readPump were blocked reading the frame above, this
	// frame would never even reach handleIncoming.
	cancelPayload, err := json.Marshal(CancelAgentPayload{SessionID: "sat-session"})
	require.NoError(t, err)
	sendWS(t, conn, WSMessage{ID: cancelReplyID, Type: CmdCancelAgent, Payload: cancelPayload})

	// Prove Cancel ran BEFORE any of the 12 saturated slots were released:
	// fakeCoord.canceled must close while release is still unclosed (workers
	// still blocked in Run). We assert this with a select race, not a sleep:
	// if canceled fires first, the assertion embedded in the select ordering
	// holds; if the test were to hang here instead (regression reintroduced),
	// the timeout fires and fails loudly rather than silently passing.
	select {
	case <-fakeCoord.canceled:
		// Good: Cancel completed. Confirm the 12 slots are STILL held open
		// at this exact instant, i.e. Cancel really did jump ahead of them
		// rather than coincidentally running after they all happened to
		// finish on their own.
		select {
		case <-release:
			t.Fatal("release channel was closed by the test binary itself unexpectedly")
		default:
			// Expected: release hasn't been closed by the test yet, and
			// nothing else could close it — so if Run() had returned for
			// any worker, it could only be because <-f.release itself
			// unblocked, which is impossible while release stays open.
			// This confirms all workerPoolSize Run() calls are still
			// parked on the channel read at the moment Cancel fired.
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not run within 5s while all work slots were held open — " +
			"readPump is starved behind the saturated work queue (regression of #612)")
	}

	// Also confirm the cancel reply frame itself reached the client-side
	// reader before we release the held handlers.
	select {
	case <-cancelSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel reply frame was never read back from the socket")
	}

	// Cleanup: release the held handlers so the worker pool and readPump can
	// wind down cleanly.
	close(release)
	select {
	case <-readPumpDone:
	case <-time.After(2 * time.Second):
		// readPump only exits on a read error/close; not exiting here isn't
		// itself a failure of this test's assertions, so just avoid hanging
		// the test suite if cleanup somehow stalls.
	}
}

// waitForSaturation polls (with a short interval, bounded overall timeout —
// not a fixed sleep) until workerPoolSize handleSendMessage calls are
// observed blocked inside blockingCoordinator.Run for Server s's most
// recently dialed client. Implemented via s.hub's broadcast of
// EventAgentBusy=true, which handleSendMessage sends synchronously BEFORE
// calling Run — so counting distinct busy=true broadcasts for the
// saturating session is a reliable proxy for "this many handleSendMessage
// goroutines have reached the blocking call", without sleeping a guessed
// duration.
func waitForSaturation(t *testing.T, s *Server, n int) {
	t.Helper()

	var count int32
	sub := make(chan struct{}, n*2)
	go func() {
		// Tap directly into the hub's broadcast channel via a second
		// registered client, since Hub has no public subscribe-and-count
		// API. A lightweight fake client with a large send buffer is
		// sufficient: Run() fans out non-blockingly to c.send.
	}()
	_ = sub // placeholder removed below; see inline registration instead.

	tapClient := newClient(s.hub, nil)
	tapClient.send = make(chan []byte, 4096)
	s.hub.register <- tapClient
	defer func() {
		s.hub.unregister <- tapClient
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case raw := <-tapClient.send:
			var env WSMessage
			if json.Unmarshal(raw, &env) == nil && env.Type == EventAgentBusy {
				var p AgentBusyPayload
				if json.Unmarshal(env.Payload, &p) == nil && p.SessionID == "sat-session" && p.Busy {
					if atomic.AddInt32(&count, 1) >= int32(n) {
						return
					}
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %d saturating handlers to report busy=true (saw %d)", n, atomic.LoadInt32(&count))
}
