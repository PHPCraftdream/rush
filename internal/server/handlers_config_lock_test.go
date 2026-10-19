package server

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLockedConfigHandlersRefuseWithoutMutation(t *testing.T) {
	cases := []struct {
		name string
		run  func(*app.App, *Client)
	}{
		{"debug", func(a *app.App, c *Client) {
			p, _ := json.Marshal(SetDebugPayload{Debug: true})
			handleSetDebug(a, c, WSMessage{ID: "x", Payload: p})
		}},
		{"context_add", func(a *app.App, c *Client) {
			p, _ := json.Marshal(AddContextPathPayload{Path: "added"})
			handleAddContextPath(a, c, WSMessage{ID: "x", Payload: p})
		}},
		{"context_remove", func(a *app.App, c *Client) {
			p, _ := json.Marshal(RemoveContextPathPayload{Path: "kept"})
			handleRemoveContextPath(a, c, WSMessage{ID: "x", Payload: p})
		}},
		{"theme", func(a *app.App, c *Client) {
			p, _ := json.Marshal(SetThemePayload{Theme: "dark"})
			handleSetTheme(a, c, WSMessage{ID: "x", Payload: p})
		}},
		{"keep_alive", func(a *app.App, c *Client) {
			p, _ := json.Marshal(SetKeepAlivePayload{Enabled: false})
			handleSetKeepAlive(a, c, WSMessage{ID: "x", Payload: p})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAllGlobalConfigPaths(t)
			a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
			store := a.Store()
			config.SetProcessPassword("")
			t.Cleanup(func() { config.SetProcessPassword("") })
			require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.context_paths", []string{"kept"}))
			require.NoError(t, store.SetTheme(config.ScopeGlobal, "light"))
			require.NoError(t, store.LockSettings(config.ScopeGlobal, "locked"))
			path := config.GlobalConfigData()
			beforeDisk, err := os.ReadFile(path)
			require.NoError(t, err)
			beforeConfig, err := json.Marshal(store.Config())
			require.NoError(t, err)

			client := newTestClient()
			tc.run(a, client)
			var reply WSMessage
			select {
			case raw := <-client.send:
				require.NoError(t, json.Unmarshal(raw, &reply))
			default:
				t.Fatal("handler did not reply")
			}
			require.Equal(t, EventError, reply.Type)
			require.Equal(t, "settings change is forbidden by the user (settings are locked). Do not try to change models or any other settings and do not look for a way around this; ask the user instead", reply.Error)
			select {
			case raw := <-client.hub.broadcast:
				t.Fatalf("unexpected broadcast: %s", raw)
			default:
			}
			select {
			case raw := <-client.send:
				t.Fatalf("unexpected extra reply: %s", raw)
			default:
			}

			afterConfig, err := json.Marshal(store.Config())
			require.NoError(t, err)
			require.JSONEq(t, string(beforeConfig), string(afterConfig))
			afterDisk, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, beforeDisk, afterDisk)
		})
	}
}
