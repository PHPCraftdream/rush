// Tests for `rush models state`'s RESTORE block: the pure command builder,
// a real-command-tree round trip, a literal golden of the full text output,
// and the --json contract. Uses the models_use_test.go isolation harness.
package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func restoreSel(provider, model, effort string) *config.SelectedModel {
	return &config.SelectedModel{Provider: provider, Model: model, ReasoningEffort: effort}
}

// runRushTree executes args through the REAL root command, with stdout
// captured (RunE prints via fmt, which bypasses cobra's output writers).
// Flags that other replayed commands leave set are reset before each run,
// and rootCmd's own state is restored for later tests in the binary.
func runRushTree(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetModelsUseFlags(t)
	resetModelsStateFlags(t)
	origCwd, _ := rootCmd.PersistentFlags().GetString("cwd")
	origDataDir, _ := rootCmd.PersistentFlags().GetString("data-dir")
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("cwd", origCwd)
		_ = rootCmd.PersistentFlags().Set("data-dir", origDataDir)
		rootCmd.SetArgs(nil)
	})
	_ = rootCmd.PersistentFlags().Set("cwd", "")
	_ = rootCmd.PersistentFlags().Set("data-dir", "")

	oldOut := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() { _, _ = io.Copy(&buf, r); close(done) }()

	rootCmd.SetArgs(args)
	runErr := rootCmd.Execute()
	rootCmd.SetArgs(nil)

	_ = w.Close()
	os.Stdout = oldOut
	<-done
	return buf.String(), runErr
}

// stubLocalCLIAvailable pins cliprovider's local-CLI detection so the raw
// "local-cli/<model>" replay path is deterministic on machines without one.
func stubLocalCLIAvailable(t *testing.T) {
	t.Helper()
	orig := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec {
		return []cliprovider.CLISpec{{ModelID: "cli-claude-fable-5", ModelName: "Claude Fable 5"}}
	}
	t.Cleanup(func() { cliprovider.AvailableFunc = orig })
}

// TestRestoreCommands_Table is the pure-function table: spec shapes, scope
// suffix, ordering, empty input, and POSIX quoting.
func TestRestoreCommands_Table(t *testing.T) {
	t.Run("atom with valid effort", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("zai", "glm-5.3", "high"),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart glm5_3-high"}, got)
	})
	t.Run("atom without effort", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeFast: restoreSel("zai", "glm-5-turbo", ""),
		}, nil)
		assert.Equal(t, []string{"rush models use --fast glm5_turbo"}, got)
	})
	t.Run("atom with stale effort falls back to bare atom", func(t *testing.T) {
		// "on" is no longer a valid glm5_3_flash level (low/high/max only).
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeWorker: restoreSel("zai", "glm-5.3-flash", "on"),
		}, nil)
		assert.Equal(t, []string{"rush models use --worker glm5_3_flash"}, got)
	})
	t.Run("EffortSource atom without effort goes raw", func(t *testing.T) {
		// `models use fable5` is rejected — a level is mandatory — while the
		// raw provider/model form is accepted without one.
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("local-cli", "cli-claude-fable-5", ""),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart local-cli/cli-claude-fable-5"}, got)
	})
	t.Run("EffortSource atom with effort keeps atom-level form", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("local-cli", "cli-claude-fable-5", "high"),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart fable5-high"}, got)
	})
	t.Run("non-atom with effort", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeReviewer: restoreSel("openai", "gpt-5", "high"),
		}, nil)
		assert.Equal(t, []string{"rush models use --reviewer openai/gpt-5@high"}, got)
	})
	t.Run("non-atom without effort", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("openai-codex", "gpt-6-luna", ""),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart openai-codex/gpt-6-luna"}, got)
	})
	t.Run("local scope gets --local and comes after global", func(t *testing.T) {
		got := restoreCommands(
			map[config.SelectedModelType]*config.SelectedModel{
				config.SelectedModelTypeFast: restoreSel("zai", "glm-5-turbo", ""),
			},
			map[config.SelectedModelType]*config.SelectedModel{
				config.SelectedModelTypeFast: restoreSel("zai", "glm-4.7-flash", "on"),
			},
		)
		assert.Equal(t, []string{
			"rush models use --fast glm5_turbo",
			"rush models use --fast glm4_7_flash-on --local",
		}, got)
	})
	t.Run("role and scope ordering", func(t *testing.T) {
		got := restoreCommands(
			map[config.SelectedModelType]*config.SelectedModel{
				config.SelectedModelTypeSmart:    restoreSel("zai", "glm-5.3", "high"),
				config.SelectedModelTypeFast:     restoreSel("zai", "glm-5-turbo", ""),
				config.SelectedModelTypeWorker:   restoreSel("zai", "glm-4.7-flash", "on"),
				config.SelectedModelTypeReviewer: restoreSel("openai", "gpt-5", "max"),
			},
			map[config.SelectedModelType]*config.SelectedModel{
				config.SelectedModelTypeSmart: restoreSel("openai-codex", "gpt-6-luna", ""),
			},
		)
		assert.Equal(t, []string{
			"rush models use --smart glm5_3-high",
			"rush models use --smart openai-codex/gpt-6-luna --local",
			"rush models use --fast glm5_turbo",
			"rush models use --worker glm4_7_flash-on",
			"rush models use --reviewer openai/gpt-5@max",
		}, got)
	})
	t.Run("empty input is empty non-nil", func(t *testing.T) {
		got := restoreCommands(nil, nil)
		require.NotNil(t, got)
		assert.Empty(t, got)
	})
	t.Run("spec with whitespace is single-quoted", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeSmart: restoreSel("my prov", "mo del", ""),
		}, nil)
		assert.Equal(t, []string{"rush models use --smart 'my prov/mo del'"}, got)
	})
	t.Run("spec with single quote is escaped", func(t *testing.T) {
		got := restoreCommands(map[config.SelectedModelType]*config.SelectedModel{
			config.SelectedModelTypeFast: restoreSel("p", "o'brien", ""),
		}, nil)
		// Expected uses octal escape (backslash-quote-quote): 'p/o'\''brien'.
		assert.Equal(t, []string{"rush models use --fast 'p/o'\134''brien'"}, got)
	})
}

