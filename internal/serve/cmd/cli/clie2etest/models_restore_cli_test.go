package clie2etest

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const restoreGoldenFull = `EFFECTIVE
╭──────────┬──────────┬───────────────┬────────┬──────┬────────┬─────────────────┬────────────────────────────╮
│ ROLE     │ PROVIDER │ MODEL         │ EFFORT │ TIER │ SCOPE  │ ATOM            │ NOTE                       │
├──────────┼──────────┼───────────────┼────────┼──────┼────────┼─────────────────┼────────────────────────────┤
│ smart    │ zai      │ glm-4.6       │ —      │ —    │ global │ glm4_6          │ unset -> thinking on, high │
│ fast     │ zai      │ glm-4.7-flash │ —      │ —    │ local  │ glm4_7_flash    │ unset -> thinking on, high │
│ worker   │ zai      │ glm-4.7-flash │ on     │ —    │ global │ glm4_7_flash-on │                            │
│ reviewer │ zai      │ glm-5-turbo   │ —      │ —    │ global │ glm5_turbo      │ unset -> thinking on, high │
╰──────────┴──────────┴───────────────┴────────┴──────┴────────┴─────────────────┴────────────────────────────╯

SCOPES
╭────────┬──────────┬──────────┬───────────────┬────────┬──────┬─────────────────────┬────────────────────────────╮
│ SCOPE  │ ROLE     │ PROVIDER │ MODEL         │ EFFORT │ TIER │ STATUS              │ NOTE                       │
├────────┼──────────┼──────────┼───────────────┼────────┼──────┼─────────────────────┼────────────────────────────┤
│ global │ smart    │ zai      │ glm-4.6       │ —      │ —    │ effective           │ unset -> thinking on, high │
│ global │ fast     │ zai      │ glm-5-turbo   │ —      │ —    │ overridden by local │ unset -> thinking on, high │
│ global │ worker   │ zai      │ glm-4.7-flash │ on     │ —    │ effective           │                            │
│ global │ reviewer │ zai      │ glm-5-turbo   │ —      │ —    │ effective           │ unset -> thinking on, high │
│ local  │ smart    │ —        │ —             │ —      │ —    │ not set             │                            │
│ local  │ fast     │ zai      │ glm-4.7-flash │ —      │ —    │ effective           │ unset -> thinking on, high │
│ local  │ worker   │ —        │ —             │ —      │ —    │ not set             │                            │
│ local  │ reviewer │ —        │ —             │ —      │ —    │ not set             │                            │
╰────────┴──────────┴──────────┴───────────────┴────────┴──────┴─────────────────────┴────────────────────────────╯

RESTORE (run these to re-create this state)
  rush models use --smart glm4_6
  rush models use --fast glm5_turbo
  rush models use --fast glm4_7_flash --local
  rush models use --worker glm4_7_flash-on
  rush models use --reviewer glm5_turbo
`

func TestRestoreCommands_RoundTrip(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	use := runModelsCLI(t, fixture, true, "models", "use", "--smart", "glm4_6", "--fast", "glm5_turbo", "--worker", "glm4_7_flash-on", "--reviewer", "glm5_turbo")
	require.Equal(t, 0, use.ExitCode, use.Stderr)
	use = runModelsCLI(t, fixture, true, "models", "use", "--local", "--fast", "glm4_7_flash")
	require.Equal(t, 0, use.ExitCode, use.Stderr)
	state := runModelsCLI(t, fixture, true, "models", "state", "--json")
	require.Equal(t, 0, state.ExitCode, state.Stderr)
	var orig map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(state.Stdout), &orig))
	var commands []string
	require.NoError(t, json.Unmarshal(orig["restore_commands"], &commands))
	require.NotEmpty(t, commands)

	// Replay the emitted commands in a fresh fixture through cli.Execute.
	restoredFixture := newModelsCLIFixture(t)
	for _, command := range commands {
		require.True(t, strings.HasPrefix(command, "rush "), "unexpected command %q", command)
		require.NotContains(t, command, "'", "quoted specs are outside this fixture: %q", command)
		args := strings.Fields(strings.TrimPrefix(command, "rush "))
		restored := runModelsCLI(t, restoredFixture, true, args...)
		require.Equal(t, 0, restored.ExitCode, restored.Stderr, "restore command %q must run cleanly", command)
	}

	restored := runModelsCLI(t, restoredFixture, true, "models", "state", "--json")
	require.Equal(t, 0, restored.ExitCode, restored.Stderr)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(restored.Stdout), &got))
	for _, key := range []string{"effective", "global", "local"} {
		require.Equal(t, string(orig[key]), string(got[key]), "%s block must be identical after replaying restore_commands", key)
	}

	t.Run("local restore uses workspace config", checkModelsStateLocalRestoreUsesWorkspaceConfig)
}

// TestRestoreCommands_RawLocalCLIFormExecutes proves the raw local-cli form
// emitted for a model with no stored effort is accepted and persisted.
func TestRestoreCommands_RawLocalCLIFormExecutes(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	result := runModelsCLIRequest(t, fixture, modelsCLIRequest{
		Args: []string{"--cwd", fixture.cwd, "--data-dir", fixture.dataDir, "models", "use", "--reviewer", "local-cli/cli-claude-fable-5"},
		// Gate off keeps rush.json the active slot file the test reads below.
		KtavGateOn:        false,
		LocalCLIAvailable: true,
	})
	require.Equal(t, 0, result.ExitCode, result.Stderr)

	data, err := os.ReadFile(fixture.globalData + string(os.PathSeparator) + "rush.json")
	require.NoError(t, err)
	require.Contains(t, string(data), `"cli-claude-fable-5"`,
		"the raw local-cli form must be accepted and written without a level")
}

func TestRestoreCommands_TextPrefixByteIdentical(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	use := runModelsCLI(t, fixture, true, "models", "use", "--smart", "glm4_6", "--fast", "glm5_turbo", "--worker", "glm4_7_flash-on", "--reviewer", "glm5_turbo")
	require.Equal(t, 0, use.ExitCode, use.Stderr)
	use = runModelsCLI(t, fixture, true, "models", "use", "--local", "--fast", "glm4_7_flash")
	require.Equal(t, 0, use.ExitCode, use.Stderr)
	state := runModelsCLI(t, fixture, true, "models", "state")
	require.Equal(t, 0, state.ExitCode, state.Stderr)
	require.Equal(t, restoreGoldenFull, state.Stdout)
}

func TestRestoreCommands_JSONShape(t *testing.T) {
	fixture := newModelsCLIFixture(t)
	use := runModelsCLI(t, fixture, true, "models", "use", "--smart", "glm4_6")
	require.Equal(t, 0, use.ExitCode, use.Stderr)
	state := runModelsCLI(t, fixture, true, "models", "state", "--json")
	require.Equal(t, 0, state.ExitCode, state.Stderr)

	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(state.Stdout), &top))
	for _, key := range []string{"effective", "global", "local", "restore_commands"} {
		require.Contains(t, top, key)
	}
	var commands []string
	require.NoError(t, json.Unmarshal(top["restore_commands"], &commands))
	require.Equal(t, []string{"rush models use --smart glm4_6"}, commands)
}
