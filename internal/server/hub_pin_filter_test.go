package server

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestEventSessionClassificationExhaustive(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	protocol, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(file), "protocol.go"), nil, parser.ParseComments)
	require.NoError(t, err)
	values := map[string]string{}
	for _, decl := range protocol.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range valueSpec.Names {
				if !strings.HasPrefix(name.Name, "Event") {
					continue
				}
				exprIndex := i
				if exprIndex >= len(valueSpec.Values) {
					exprIndex = len(valueSpec.Values) - 1
				}
				require.GreaterOrEqual(t, exprIndex, 0, "event %s must have an explicit string value", name.Name)
				literal, ok := valueSpec.Values[exprIndex].(*ast.BasicLit)
				require.True(t, ok, "event %s must have a string literal value", name.Name)
				value, err := strconv.Unquote(literal.Value)
				require.NoError(t, err)
				values[value] = name.Name
			}
		}
	}
	for value, name := range values {
		class, ok := eventClasses[value]
		require.True(t, ok, "event constant %s=%q lacks explicit classification", name, value)
		require.NotEqual(t, eventUnknown, class, "event constant %s=%q has unknown classification", name, value)
	}
	for value := range eventClasses {
		require.Contains(t, values, value, "classification %q has no Event* constant", value)
	}
}

func broadcastMarkerAfterDelivery(h *Hub, payload any) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	done := make(chan struct{})
	select {
	case h.deliveryBarrier <- done:
	case <-h.stopped:
		return errors.New("hub stopped before delivery barrier was accepted")
	case <-timer.C:
		return errors.New("timed out submitting hub delivery barrier")
	}
	select {
	case <-h.stopped:
		return errors.New("hub stopped before delivery barrier completed")
	default:
	}
	select {
	case <-done:
	case <-h.stopped:
		return errors.New("hub stopped before delivery barrier completed")
	case <-timer.C:
		return errors.New("timed out waiting for hub delivery barrier")
	}
	select {
	case <-h.stopped:
		return errors.New("hub stopped before marker broadcast")
	default:
	}
	h.Broadcast(EventSessionCreated, payload)
	return nil
}

func isPinnedViewDeliveryMarker(message WSMessage, sessionID string) bool {
	if message.Type != EventSessionCreated {
		return false
	}
	var marker struct {
		SessionID string `json:"sessionID"`
		ID        string `json:"id"`
	}
	if json.Unmarshal(message.Payload, &marker) != nil {
		return false
	}
	return marker.SessionID == sessionID && marker.ID == "delivery-barrier"
}

func logHubFrameDiagnostics(t *testing.T, frames, sentinel string) {
	t.Helper()
	present := strings.Contains(frames, sentinel)
	const maxDiagnosticBytes = 64 * 1024
	if len(frames) > maxDiagnosticBytes {
		t.Logf("hub frame diagnostics: bytes=%d summary=omitted sentinel_present=%t", len(frames), present)
		return
	}
	var messages []WSMessage
	if err := json.Unmarshal([]byte(frames), &messages); err != nil {
		t.Logf("hub frame diagnostics: parse_ok=false sentinel_present=%t", present)
		return
	}
	counts := make(map[string]int)
	for _, message := range messages {
		typ := message.Type
		if len(typ) > 48 {
			typ = typ[:48]
		}
		counts[typ]++
	}
	types := make([]string, 0, len(counts))
	for typ := range counts {
		types = append(types, typ)
	}
	sort.Strings(types)
	if len(types) > 16 {
		types = types[:16]
	}
	boundedCounts := make(map[string]int, len(types))
	for _, typ := range types {
		boundedCounts[typ] = counts[typ]
	}
	t.Logf("hub frame diagnostics: frame_count=%d type_counts=%v sentinel_present=%t", len(messages), boundedCounts, present)
}

func logHubFrameDiagnosticsOnUnexpected(t *testing.T, frames, sentinel string, wantPresent bool) {
	t.Helper()
	if strings.Contains(frames, sentinel) != wantPresent {
		logHubFrameDiagnostics(t, frames, sentinel)
	}
}