// TestRestoreCommands_EmptyState covers the nothing-set-anywhere case: the
// section still prints with the placeholder, and --json carries [] not null.
func TestRestoreCommands_EmptyState(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsStateFlags(t)
	text, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)
	assert.Contains(t, text, "RESTORE (run these to re-create this state)")
	assert.Contains(t, text, "(nothing is set explicitly)")

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &top))
	require.Contains(t, top, "restore_commands")
	require.Equal(t, "[]", string(top["restore_commands"]),
		"empty restore_commands must marshal as [] not null")
}

// TestRestoreCommands_RoundTrip is the oracle: a state built via `models use`
// must be re-creatable byte-for-byte by replaying the emitted commands
// through the REAL command tree (rootCmd.Execute) in a fresh store.
func TestRestoreCommands_RoundTrip(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"--smart", "glm4_6", "--fast", "glm5_turbo", "--worker", "glm4_7_flash-on", "--reviewer", "glm5_turbo")
	require.NoError(t, runErr)
	resetModelsUseFlags(t)
	_, runErr = runModelsCmd(t, modelsUseCmd, "--local", "--fast", "glm4_7_flash")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	origJSON, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)
	var orig map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(origJSON), &orig))
	var cmds []string
	require.NoError(t, json.Unmarshal(orig["restore_commands"], &cmds))
	require.NotEmpty(t, cmds)

	// Fresh isolated store; replay every emitted command line through the
	// command tree. cwd is already the fresh workspace, so the --local
	// replay writes there, exactly like the original fixture did.
	isolatedModelsEnv(t)
	for _, c := range cmds {
		require.True(t, strings.HasPrefix(c, "rush "), "unexpected command %q", c)
		require.NotContains(t, c, "'", "quoted specs are outside this fixture: %q", c)
		args := strings.Fields(strings.TrimPrefix(c, "rush "))
		_, runErr := runRushTree(t, args...)
		require.NoError(t, runErr, "restore command %q must run cleanly", c)
	}

	restoredJSON, runErr := runRushTree(t, "models", "state", "--json")
	require.NoError(t, runErr)
	var restored map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(restoredJSON), &restored))
	require.Equal(t, string(orig["effective"]), string(restored["effective"]),
		"effective block must be identical after replaying restore_commands")
	require.Equal(t, string(orig["global"]), string(restored["global"]),
		"global block must be identical after replaying restore_commands")
	require.Equal(t, string(orig["local"]), string(restored["local"]),
		"local block must be identical after replaying restore_commands")
}

