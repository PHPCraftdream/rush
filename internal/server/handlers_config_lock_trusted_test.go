package server

// Task #57: trusted Web-UI settings writes while the settings lock is active.
//
// These tests complement TestLockedConfigHandlersRefuseWithoutMutation
// (handlers_config_lock_test.go), which pins the guarded behavior: while the
// settings lock is active, plain (non-UI) clients are refused. Here we pin
// the other half of the contract:
//
//   - A client carrying trustedSettingsWrite (set only by Server.handleWS on
//     the UI-authenticated listener) may apply config writes while the lock
//     is held, with real disk bytes, broadcast envelopes, and replies
//     asserted — never just flags.
//   - A fully privileged admin-scoped serve/API-style client (admin scope +
//     AllowAdmin policy) is STILL refused: config/admin API scopes never
//     confer the UI write authority.
//   - End-to-end over a real socket: only a connection authenticated by the
//     UI listener's cookie transport gets the authority; an invalid cookie
//     cannot even upgrade.
//
// All tests are sequential (newAttachmentsTestApp calls t.Setenv and the
// config package keeps process-global state). The UI token used in test C is
// a throwaway per-test value but is never printed or asserted directly.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

// ── Test A: trusted-UI client applies writes while locked ────────────────────

// pendingConfigEnvelope snapshots the hub's coalescing slot. Hub.Broadcast
// routes EventConfig through BroadcastConfig (configPending + a wakeup token
// for the Run loop), NOT through h.broadcast — with a detached test hub (no
// Run loop) the envelope waits in the slot, which is exactly what the
// handler emitted and therefore what we assert on. See hub.go's
// Broadcast/BroadcastConfig.
func pendingConfigEnvelope(t *testing.T, h *Hub) []byte {
	t.Helper()
	h.configMu.Lock()
	defer h.configMu.Unlock()
	return h.configPending
}

// lockedSyncRev extracts the settings-lock hash from disk bytes. It fails if
// missing, and the caller must never log or assert the hash value itself in
// diagnostics beyond what require.Equal prints for sync_rev equality (the
// lock password "locked" is a throwaway test fixture).
func lockedSyncRev(t *testing.T, disk []byte) string {
	t.Helper()
	rev := gjson.GetBytes(disk, "sync_rev").String()
	require.NotEmpty(t, rev, "global config on disk must carry the settings-lock sync_rev")
	return rev
}

// readTrustedReply reads exactly one reply envelope from the client's send
// channel (buffered, filled synchronously by c.reply) — no hub Run loop is
// needed, mirroring p460_model_scope_test.go's newTestClient contract.
func readTrustedReply(t *testing.T, c *Client) WSMessage {
	t.Helper()
	select {
	case raw := <-c.send:
		var reply WSMessage
		require.NoError(t, json.Unmarshal(raw, &reply))
		return reply
	default:
		t.Fatal("handler did not reply")
		return WSMessage{}
	}
}

func requireNoMoreReplies(t *testing.T, c *Client) {
	t.Helper()
	select {
	case raw := <-c.send:
		t.Fatalf("unexpected extra reply: %s", raw)
	default:
	}
}

func requireNoPlainBroadcast(t *testing.T, c *Client) {
	t.Helper()
	select {
	case raw := <-c.hub.broadcast:
		t.Fatalf("unexpected broadcast: %s", raw)
	default:
	}
}

