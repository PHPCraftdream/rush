package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestPolicySettingsAuthorityHeadersCannotBypassLock(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	f := newServeWSScopeFixture(t, true, map[string][]string{"admin": {"admin"}})
	store := f.app.Store()
	config.SetProcessPassword("")
	t.Cleanup(func() { config.SetProcessPassword("") })
	require.NoError(t, store.LockSettings(config.ScopeGlobal, "locked"))
	before, err := os.ReadFile(config.GlobalConfigData())
	require.NoError(t, err)
	phase := "policy handshake"
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("settings authority transport: listener=policy phase=%s", phase)
		}
	})
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	headers := http.Header{
		"Authorization":             {"Bearer " + f.tokens["admin"]},
		"X-Rush-Settings-Authority": {"trusted_ui"},
		"X-Settings-Source":         {"web"},
	}
	conn, response, err := dialer.Dial(f.endpoint+"/ws", headers)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if conn != nil {
		defer conn.Close()
	}
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	var hello WSMessage
	require.NoError(t, conn.ReadJSON(&hello))
	require.Equal(t, EventHello, hello.Type)
	phase = "spoofed authority write"
	reply, _ := exchange(t, conn, WSMessage{
		ID: "spoofed-header", Type: "set_debug",
		Payload: json.RawMessage(`{"debug":true,"source":"web","trustedSettingsWrite":true}`),
	})
	require.Equal(t, EventError, reply.Type)
	require.Equal(t, config.ErrSettingsLocked.Error(), reply.Error)
	after, err := os.ReadFile(config.GlobalConfigData())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestUISettingsAuthorityRequiresAcceptedOrigin(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	app := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	config.SetProcessPassword("")
	t.Cleanup(func() { config.SetProcessPassword("") })
	require.NoError(t, app.Store().LockSettings(config.ScopeGlobal, "locked"))
	before, err := os.ReadFile(config.GlobalConfigData())
	require.NoError(t, err)
	srv := New(app, "127.0.0.1:0", nil)
	endpoint := startPolicyServer(t, srv)
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	var handshakeStatus int
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("settings authority transport: listener=ui origin=rejected status=%d", handshakeStatus)
		}
	})
	// Валидная UI authentication не отменяет Origin check.
	headers := http.Header{
		"Authorization": {"Bearer " + srv.Token()},
		"Origin":        {"https://untrusted.example"},
	}
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(endpoint, "http")+"/ws", headers)
	if conn != nil {
		_ = conn.Close()
	}
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if response != nil {
		handshakeStatus = response.StatusCode
	}
	require.Error(t, err)
	require.Nil(t, conn)
	require.NotNil(t, response)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	after, err := os.ReadFile(config.GlobalConfigData())
	require.NoError(t, err)
	require.Equal(t, before, after)
}