// TestRestoreCommands_RawLocalCLIFormExecutes proves the F3 replacement form
// is really accepted: `models use --reviewer local-cli/cli-claude-fable-5`
// (raw, no level — the exact spec restoreSpec emits for an EffortSource atom
// with no stored effort) runs through the command tree, while the bare atom
// form would be rejected.
func TestRestoreCommands_RawLocalCLIFormExecutes(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	stubLocalCLIAvailable(t)

	_, runErr := runRushTree(t, "models", "use", "--reviewer", "local-cli/cli-claude-fable-5")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"cli-claude-fable-5"`,
		"the raw local-cli form must be accepted and written without a level")
}

// restoreGoldenFull is the byte-exact full `rush models state` text for the
// fixture below, captured from the pre-RESTORE rendering (feature appends).
const restoreGoldenFull = `EFFECTIVE
  smart:  zai/glm-4.6 (atom: glm4_6)  (unset -> thinking on, high)   (from GLOBAL)
  fast:  zai/glm-4.7-flash (atom: glm4_7_flash)  (unset -> thinking on, high)   (from LOCAL)
  worker:  zai/glm-4.7-flash effort=on (atom: glm4_7_flash-on)   (from GLOBAL)
  reviewer:  zai/glm-5-turbo (atom: glm5_turbo)  (unset -> thinking on, high)   (from GLOBAL)

SCOPES
  global  smart = zai/glm-4.6  (unset -> thinking on, high)         (effective)
  global  fast = zai/glm-5-turbo  (unset -> thinking on, high)      (overridden by local)
  global  worker = zai/glm-4.7-flash effort=on                      (effective)
  global  reviewer = zai/glm-5-turbo  (unset -> thinking on, high)  (effective)
  local   smart = —                                                 (not set)
  local   fast = zai/glm-4.7-flash  (unset -> thinking on, high)    (effective)
  local   worker = —                                                (not set)
  local   reviewer = —                                              (not set)

RESTORE (run these to re-create this state)
  rush models use --smart glm4_6
  rush models use --fast glm5_turbo
  rush models use --fast glm4_7_flash --local
  rush models use --worker glm4_7_flash-on
  rush models use --reviewer glm5_turbo
`

// TestRestoreCommands_TextPrefixByteIdentical pins the whole text output
// against the literal golden above: any byte changed in EFFECTIVE, SCOPES,
// the separator, the heading or the command lines turns this red.
func TestRestoreCommands_TextPrefixByteIdentical(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"--smart", "glm4_6", "--fast", "glm5_turbo", "--worker", "glm4_7_flash-on", "--reviewer", "glm5_turbo")
	require.NoError(t, runErr)
	resetModelsUseFlags(t)
	_, runErr = runModelsCmd(t, modelsUseCmd, "--local", "--fast", "glm4_7_flash")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	text, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)

	require.Equal(t, restoreGoldenFull, text)
}

// TestRestoreCommands_JSONShape pins the --json contract: restore_commands
// is an array of strings and every pre-existing key is still present.
func TestRestoreCommands_JSONShape(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "--smart", "glm4_6")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)

	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &top))
	for _, k := range []string{"effective", "global", "local", "restore_commands"} {
		require.Contains(t, top, k)
	}
	var cmds []string
	require.NoError(t, json.Unmarshal(top["restore_commands"], &cmds))
	require.Equal(t, []string{"rush models use --smart glm4_6"}, cmds)
}

// TestRestoreCommands_NoSecretLeakage proves the RESTORE section can only
// ever carry provider/model/effort: a fake api key in the provider fixture
// must not appear in any output, text or JSON.
func TestRestoreCommands_NoSecretLeakage(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	fixture := `{"providers":{"zai":{"api_key":"sk-FAKE-SECRET-abc123"}},` +
		`"models":{"smart":{"provider":"zai","model":"glm-4.6","reasoning_effort":"on"}}}`
	require.NoError(t, os.WriteFile(globalPath, []byte(fixture), 0o644))
	// Sanity: the secret really is on disk, so the assertions below are not
	// vacuously true.
	onDisk, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.Contains(t, string(onDisk), "sk-FAKE-SECRET-abc123")

	resetModelsStateFlags(t)
	text, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)
	assert.NotContains(t, text, "sk-FAKE-SECRET-abc123")
	assert.Contains(t, text, "rush models use --smart glm4_6-on")

	resetModelsStateFlags(t)
	jsonOut, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)
	assert.NotContains(t, jsonOut, "sk-FAKE-SECRET-abc123")
}