func TestLockedConfigHandlersTrustedUIClientApplies(t *testing.T) {
	var defaultContextPaths []string
	checkContextWire := func(t *testing.T, payload gjson.Result, paths ...string) {
		t.Helper()
		expected := append(slices.Clone(defaultContextPaths), paths...)
		slices.Sort(expected)
		expected = slices.Compact(expected)
		want, err := json.Marshal(expected)
		require.NoError(t, err)
		require.JSONEq(t, string(want), payload.Get("contextPaths").Raw)
	}
	// Same fixture rationale as TestLockedConfigHandlersRefuseWithoutMutation:
	// the raw-bytes-on-disk assertions only have their meaning with the
	// legacy-JSON migration gate off.
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	cases := []struct {
		name string
		// seed runs before LockSettings; the common context/theme seeds are
		// applied first.
		seed func(*config.ConfigStore) error
		run  func(*appPkg.App, *Client)
		// wantReply: handler answers with an EventResponse ok reply (theme
		// only broadcasts).
		wantReply bool
		// extraBroadcast: additional plain-broadcast envelope the handler
		// emits on client.hub.broadcast ("" = none). EventConfig is never
		// here — it goes through the coalescing slot.
		extraBroadcast string
		// diskCheck asserts on the raw global config bytes after the write.
		diskCheck func(*testing.T, []byte)
		// wireCheck asserts on the EventConfig payload the handler emitted.
		wireCheck func(*testing.T, gjson.Result)
		// scopedCheck asserts on the extraBroadcast payload. The scoped wire
		// is disk-derived (buildScopedModelsWire reads the on-disk slots);
		// effective-model values are recomputed by reload from the provider
		// catalog and are deliberately not asserted.
		scopedCheck func(*testing.T, gjson.Result)
	}{
		{
			name:      "set_debug",
			wantReply: true,
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(SetDebugPayload{Debug: true})
				handleSetDebug(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.True(t, gjson.GetBytes(disk, "options.debug").Bool())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				require.True(t, payload.Get("debug").Bool())
			},
		},
		{
			// set_theme only broadcasts; it never replies.
			name: "set_theme",
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(SetThemePayload{Theme: "dark"})
				handleSetTheme(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.Equal(t, "dark", gjson.GetBytes(disk, "options.tui.theme").String())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				require.Equal(t, "dark", payload.Get("theme").String())
			},
		},
		{
			// set_theme and set_keep_alive broadcast only — handleSetKeepAlive
			// never replies.
			name: "set_keep_alive",
			seed: func(s *config.ConfigStore) error {
				return s.SetConfigField(config.ScopeGlobal, "options.keep_alive_enabled", true)
			},
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(SetKeepAlivePayload{Enabled: false})
				handleSetKeepAlive(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.False(t, gjson.GetBytes(disk, "options.keep_alive_enabled").Bool())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				require.False(t, payload.Get("keepAliveEnabled").Bool())
			},
		},
		{
			name:      "add_context_path",
			wantReply: true,
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(AddContextPathPayload{Path: "added"})
				handleAddContextPath(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				paths := gjson.GetBytes(disk, "options.context_paths").Array()
				require.Len(t, paths, 2)
				require.Equal(t, "kept", paths[0].String())
				require.Equal(t, "added", paths[1].String())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				checkContextWire(t, payload, "kept", "added")
			},
		},
		{
			name:      "remove_context_path",
			wantReply: true,
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(RemoveContextPathPayload{Path: "kept"})
				handleRemoveContextPath(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				paths := gjson.GetBytes(disk, "options.context_paths").Array()
				require.Len(t, paths, 0)
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				checkContextWire(t, payload)
			},
		},
		{
			name:      "add_skills_path",
			wantReply: true,
			seed: func(s *config.ConfigStore) error {
				return s.SetConfigField(config.ScopeGlobal, "options.skills_paths", []string{"kept_skills"})
			},
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(AddSkillsPathPayload{Path: "added_skills"})
				handleAddSkillsPath(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				paths := gjson.GetBytes(disk, "options.skills_paths").Array()
				require.Len(t, paths, 2)
				require.Equal(t, "added_skills", paths[1].String())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				paths := payload.Get("skillsPaths").Array()
				require.Len(t, paths, 2)
				require.Equal(t, "added_skills", paths[1].String())
			},
		},
		{
			name:      "remove_skills_path",
			wantReply: true,
			seed: func(s *config.ConfigStore) error {
				return s.SetConfigField(config.ScopeGlobal, "options.skills_paths", []string{"kept_skills"})
			},
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(RemoveSkillsPathPayload{Path: "kept_skills"})
				handleRemoveSkillsPath(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.Len(t, gjson.GetBytes(disk, "options.skills_paths").Array(), 0)
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				require.Len(t, payload.Get("skillsPaths").Array(), 0)
			},
		},
		{
			name:           "set_scoped_model",
			wantReply:      true,
			extraBroadcast: EventScopedModels,
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(SetScopedModelPayload{Scope: "global", Slot: "smart", Provider: "prov", Model: "mdl"})
				handleSetScopedModel(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.Equal(t, "prov", gjson.GetBytes(disk, "models.smart.provider").String())
				require.Equal(t, "mdl", gjson.GetBytes(disk, "models.smart.model").String())
			},
			scopedCheck: func(t *testing.T, payload gjson.Result) {
				require.Equal(t, "prov", payload.Get("smart.global.provider").String())
				require.Equal(t, "mdl", payload.Get("smart.global.model").String())
			},
		},
		{
			name:           "clear_scoped_model",
			wantReply:      true,
			extraBroadcast: EventScopedModels,
			seed: func(s *config.ConfigStore) error {
				return s.UpdatePreferredModels(config.ScopeGlobal, map[config.SelectedModelType]config.SelectedModel{
					config.SelectedModelTypeSmart: {Provider: "prov", Model: "mdl"},
				})
			},
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(ClearScopedModelPayload{Scope: "global", Slot: "smart"})
				handleClearScopedModel(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.False(t, gjson.GetBytes(disk, "models.smart").Exists())
			},
			scopedCheck: func(t *testing.T, payload gjson.Result) {
				// The wire mirrors the cleared slot: no explicit value remains
				// at either scope for the smart key.
				require.Equal(t, "null", payload.Get("smart.global").Raw)
				require.Equal(t, "null", payload.Get("smart.workspace").Raw)
			},
		},
		{
			name:      "set_provider_key",
			wantReply: true,
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(SetProviderKeyPayload{ProviderID: "acme", APIKey: "k1"})
				handleSetProviderKey(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.Equal(t, "k1", gjson.GetBytes(disk, "providers.acme.api_key").String())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				// The wire never carries the raw key — only the presence bit.
				require.True(t, payload.Get("providers.acme.apiKeySet").Bool())
			},
		},
		{
			name:      "remove_provider_key",
			wantReply: true,
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(RemoveProviderKeyPayload{ProviderID: "acme"})
				handleRemoveProviderKey(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.False(t, gjson.GetBytes(disk, "providers.acme.api_key").Exists())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				require.False(t, payload.Get("providers.acme.apiKeySet").Bool())
			},
		},
		{
			name:      "add_custom_provider",
			wantReply: true,
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(AddCustomProviderPayload{
					ID: "cust", Type: "openai-compat", BaseURL: "http://x",
					// Without a model the reload drops the custom provider and the
					// wire assertion would fail.
					Models: []CustomModelPayload{{ID: "m1", Name: "m1"}},
				})
				handleAddCustomProvider(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.Equal(t, "cust", gjson.GetBytes(disk, "providers.cust.id").String())
				require.Equal(t, "http://x", gjson.GetBytes(disk, "providers.cust.base_url").String())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				require.Equal(t, "http://x", payload.Get("providers.cust.baseUrl").String())
			},
		},
		{
			name:      "set_provider_peak_hours",
			wantReply: true,
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(SetProviderPeakHoursPayload{
					ID:        "acme",
					Scope:     "global",
					PeakHours: &PeakHoursWirePayload{Start: "09:00", End: "17:00"},
				})
				handleSetProviderPeakHours(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.Equal(t, "09:00", gjson.GetBytes(disk, "providers.acme.peak_hours.start").String())
				require.Equal(t, "17:00", gjson.GetBytes(disk, "providers.acme.peak_hours.end").String())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				require.Equal(t, "09:00", payload.Get("providers.acme.peakHours.start").String())
			},
		},
		{
			// Clear variant: peakHours null removes the window from disk.
			name:      "clear_provider_peak_hours",
			wantReply: true,
			seed: func(s *config.ConfigStore) error {
				return s.SetConfigField(config.ScopeGlobal, "providers.acme.peak_hours", config.PeakHoursWindow{Start: "09:00", End: "17:00"})
			},
			run: func(a *appPkg.App, c *Client) {
				p, _ := json.Marshal(SetProviderPeakHoursPayload{ID: "acme", Scope: "global"})
				handleSetProviderPeakHours(a, c, WSMessage{ID: "x", Payload: p})
			},
			diskCheck: func(t *testing.T, disk []byte) {
				require.False(t, gjson.GetBytes(disk, "providers.acme.peak_hours").Exists())
			},
			wireCheck: func(t *testing.T, payload gjson.Result) {
				require.False(t, payload.Get("providers.acme.peakHours").Exists())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateAllGlobalConfigPaths(t)
			a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
			store := a.Store()
			defaultContextPaths = slices.Clone(store.Config().Options.ContextPaths)
			config.SetProcessPassword("")
			t.Cleanup(func() { config.SetProcessPassword("") })
			// Common seed values, mirroring the refusal test's fixture.
			require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.context_paths", []string{"kept"}))
			require.NoError(t, store.SetTheme(config.ScopeGlobal, "light"))
			// A full custom provider keeps every later reload successful and
			// the post-write EventConfig payloads deterministic; reload
			// drops model-less custom providers, so the seed carries a
			// model.
			require.NoError(t, store.SetConfigField(config.ScopeGlobal, "providers.acme", config.ProviderConfig{
				ID:      "acme",
				Name:    "acme",
				Type:    "openai-compat",
				BaseURL: "http://acme.example",
				APIKey:  "old",
				Models:  []catwalk.Model{{ID: "m1", Name: "m1"}},
			}))
			if tc.seed != nil {
				require.NoError(t, tc.seed(store))
			}
			require.NoError(t, store.LockSettings(config.ScopeGlobal, "locked"))
			path := config.GlobalConfigData()
			beforeDisk, err := os.ReadFile(path)
			require.NoError(t, err)
			beforeRev := lockedSyncRev(t, beforeDisk)

			client := newTestClient()
			client.trustedSettingsWrite = true
			tc.run(a, client)

			// Reply: either an ok response (most handlers) or nothing
			// (set_theme broadcasts only). Never an EventError.
			if tc.wantReply {
				reply := readTrustedReply(t, client)
				require.Equal(t, EventResponse, reply.Type)
				require.Empty(t, reply.Error)
				status := gjson.GetBytes(reply.Payload, "status")
				require.Equal(t, "ok", status.String())
			} else {
				requireNoMoreReplies(t, client)
			}
			requireNoMoreReplies(t, client)

			// Plain broadcasts: exactly the extra envelope the handler emits
			// (EventScopedModels for the scoped-model handlers), nothing else.
			if tc.extraBroadcast != "" {
				select {
				case raw := <-client.hub.broadcast:
					var broadcastEnv WSMessage
					require.NoError(t, json.Unmarshal(raw, &broadcastEnv))
					require.Equal(t, tc.extraBroadcast, broadcastEnv.Type)
					if tc.scopedCheck != nil {
						tc.scopedCheck(t, gjson.Parse(string(broadcastEnv.Payload)))
					}
				default:
					t.Fatalf("expected a %s broadcast, got none", tc.extraBroadcast)
				}
			}
			requireNoPlainBroadcast(t, client)

			// EventConfig broadcast: the handler's coalesced envelope.
			pending := pendingConfigEnvelope(t, client.hub)
			require.NotEmpty(t, pending, "handler must emit an EventConfig broadcast")
			var env WSMessage
			require.NoError(t, json.Unmarshal(pending, &env))
			require.Equal(t, EventConfig, env.Type)
			if tc.wireCheck != nil {
				tc.wireCheck(t, gjson.Parse(string(env.Payload)))
			}

			// Disk: the write really landed, and the settings lock was left
			// byte-identical (sync_rev unchanged and still verifies against
			// the lock password).
			afterDisk, err := os.ReadFile(path)
			require.NoError(t, err)
			if tc.diskCheck != nil {
				tc.diskCheck(t, afterDisk)
			}
			afterRev := lockedSyncRev(t, afterDisk)
			require.Equal(t, beforeRev, afterRev, "trusted write must not disturb sync_rev")
			require.True(t, config.VerifyPassword("locked", afterRev),
				"sync_rev must still verify against the active lock password")
		})
	}
}