func TestPinnedViewDeliveryMarkerMatchesExactMarker(t *testing.T) {
	valid := WSMessage{Type: EventSessionCreated, Payload: json.RawMessage(`{"sessionID":"own","id":"delivery-barrier"}`)}
	for _, tc := range []struct {
		name      string
		message   WSMessage
		sessionID string
		want      bool
	}{
		{name: "exact marker", message: valid, sessionID: "own", want: true},
		{name: "foreign session", message: valid, sessionID: "foreign"},
		{name: "other marker id", message: WSMessage{Type: EventSessionCreated, Payload: json.RawMessage(`{"sessionID":"own","id":"other"}`)}, sessionID: "own"},
		{name: "unrelated session created", message: WSMessage{Type: EventSessionCreated, Payload: json.RawMessage(`{"sessionID":"own","id":"ordinary"}`)}, sessionID: "own"},
		{name: "malformed payload", message: WSMessage{Type: EventSessionCreated, Payload: json.RawMessage(`{`)}, sessionID: "own"},
		{name: "other event type", message: WSMessage{Type: EventSessionUpdated, Payload: valid.Payload}, sessionID: "own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isPinnedViewDeliveryMarker(tc.message, tc.sessionID))
		})
	}
}

func TestHubMarkerWaitsForPriorDeliveryBeforeBroadcast(t *testing.T) {
	h := newHub()
	t.Cleanup(func() { close(h.stopped) })
	result := make(chan error, 1)
	go func() {
		result <- broadcastMarkerAfterDelivery(h, map[string]string{"sessionID": "own", "id": "delivery-barrier"})
	}()

	var ack chan struct{}
	ackClosed := false
	defer func() {
		if ack != nil && !ackClosed {
			close(ack)
		}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case ack = <-h.deliveryBarrier:
	case err := <-result:
		t.Fatalf("marker helper returned before its barrier was accepted: %v", err)
	case <-timer.C:
		t.Fatal("marker helper did not request a delivery barrier")
	}
	select {
	case raw := <-h.broadcast:
		t.Fatalf("marker was queued before the barrier acknowledgement: type=%q", envelopeType(raw))
	default:
	}

	close(ack)
	ackClosed = true
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-timer.C:
		t.Fatal("marker helper did not finish after barrier acknowledgement")
	}
	select {
	case raw := <-h.broadcast:
		var message WSMessage
		require.NoError(t, json.Unmarshal(raw, &message))
		require.True(t, isPinnedViewDeliveryMarker(message, "own"), "queued event is not the exact delivery marker")
	case <-timer.C:
		t.Fatal("marker helper did not queue the marker after barrier acknowledgement")
	}
}

