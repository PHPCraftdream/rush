package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestThemeAndKeepAliveHandlersMapPersistedValues(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	client := newTestClient()
	p, err := json.Marshal(SetThemePayload{Theme: "dark"})
	require.NoError(t, err)
	handleSetTheme(a, client, WSMessage{ID: "theme", Payload: p})
	event := readConfigEvent(t, client.hub)
	require.Equal(t, "dark", event.Theme)
	p, err = json.Marshal(SetKeepAlivePayload{Enabled: false})
	require.NoError(t, err)
	handleSetKeepAlive(a, client, WSMessage{ID: "keep", Payload: p})
	event = readConfigEvent(t, client.hub)
	require.False(t, event.KeepAliveEnabled)
}

func readConfigEvent(t *testing.T, hub *Hub) ConfigWire {
	t.Helper()
	// EventConfig goes through the hub's coalescing slot, not hub.broadcast.
	hub.configMu.Lock()
	raw := hub.configPending
	hub.configPending = nil
	hub.configMu.Unlock()
	select {
	case <-hub.configBroadcast:
	default:
	}
	require.NotNil(t, raw, "handler did not broadcast config")
	var envelope WSMessage
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.Equal(t, EventConfig, envelope.Type)
	var wire ConfigWire
	require.NoError(t, json.Unmarshal(envelope.Payload, &wire))
	return wire
}