// ── Test B: admin-scoped non-UI client is still refused ──────────────────────

func TestLockedConfigHandlersAdminScopedClientStillRefused(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	// exactLockedText: the store-backed handlers surface the guard error
	// verbatim, exactly like the existing refusal test asserts.
	exactLockedText := config.ErrSettingsLocked.Error()

	cases := []struct {
		name string
		seed func(*config.ConfigStore) error
		run  func(*appPkg.App, *Client)
	}{
		{"set_debug", nil, func(a *appPkg.App, c *Client) {
			p, _ := json.Marshal(SetDebugPayload{Debug: true})
			handleSetDebug(a, c, WSMessage{ID: "x", Payload: p})
		}},
		{"set_provider_key", nil, func(a *appPkg.App, c *Client) {
			p, _ := json.Marshal(SetProviderKeyPayload{ProviderID: "acme", APIKey: "k1"})
			handleSetProviderKey(a, c, WSMessage{ID: "x", Payload: p})
		}},
		{"add_context_path", nil, func(a *appPkg.App, c *Client) {
			p, _ := json.Marshal(AddContextPathPayload{Path: "added"})
			handleAddContextPath(a, c, WSMessage{ID: "x", Payload: p})
		}},
		{"set_mcp_disabled", func(s *config.ConfigStore) error {
			return s.PersistMCPConfig(config.ScopeGlobal, "lockedsrv", config.MCPConfig{Type: config.MCPStdio, Command: "echo"})
		}, func(a *appPkg.App, c *Client) {
			p, _ := json.Marshal(SetMCPDisabledPayload{Name: "lockedsrv", Disabled: true})
			handleSetMCPDisabled(context.Background(), a, c, WSMessage{ID: "x", Payload: p})
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
			// A full custom provider keeps every later reload successful and
			// the post-write EventConfig payloads deterministic; reload
			// drops model-less custom providers, so the seed carries a
			// model.
			require.NoError(t, store.SetConfigField(config.ScopeGlobal, "providers.acme", config.ProviderConfig{
				ID:      "acme",
				Name:    "acme",
				Type:    "openai-compat",
				BaseURL: "http://acme.example",
				APIKey:  "old",
				Models:  []catwalk.Model{{ID: "m1", Name: "m1"}},
			}))
			if tc.seed != nil {
				require.NoError(t, tc.seed(store))
			}
			require.NoError(t, store.LockSettings(config.ScopeGlobal, "locked"))
			path := config.GlobalConfigData()
			beforeDisk, err := os.ReadFile(path)
			require.NoError(t, err)

			// Fully privileged serve/API-style listener client: admin scope
			// on an allow-admin listener. trustedSettingsWrite stays false —
			// scopes and policies must never confer the UI write authority.
			client := newTestClient()
			client.auth = newClientAuth(Principal{Scopes: []string{"admin"}}, CommandPolicy{AllowAdmin: true})

			tc.run(a, client)

			reply := readTrustedReply(t, client)
			require.Equal(t, EventError, reply.Type)
			if tc.name == "set_mcp_disabled" {
				// The MCP owner wraps the guard error, so the lock text is
				// carried inside the wrapper rather than verbatim.
				require.Contains(t, reply.Error, exactLockedText)
			} else {
				require.Equal(t, exactLockedText, reply.Error)
			}
			requireNoMoreReplies(t, client)
			requireNoPlainBroadcast(t, client)
			require.Empty(t, pendingConfigEnvelope(t, client.hub),
				"refused handler must not emit an EventConfig broadcast")

			afterDisk, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, beforeDisk, afterDisk, "refused write must leave disk untouched")
		})
	}
}

