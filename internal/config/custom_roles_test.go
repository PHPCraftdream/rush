package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomRoleToolsPresenceRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tools string
		present     bool
		count       int
	}{
		{name: "absent"},
		{name: "empty", tools: `,"tools":[]`, present: true},
		{name: "patterns", tools: `,"tools":["view","mcp_*","tool?"]`, present: true, count: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte(`{"provider":"provider","model":"model"` + tc.tools + `}`)
			var role CustomRole
			require.NoError(t, json.Unmarshal(raw, &role))
			require.Equal(t, tc.present, role.Tools != nil)
			if tc.present {
				require.Len(t, *role.Tools, tc.count)
			}
			encoded, err := json.Marshal(role)
			require.NoError(t, err)
			require.JSONEq(t, string(raw), string(encoded))
			var restored CustomRole
			require.NoError(t, json.Unmarshal(encoded, &restored))
			require.Equal(t, role, restored)
		})
	}
	var nilList []string
	encoded, err := json.Marshal(CustomRole{SelectedModel: SelectedModel{Provider: "p", Model: "m"}, Tools: &nilList})
	require.NoError(t, err)
	require.JSONEq(t, `{"provider":"p","model":"m","tools":[]}`, string(encoded))
}

func TestCustomRoleRejectsInvalidDefinitionWithoutReplacingValue(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"provider":"p"}`, `{"model":"m"}`,
		`{"provider":"p","model":"m","tools":null}`,
		`{"provider":"p","model":"m","tools":"*"}`,
		`{"provider":"p","model":"m","tools":[null]}`,
		`{"provider":"p","model":"m","tools":[2]}`,
		`{"provider":"p","model":"m","tools":[""]}`,
		`{"provider":"p","model":"m","tools":[" view"]}`,
		`{"provider":"p","model":"m","tools":["tool\nname"]}`,
		`{"provider":"p","model":"m","tool":["*"]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			role := CustomRole{SelectedModel: SelectedModel{Provider: "original", Model: "original"}}
			before := role
			require.Error(t, json.Unmarshal([]byte(raw), &role))
			require.Equal(t, before, role, "a rejected definition must not partially mutate the old snapshot")
		})
	}
}

func TestCustomRoleNameValidation(t *testing.T) {
	t.Parallel()
	for _, valid := range []string{"reader", "review-code", "data.reader", "аналитик", "code reader"} {
		require.NoError(t, ValidateCustomRoleName(valid), valid)
	}
	for _, invalid := range []string{"", "smart", "Smart", "FAST", "worker", "reviewer", " x", "x ", "a/b", `a\b`, "a\nb", strings.Repeat("a", 65)} {
		require.Error(t, ValidateCustomRoleName(invalid), invalid)
	}
}

func TestCustomRoleWholeDefinitionScopeOverride(t *testing.T) {
	t.Parallel()
	global := []byte(`{"roles":{"reader":{"provider":"p","model":"old","tools":["view"]},"keep":{"provider":"p","model":"keep","tools":[]}},"options":{"debug":true}}`)
	local := []byte(`{"roles":{"reader":{"provider":"q","model":"new"}}}`)
	beforeGlobal, beforeLocal := string(global), string(local)
	cfg, err := loadFromBytes([][]byte{global, local})
	require.NoError(t, err)
	require.Equal(t, "q", cfg.Roles["reader"].Provider)
	require.Equal(t, "new", cfg.Roles["reader"].Model)
	require.Nil(t, cfg.Roles["reader"].Tools, "a complete role override does not inherit another definition's tools")
	require.NotNil(t, cfg.Roles["keep"].Tools)
	require.Empty(t, *cfg.Roles["keep"].Tools)
	require.True(t, cfg.Options.Debug, "unrelated fields retain ordinary config merging")
	require.Equal(t, beforeGlobal, string(global))
	require.Equal(t, beforeLocal, string(local))

	cfg, err = loadFromBytes([][]byte{global, []byte(`{"roles":{"reader":{"provider":"q","model":"new","tools":[]}}}`)})
	require.NoError(t, err)
	require.NotNil(t, cfg.Roles["reader"].Tools)
	require.Empty(t, *cfg.Roles["reader"].Tools, "empty must not append to the global allowlist")
}

func TestCustomRoleValidatesEachScopeBeforeMerge(t *testing.T) {
	t.Parallel()
	valid := []byte(`{"roles":{"reader":{"provider":"p","model":"m"}}}`)
	for _, invalid := range []string{
		`{"roles":{"reader":{"tools":[]}}}`,
		`{"roles":{"reader":{"provider":"p","model":"m","tools":null}}}`,
		`{"roles":{"smart":{"provider":"p","model":"m"}}}`,
		`{"roles":null}`,
	} {
		_, err := loadFromBytes([][]byte{[]byte(invalid), valid})
		require.Error(t, err, "an invalid lower scope must not disappear under a later role: %s", invalid)
	}
}