func TestHub_PinnedViewNeverReceivesForeignSessionEvents(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	workingDir := t.TempDir()
	app := newAttachmentsTestApp(t, workingDir, t.TempDir())
	own, err := app.Sessions.Create(t.Context(), "pinned session")
	require.NoError(t, err)
	foreign, err := app.Sessions.Create(t.Context(), "foreign session")
	require.NoError(t, err)

	store := config.GlobalAPITokenStore()
	pinnedToken, pinnedRecord, err := store.CreateAPITokenWithBinding("pinned websocket", []string{"read"}, "ask", nil, config.APITokenBinding{
		Kind: "session", Pin: &config.APITokenPinBinding{Session: own.ID, Workspace: workingDir},
	})
	require.NoError(t, err)
	controlToken, _, err := store.CreateAPIToken("unpinned websocket", []string{"admin"}, "ask", nil)
	require.NoError(t, err)
	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	policy := ListenerPolicy{Address: "127.0.0.1:0", Workspace: workingDir, Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS}, AllowAdmin: true, Limits: ListenerLimits{MaxConnsPerToken: 10, RPS: 100000, Burst: 100000}}
	api, err := NewServeAPI(policy, app, store, nil)
	require.NoError(t, err)
	api.Configure(&policy)
	server := NewWithPolicy(app, policy, nil)
	base := startPolicyServer(t, server)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, api.Stop(ctx))
	})
	fixture := &scopeFixture{
		endpoint: "ws" + strings.TrimPrefix(base, "http"),
		server:   server,
		tokens:   map[string]string{"pinned": pinnedToken, "control": controlToken},
	}

	// One event of every non-session class, with a sentinel: none may reach a
	// pinned view through any path, all must reach the unpinned control.
	classSentinels := []string{"CLASS_LOGS_SECRET", "CLASS_SKILLS_SECRET", "CLASS_UNKNOWN_SECRET", "CLASS_NOID_SECRET"}
	broadcastClasses := func(prefix string) {
		server.hub.Broadcast(EventLogs, map[string]string{"line": prefix + "CLASS_LOGS_SECRET"})
		server.hub.Broadcast(EventSkills, map[string]string{"skill": prefix + "CLASS_SKILLS_SECRET"})
		server.hub.Broadcast("mystery_event", map[string]string{"x": prefix + "CLASS_UNKNOWN_SECRET"})
		server.hub.Broadcast(EventSessionUpdated, map[string]string{"note": prefix + "CLASS_NOID_SECRET"}) // session class, no session id
	}
	stickyClassSentinels := []string{"STICKY_CLASS_SKILLS_SECRET", "STICKY_CLASS_UNKNOWN_SECRET"}
	server.hub.BroadcastSticky(EventSkills, map[string]string{"skill": "STICKY_CLASS_SKILLS_SECRET"})
	server.hub.BroadcastSticky("mystery_event", map[string]string{"x": "STICKY_CLASS_UNKNOWN_SECRET"})
	broadcastClasses("")
	// Sticky and buffered history are present before either production client registers.
	server.hub.BroadcastSticky(EventSessionUpdated, map[string]string{"sessionID": foreign.ID, "sentinel": "STICKY_SECRET"})
	server.hub.BroadcastSticky(EventMCPState, map[string]any{"headers": map[string]string{"X-Sentinel": "MCP_STICKY_SECRET"}})
	server.hub.Broadcast(EventSessionUpdated, map[string]string{"sessionID": foreign.ID, "sentinel": "REPLAY_SECRET"})
	server.hub.Broadcast(EventMCPState, map[string]any{"headers": map[string]string{"X-Sentinel": "MCP_REPLAY_SECRET"}})
	select {
	case <-server.hub.WaitDelivery():
	case <-time.After(5 * time.Second):
		t.Fatal("hub did not store replay event")
	}
	pinned := fixture.dial(t, "pinned")
	control := fixture.dial(t, "control")
	readUntilReply := func(conn *websocket.Conn, id string) []WSMessage {
		t.Helper()
		sendCommand(t, conn, WSMessage{ID: id, Type: CmdListSessions})
		var messages []WSMessage
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, raw, readErr := conn.ReadMessage()
			require.NoError(t, readErr)
			var message WSMessage
			require.NoError(t, json.Unmarshal(raw, &message))
			messages = append(messages, message)
			if message.ID == id {
				return messages
			}
		}
	}
	pinnedInitial := readUntilReply(pinned, "pinned-list")
	controlInitial := readUntilReply(control, "control-list")
	messagesText := func(messages []WSMessage) string {
		raw, marshalErr := json.Marshal(messages)
		require.NoError(t, marshalErr)
		return string(raw)
	}
	logHubFrameDiagnosticsOnUnexpected(t, messagesText(pinnedInitial), "REPLAY_SECRET", false)
	require.NotContains(t, messagesText(pinnedInitial), "REPLAY_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, messagesText(pinnedInitial), "STICKY_SECRET", false)
	require.NotContains(t, messagesText(pinnedInitial), "STICKY_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, messagesText(pinnedInitial), "MCP_REPLAY_SECRET", false)
	require.NotContains(t, messagesText(pinnedInitial), "MCP_REPLAY_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, messagesText(pinnedInitial), "MCP_STICKY_SECRET", false)
	require.NotContains(t, messagesText(pinnedInitial), "MCP_STICKY_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, messagesText(controlInitial), "REPLAY_SECRET", true)
	require.Contains(t, messagesText(controlInitial), "REPLAY_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, messagesText(controlInitial), "STICKY_SECRET", true)
	require.Contains(t, messagesText(controlInitial), "STICKY_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, messagesText(controlInitial), "MCP_REPLAY_SECRET", true)
	require.Contains(t, messagesText(controlInitial), "MCP_REPLAY_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, messagesText(controlInitial), "MCP_STICKY_SECRET", true)
	require.Contains(t, messagesText(controlInitial), "MCP_STICKY_SECRET")
	for _, sentinel := range append(append([]string{}, classSentinels...), stickyClassSentinels...) {
		logHubFrameDiagnosticsOnUnexpected(t, messagesText(pinnedInitial), sentinel, false)
		require.NotContains(t, messagesText(pinnedInitial), sentinel, "replay leaked a non-session event class to the pinned view")
		logHubFrameDiagnosticsOnUnexpected(t, messagesText(controlInitial), sentinel, true)
		require.Contains(t, messagesText(controlInitial), sentinel, "the unpinned control must receive %s", sentinel)
	}
	var pinnedSessions []struct {
		ID string `json:"id"`
	}
	var controlSessions []struct {
		ID string `json:"id"`
	}
	for _, message := range pinnedInitial {
		if message.Type == EventSessionsList {
			require.NoError(t, json.Unmarshal(message.Payload, &pinnedSessions))
		}
	}
	for _, message := range controlInitial {
		if message.Type == EventSessionsList {
			require.NoError(t, json.Unmarshal(message.Payload, &controlSessions))
		}
	}
	require.Equal(t, []string{own.ID}, sessionIDs(pinnedSessions))
	require.ElementsMatch(t, []string{own.ID, foreign.ID}, sessionIDs(controlSessions))

	server.hub.BroadcastSticky(EventSkills, map[string]string{"skill": "LIVE_STICKY_CLASS_SKILLS_SECRET"})
	select {
	case <-server.hub.WaitDelivery():
	case <-time.After(5 * time.Second):
		t.Fatal("hub live sticky delivery timeout")
	}
	server.hub.Broadcast(EventSessionUpdated, map[string]string{"sessionID": foreign.ID, "sentinel": "LIVE_SECRET"})
	server.hub.Broadcast(EventSessionUpdated, map[string]string{"sessionID": own.ID, "sentinel": "OWN_VISIBLE"})
	server.hub.Broadcast(EventMCPState, map[string]any{"headers": map[string]string{"X-Sentinel": "MCP_LIVE_SECRET"}})
	broadcastClasses("LIVE_")
	require.NoError(t, broadcastMarkerAfterDelivery(server.hub, map[string]string{"sessionID": own.ID, "id": "delivery-barrier"}))
	select {
	case <-server.hub.WaitDelivery():
	case <-time.After(5 * time.Second):
		t.Fatal("hub live delivery timeout")
	}
	readThroughBarrier := func(conn *websocket.Conn) string {
		t.Helper()
		var frames []WSMessage
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				logHubFrameDiagnostics(t, messagesText(frames), "delivery-barrier")
				require.NoError(t, readErr)
			}
			var message WSMessage
			require.NoError(t, json.Unmarshal(raw, &message))
			frames = append(frames, message)
			if isPinnedViewDeliveryMarker(message, own.ID) {
				return messagesText(frames)
			}
		}
	}
	pinnedLive := readThroughBarrier(pinned)
	logHubFrameDiagnosticsOnUnexpected(t, pinnedLive, "LIVE_SECRET", false)
	require.NotContains(t, pinnedLive, "LIVE_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, pinnedLive, "MCP_LIVE_SECRET", false)
	require.NotContains(t, pinnedLive, "MCP_LIVE_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, pinnedLive, "OWN_VISIBLE", true)
	require.Contains(t, pinnedLive, "OWN_VISIBLE")
	controlLive := readThroughBarrier(control)
	logHubFrameDiagnosticsOnUnexpected(t, controlLive, "LIVE_SECRET", true)
	require.Contains(t, controlLive, "LIVE_SECRET")
	logHubFrameDiagnosticsOnUnexpected(t, controlLive, "OWN_VISIBLE", true)
	require.Contains(t, controlLive, "OWN_VISIBLE")
	logHubFrameDiagnosticsOnUnexpected(t, controlLive, "MCP_LIVE_SECRET", true)
	require.Contains(t, controlLive, "MCP_LIVE_SECRET")
	for _, sentinel := range append(append([]string{}, classSentinels...), "LIVE_STICKY_CLASS_SKILLS_SECRET") {
		live := sentinel
		if sentinel != "LIVE_STICKY_CLASS_SKILLS_SECRET" {
			live = "LIVE_" + sentinel
		}
		logHubFrameDiagnosticsOnUnexpected(t, pinnedLive, live, false)
		require.NotContains(t, pinnedLive, live, "live delivery leaked a non-session event class to the pinned view")
		logHubFrameDiagnosticsOnUnexpected(t, controlLive, live, true)
		require.Contains(t, controlLive, live, "the unpinned control must receive %s", live)
	}

	// list_sessions is a real direct reply: its unpinned control contains both
	// sessions, while the bound principal's reply contains only the pin.
	require.NotEmpty(t, pinnedRecord.ID)
	for _, token := range []string{pinnedToken, controlToken} {
		secret := token[strings.IndexByte(token, '.')+1:]
		require.NotContains(t, logs.String(), token)
		require.NotContains(t, logs.String(), secret)
	}
}

func sessionIDs(sessions []struct {
	ID string `json:"id"`
},
) []string {
	ids := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.ID
	}
	return ids
}

func TestClientReplyAppliesPinnedViewExceptCorrelatedErrors(t *testing.T) {
	client := &Client{send: make(chan []byte, 8), auth: newClientAuth(Principal{PinnedSession: "own", Scopes: []string{"read"}}, CommandPolicy{})}
	drain := func() []string {
		var out []string
		for {
			select {
			case msg := <-client.send:
				out = append(out, string(msg))
			default:
				return out
			}
		}
	}

	client.reply("a", EventSessionUpdated, map[string]string{"sessionID": "foreign"}, "")
	client.reply("b", EventSessionsList, []map[string]string{{"id": "own"}, {"id": "foreign"}}, "")
	client.reply("c", EventLogs, map[string]string{"line": "SECRET"}, "")
	require.Empty(t, drain(), "replies about foreign sessions or non-session classes must not reach a pinned client")

	client.reply("d", EventSessionUpdated, map[string]string{"sessionID": "own"}, "")
	require.Len(t, drain(), 1, "a reply about the pinned session is delivered")

	client.reply("e", EventError, nil, "not found")
	client.reply("f", EventResponse, map[string]string{"ok": "true"}, "")
	require.Len(t, drain(), 2, "correlated error and response replies bypass the view")

	client.reply("", EventError, nil, "uncorrelated")
	require.Empty(t, drain(), "an error without a request id is an event and stays filtered")
}

func TestPinnedViewNeverAllowsInvalidMetadata(t *testing.T) {
	view := newPinnedView("own")
	for _, meta := range []eventMetadata{
		{valid: false, class: eventSafeGlobal, msgType: EventUpdateAvailable},
		{valid: false, class: eventSession, sessionIDs: []string{"own"}},
	} {
		require.False(t, view.allows(meta), "%+v", meta)
	}
	require.True(t, view.allows(eventMetadata{valid: true, class: eventSession, sessionIDs: []string{"own"}}))
	require.True(t, newPinnedView("").allows(eventMetadata{}), "an unpinned view allows everything")
}

// WaitDelivery is the barrier tests read through; a sticky fan-out still
// queued behind it would arrive after the frame the test stops at. Many
// clients keep that late fan-out busy long enough for the check to see it.
func TestHubDeliveryBarrierIncludesPendingStickyFanOut(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := newHub()
		clients := make([]*Client, 3000)
		for j := range clients {
			clients[j] = &Client{hub: h, send: make(chan []byte, 1)}
			h.clients[clients[j]] = struct{}{}
		}
		h.BroadcastSticky(EventSkills, map[string]string{"skill": "S"})
		barrier := make(chan (<-chan struct{}), 1)
		go func() { barrier <- h.WaitDelivery() }() // parks until Run starts, so both inputs are ready at once
		ctx, cancel := context.WithCancel(t.Context())
		go h.Run(ctx)
		select {
		case done := <-barrier:
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("barrier did not complete")
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("barrier was not accepted")
		}
		for j, client := range clients {
			select {
			case msg := <-client.send:
				require.Contains(t, string(msg), `"skill":"S"`)
			default:
				cancel()
				t.Fatalf("iteration %d: client %d had no sticky event when the barrier completed", i, j)
			}
		}
		cancel()
	}
}
