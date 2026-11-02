package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"runtime"
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
	require.NotContains(t, messagesText(pinnedInitial), "REPLAY_SECRET")
	require.NotContains(t, messagesText(pinnedInitial), "STICKY_SECRET")
	require.NotContains(t, messagesText(pinnedInitial), "MCP_REPLAY_SECRET")
	require.NotContains(t, messagesText(pinnedInitial), "MCP_STICKY_SECRET")
	require.Contains(t, messagesText(controlInitial), "REPLAY_SECRET")
	require.Contains(t, messagesText(controlInitial), "STICKY_SECRET")
	require.Contains(t, messagesText(controlInitial), "MCP_REPLAY_SECRET")
	require.Contains(t, messagesText(controlInitial), "MCP_STICKY_SECRET")
	for _, sentinel := range append(append([]string{}, classSentinels...), stickyClassSentinels...) {
		require.NotContains(t, messagesText(pinnedInitial), sentinel, "replay leaked a non-session event class to the pinned view")
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

	server.hub.Broadcast(EventSessionUpdated, map[string]string{"sessionID": foreign.ID, "sentinel": "LIVE_SECRET"})
	server.hub.Broadcast(EventSessionUpdated, map[string]string{"sessionID": own.ID, "sentinel": "OWN_VISIBLE"})
	server.hub.Broadcast(EventMCPState, map[string]any{"headers": map[string]string{"X-Sentinel": "MCP_LIVE_SECRET"}})
	broadcastClasses("LIVE_")
	server.hub.BroadcastSticky(EventSkills, map[string]string{"skill": "LIVE_STICKY_CLASS_SKILLS_SECRET"})
	server.hub.Broadcast(EventSessionCreated, map[string]string{"sessionID": own.ID, "id": "delivery-barrier"})
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
			require.NoError(t, readErr)
			var message WSMessage
			require.NoError(t, json.Unmarshal(raw, &message))
			frames = append(frames, message)
			if message.Type == EventSessionCreated {
				return messagesText(frames)
			}
		}
	}
	pinnedLive := readThroughBarrier(pinned)
	require.NotContains(t, pinnedLive, "LIVE_SECRET")
	require.NotContains(t, pinnedLive, "MCP_LIVE_SECRET")
	require.Contains(t, pinnedLive, "OWN_VISIBLE")
	controlLive := readThroughBarrier(control)
	require.Contains(t, controlLive, "LIVE_SECRET")
	require.Contains(t, controlLive, "OWN_VISIBLE")
	require.Contains(t, controlLive, "MCP_LIVE_SECRET")
	for _, sentinel := range append(append([]string{}, classSentinels...), "LIVE_STICKY_CLASS_SKILLS_SECRET") {
		live := sentinel
		if sentinel != "LIVE_STICKY_CLASS_SKILLS_SECRET" {
			live = "LIVE_" + sentinel
		}
		require.NotContains(t, pinnedLive, live, "live delivery leaked a non-session event class to the pinned view")
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
