package server

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCustomRolePayloadToolsPresenceAndStrictDecoding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, suffix string
		present      bool
		invalid      bool
	}{
		{name: "missing"},
		{name: "empty", suffix: `,"tools":[]`, present: true},
		{name: "null", suffix: `,"tools":null`, invalid: true},
		{name: "string", suffix: `,"tools":"*"`, invalid: true},
		{name: "conflicting", suffix: `,"tools":[],"inherit_tools":true`, invalid: true},
		{name: "empty-name-pattern", suffix: `,"tools":[""]`, invalid: true},
		{name: "unknown-field", suffix: `,"trustedSettingsWrite":true`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var payload SetCustomRolePayload
			err := decodeCustomRolePayload([]byte(`{"scope":"global","name":"reader","provider":"p","model":"m"`+tc.suffix+`}`), &payload)
			if err == nil {
				var update config.CustomRoleUpdate
				update, err = customRoleUpdateFromPayload(payload)
				if !tc.invalid {
					require.Equal(t, tc.present, update.Tools != nil)
				}
			}
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCustomRoleHandlerScopesAndPreservedModelOnlyCap(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	addTestProvider(t, a)
	set := func(payload string) {
		t.Helper()
		client := newTestClient()
		handleSetCustomRole(a, client, WSMessage{ID: "role-set", Type: CmdSetCustomRole, Payload: json.RawMessage(payload)})
		reply := readTrustedReply(t, client)
		require.Equal(t, EventResponse, reply.Type, reply.Error)
		require.Empty(t, reply.Error)
	}
	set(`{"scope":"global","name":"reader","provider":"acme","model":"small-global","tools":[]}`)
	set(`{"scope":"global","name":"reader","provider":"acme","model":"large-global"}`)
	wire, err := buildCustomRolesWire(a)
	require.NoError(t, err)
	require.NotNil(t, wire.Global["reader"].Tools)
	require.Empty(t, *wire.Global["reader"].Tools)
	require.Equal(t, "large-global", wire.Effective["reader"].Model)
	set(`{"scope":"workspace","name":"reader","provider":"acme","model":"small-workspace"}`)
	wire, err = buildCustomRolesWire(a)
	require.NoError(t, err)
	require.Equal(t, "small-workspace", wire.Effective["reader"].Model)
	require.Nil(t, wire.Effective["reader"].Tools, "a new complete scoped definition replaces the lower definition")
	client := newTestClient()
	handleRemoveCustomRole(a, client, WSMessage{ID: "role-remove", Type: CmdRemoveCustomRole, Payload: json.RawMessage(`{"scope":"workspace","name":"reader"}`)})
	reply := readTrustedReply(t, client)
	require.Equal(t, EventResponse, reply.Type, reply.Error)
	wire, err = buildCustomRolesWire(a)
	require.NoError(t, err)
	require.NotContains(t, wire.Workspace, "reader")
	require.Equal(t, "large-global", wire.Effective["reader"].Model)
	require.NotNil(t, wire.Effective["reader"].Tools)
}

func TestCustomRoleHandlerLockedNonUIClientsCannotMutate(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	addTestProvider(t, a)
	config.SetProcessPassword("")
	t.Cleanup(func() { config.SetProcessPassword("") })
	require.NoError(t, a.Store().LockSettings(config.ScopeGlobal, "locked"))
	path := config.GlobalConfigData()
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, admin := range []bool{false, true} {
		client := newTestClient()
		if admin {
			client.auth = newClientAuth(Principal{Scopes: []string{"admin"}}, CommandPolicy{AllowAdmin: true})
		}
		handleSetCustomRole(a, client, WSMessage{ID: "refused", Type: CmdSetCustomRole, Payload: json.RawMessage(`{"scope":"global","name":"reader","provider":"acme","model":"small-global","tools":[]}`)})
		reply := readTrustedReply(t, client)
		require.Equal(t, EventError, reply.Type)
		require.Equal(t, config.ErrSettingsLocked.Error(), reply.Error)
		requireNoPlainBroadcast(t, client)
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
}

func TestCustomRoleCommandsRequireModelConfigAuthorityAndRejectPinnedSessions(t *testing.T) {
	t.Parallel()
	require.NoError(t, AuthorizeCommand(Principal{Scopes: []string{"read"}}, CmdGetCustomRoles, CommandPolicy{}))
	for _, command := range []string{CmdSetCustomRole, CmdRemoveCustomRole} {
		require.ErrorIs(t, AuthorizeCommand(Principal{Scopes: []string{"read"}}, command, CommandPolicy{}), ErrScopeForbidden)
		require.ErrorIs(t, AuthorizeCommand(Principal{Scopes: []string{"run"}}, command, CommandPolicy{}), ErrScopeForbidden)
		require.NoError(t, AuthorizeCommand(Principal{Scopes: []string{"config"}}, command, CommandPolicy{}))
	}
	for _, command := range []string{CmdGetCustomRoles, CmdSetCustomRole, CmdRemoveCustomRole} {
		rule, exists := sessionResolutionTable[command]
		require.True(t, exists)
		require.Equal(t, "deny", rule.class)
		require.Empty(t, rule.field)
	}
}

func TestCustomRoleWireDoesNotExposeProviderOptions(t *testing.T) {
	none := []string{}
	wire := customRolesToWire(map[string]config.CustomRole{
		"reader": {SelectedModel: config.SelectedModel{Provider: "p", Model: "m", ProviderOptions: map[string]any{"private-option": "fixture-value"}}, Tools: &none},
	})
	encoded, err := json.Marshal(wire)
	require.NoError(t, err)
	require.JSONEq(t, `{"reader":{"provider":"p","model":"m","tools":[]}}`, string(encoded))
}
