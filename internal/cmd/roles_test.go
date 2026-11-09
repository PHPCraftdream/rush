package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func runRolesTestCommand(t *testing.T, globalPath string, args ...string) (string, error) {
	t.Helper()
	command := newRolesCmd()
	command.PersistentFlags().String("data-dir", filepath.Dir(globalPath), "")
	command.SetContext(t.Context())
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}

func TestRolesCLISetShowUpdateAndRemove(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	output, err := runRolesTestCommand(t, globalPath, "set", "reader", "zai/glm-5.3", "--tools", "[]", "--json")
	require.NoError(t, err, output)
	var view customRoleView
	require.NoError(t, json.Unmarshal([]byte(output), &view))
	require.Equal(t, "reader", view.Name)
	require.Equal(t, "global", view.Source)
	require.NotNil(t, view.Role.Tools)
	require.Empty(t, *view.Role.Tools)

	output, err = runRolesTestCommand(t, globalPath, "set", "reader", "zai/glm-5.3@high", "--json")
	require.NoError(t, err, output)
	view = customRoleView{}
	require.NoError(t, json.Unmarshal([]byte(output), &view))
	require.Equal(t, "high", view.Role.ReasoningEffort)
	require.NotNil(t, view.Role.Tools, "changing only model/effort must not silently remove the cap")
	require.Empty(t, *view.Role.Tools)

	output, err = runRolesTestCommand(t, globalPath, "set", "reader", "zai/glm-5.3", "--inherit-tools", "--json")
	require.NoError(t, err, output)
	view = customRoleView{}
	require.NoError(t, json.Unmarshal([]byte(output), &view))
	require.Nil(t, view.Role.Tools)
	output, err = runRolesTestCommand(t, globalPath, "show", "reader", "--scope", "global", "--json")
	require.NoError(t, err, output)
	var shown customRoleView
	require.NoError(t, json.Unmarshal([]byte(output), &shown))
	require.Equal(t, view, shown)

	output, err = runRolesTestCommand(t, globalPath, "remove", "reader", "--scope", "global", "--json")
	require.NoError(t, err, output)
	require.Contains(t, output, `"lower_scope_may_apply":true`)
	output, err = runRolesTestCommand(t, globalPath, "list", "--scope", "global", "--json")
	require.NoError(t, err, output)
	require.JSONEq(t, `[]`, output)
}

func TestRolesCLIInvalidInputDoesNotWrite(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	before, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	for _, args := range [][]string{
		{"set", "reader", "zai/glm-5.3", "--tools", "null"},
		{"set", "reader", "zai/glm-5.3", "--tools", `{"view":true}`},
		{"set", "reader", "zai/glm-5.3", "--tools", "[]", "--inherit-tools"},
		{"set", "smart", "zai/glm-5.3"},
		{"set", "reader", "zai/glm-5.3", "--scope", "bad"},
		{"set", "reader", "zai/glm-5.3", "--scope", ""},
		{"set", "reader", "zai/glm-5.3", "--scope", "global", "--local"},
	} {
		output, err := runRolesTestCommand(t, globalPath, args...)
		require.Error(t, err, output)
		after, err := os.ReadFile(globalPath)
		require.NoError(t, err)
		require.Equal(t, before, after, "invalid command must not write: %v", args)
	}
}
