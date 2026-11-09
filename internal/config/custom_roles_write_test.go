package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomRoleTypedWriterPreservesToolsAndLiteralNames(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"reader", "123", "data.reader", "аналитик", "code reader", ":literal"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rush.json")
			store := newTestConfigStore(testStoreOpts{globalDataPath: path})
			none := []string{}
			update := CustomRoleUpdate{Model: SelectedModel{Provider: "p", Model: "old", Think: true}, Tools: &none}
			require.NoError(t, store.SetCustomRole(ScopeGlobal, name, update))
			roles, err := store.CustomRolesForScope(ScopeGlobal)
			require.NoError(t, err)
			require.Len(t, roles, 1)
			require.NotNil(t, roles[name].Tools)
			require.Empty(t, *roles[name].Tools)
			require.True(t, roles[name].Think)
			update.Model.Model = "new"
			update.Model.Think = false
			update.Tools = nil
			require.NoError(t, store.SetCustomRole(ScopeGlobal, name, update))
			roles, err = store.CustomRolesForScope(ScopeGlobal)
			require.NoError(t, err)
			require.Equal(t, "new", roles[name].Model)
			require.False(t, roles[name].Think)
			require.NotNil(t, roles[name].Tools, "a model-only update preserves explicit denial")
			require.Empty(t, *roles[name].Tools)
			update.InheritTools = true
			require.NoError(t, store.SetCustomRole(ScopeGlobal, name, update))
			roles, err = store.CustomRolesForScope(ScopeGlobal)
			require.NoError(t, err)
			require.Nil(t, roles[name].Tools)
			require.Equal(t, "new", roles[name].Model)
			require.NoError(t, store.RemoveCustomRole(ScopeGlobal, name))
			roles, err = store.CustomRolesForScope(ScopeGlobal)
			require.NoError(t, err)
			require.Empty(t, roles)
		})
	}
}

func TestCustomRoleInvalidWritesPreserveDocumentAndSnapshot(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, extension := range []string{"json", "ktav"} {
		t.Run(extension, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rush."+extension)
			initial := []byte(`{"roles":{"reader":{"provider":"p","model":"m","tools":[]}},"options":{"debug":false}}`)
			format := settingsDocumentFormatOf(path)
			encoded, err := encodeSettingsDocument(initial, format)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, encoded, 0o600))
			cfg, err := loadFromBytes([][]byte{initial})
			require.NoError(t, err)
			store := newTestConfigStore(testStoreOpts{config: cfg, globalDataPath: path})

			err = store.SetConfigFields(ScopeGlobal, map[string]any{"roles.reader.tools": nil, "options.debug": true})
			require.ErrorContains(t, err, "not null")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, encoded, after, "a rejected role must roll back the entire field update")
			require.False(t, store.Config().Options.Debug)
			require.NotNil(t, store.Config().Roles["reader"].Tools)
			require.Empty(t, *store.Config().Roles["reader"].Tools)

			err = store.RemoveConfigField(ScopeGlobal, "roles.reader.provider")
			require.ErrorContains(t, err, "requires provider and model")
			after, err = os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, encoded, after)

			require.NoError(t, store.RemoveConfigField(ScopeGlobal, "roles.reader.tools"), "explicitly removing the field restores inheritance")
			doc, err := readSettingsDocument(path)
			require.NoError(t, err)
			loaded, err := loadFromBytes([][]byte{doc.jsonBytes})
			require.NoError(t, err)
			require.Nil(t, loaded.Roles["reader"].Tools)
			require.NoError(t, store.SetConfigField(ScopeGlobal, "roles.reader.tools", []string{}))
			doc, err = readSettingsDocument(path)
			require.NoError(t, err)
			loaded, err = loadFromBytes([][]byte{doc.jsonBytes})
			require.NoError(t, err)
			require.NotNil(t, loaded.Roles["reader"].Tools)
			require.Empty(t, *loaded.Roles["reader"].Tools, "the writer must persist [] rather than dropping or nulling it")
		})
	}
}