// ── Test C: end-to-end over a real socket ─────────────────────────────────────

// TestHandleWSGrantsTrustedSettingsOnlyOnUIListener drives a real UI-auth
// server: a cookie-authenticated browser-style connection applies set_debug
// while the settings lock is active, an invalid-cookie connection cannot even
// complete the upgrade, and the disk bytes prove both outcomes.
func TestHandleWSGrantsTrustedSettingsOnlyOnUIListener(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := a.Store()
	config.SetProcessPassword("")
	t.Cleanup(func() { config.SetProcessPassword("") })
	require.NoError(t, store.SetTheme(config.ScopeGlobal, "light"))
	require.NoError(t, store.LockSettings(config.ScopeGlobal, "locked"))
	path := config.GlobalConfigData()
	beforeDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	beforeRev := lockedSyncRev(t, beforeDisk)

	// New() builds the uiAuth listener: /ws behind Auth.Middleware, /auth
	// issuing the rush_session cookie. startPolicyServer (serve_listener_test.go)
	// runs Start with a context-cancel cleanup and returns the bound address.
	srv := New(a, "127.0.0.1:0", nil)
	endpoint := startPolicyServer(t, srv)

	// 1. Obtain the session cookie through the real /auth endpoint.
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	httpClient := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	authBody := strings.NewReader(`{"token":"` + srv.Token() + `"}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint+"/auth", authBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "auth failed: %s", body)
	parsed, err := url.Parse(endpoint)
	require.NoError(t, err)
	cookies := jar.Cookies(parsed)
	require.NotEmpty(t, cookies, "POST /auth must set the session cookie")
	var cookieHeader string
	for _, ck := range cookies {
		if ck.Name == cookieName {
			cookieHeader = ck.Name + "=" + ck.Value
		}
	}
	require.NotEmpty(t, cookieHeader, "expected a %s cookie", cookieName)

	// 2. Dial /ws with ONLY the cookie header as transport identity — never
	// the token in the URL query.
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	header := http.Header{"Cookie": []string{cookieHeader}}
	wsURL := "ws" + strings.TrimPrefix(endpoint, "http") + "/ws"
	conn, dialResp, err := dialer.Dial(wsURL, header) //nolint:bodyclose // gorilla upgrade responses carry no body
	require.NoError(t, err, "cookie-authenticated UI dial failed (%v)", dialResp)
	defer conn.Close()

	// 3. Apply set_debug over the socket while the settings lock is held.
	msg, err := json.Marshal(WSMessage{ID: "t1", Type: "set_debug", Payload: json.RawMessage(`{"debug":true}`)})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, msg))

	// Read envelopes until the EventConfig carrying debug=true AND the ok
	// response both arrived, or the deadline (failure guard) fires. Other
	// startup/state frames may legitimately interleave.
	var sawConfig, sawResponse bool
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for i := 0; i < 32 && !(sawConfig && sawResponse); i++ {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var envelope WSMessage
		require.NoError(t, json.Unmarshal(frame, &envelope))
		require.NotEqual(t, EventError, envelope.Type, "unexpected error envelope: %s", frame)
		switch envelope.Type {
		case EventConfig:
			require.True(t, gjson.GetBytes(envelope.Payload, "debug").Bool(),
				"EventConfig must carry the applied debug value: %s", frame)
			sawConfig = true
		case EventResponse:
			require.Equal(t, "t1", envelope.ID)
			require.Equal(t, "ok", gjson.GetBytes(envelope.Payload, "status").String())
			sawResponse = true
		}
	}
	require.True(t, sawConfig, "never received the EventConfig broadcast")
	require.True(t, sawResponse, "never received the EventResponse ok reply")
	_ = conn.Close()

	// 4. Disk proof: the write landed behind the lock, and the lock survived.
	afterDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(afterDisk, "options.debug").Bool())
	afterRev := lockedSyncRev(t, afterDisk)
	require.Equal(t, beforeRev, afterRev, "trusted write must not disturb sync_rev")
	require.True(t, config.VerifyPassword("locked", afterRev),
		"sync_rev must still verify against the active lock password")

	// 5. Invalid cookie: the upgrade itself must fail (HTTP 401) and disk
	// must stay untouched — unauthenticated clients have no write path.
	badHeader := http.Header{"Cookie": []string{cookieName + "=definitely-not-the-value"}}
	badConn, badResp, err := dialer.Dial(wsURL, badHeader) //nolint:bodyclose // gorilla upgrade responses carry no body
	require.Error(t, err, "invalid-cookie dial must fail")
	require.Nil(t, badConn)
	require.NotNil(t, badResp)
	require.Equal(t, http.StatusUnauthorized, badResp.StatusCode)

	untouchedDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, afterDisk, untouchedDisk, "unauthenticated dial must not change disk")
}

// TestLockedConfigPolicyWSAdminTokenRefusedUnderLock drives a REAL policy
// listener socket: a rush.v1 + bearer admin token through Server.handleWS on
// a NewWithPolicy server. This is the transport oracle the synthetic-Client
// refusal test cannot provide — if handleWS ever granted trustedSettingsWrite
// to policy listeners, the write below would succeed and fail this test. The
// payload deliberately carries a "trustedSettingsWrite" JSON field to prove
// wire input cannot confer the authority: the payload structs have no such
// field, and the connection flag is set only on the uiAuth listener.
func TestLockedConfigPolicyWSAdminTokenRefusedUnderLock(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	f := newServeWSScopeFixture(t, true, map[string][]string{"admin": {"admin"}})
	store := f.app.Store()
	config.SetProcessPassword("")
	t.Cleanup(func() { config.SetProcessPassword("") })
	require.NoError(t, store.LockSettings(config.ScopeGlobal, "locked"))
	path := config.GlobalConfigData()
	beforeDisk, err := os.ReadFile(path)
	require.NoError(t, err)

	conn := f.dial(t, "admin")
	reply, _ := exchange(t, conn, WSMessage{ID: "p1", Type: "set_debug", Payload: json.RawMessage(`{"debug":true,"trustedSettingsWrite":true}`)})
	require.Equal(t, EventError, reply.Type)
	require.Equal(t, config.ErrSettingsLocked.Error(), reply.Error)

	afterDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk, "the refused write must leave disk untouched")
	require.True(t, config.VerifyPassword("locked", gjson.GetBytes(afterDisk, "sync_rev").String()),
		"the settings lock must survive unchanged")
}
