// Tests for the models-integrity startup abort (setupApp) and the
// setupAppLite warn-and-proceed path. REVERT-CHECKS (orchestrator runs the
// mutants):
//
//   - TestModelsIntegrityCmd_SessionsListAborts catches deleting the
//     `store.ModelsIntegrity()` check in setupApp (internal/cmd/root.go:426).
//   - TestModelsIntegrityCmd_ModelsStateWarns (a) catches deleting the
//     warnModelsEditedDirectly() call in internal/cmd/models_state.go.
//   - TestModelsIntegrityCmd_ModelsStateWarns (b) catches deleting
//     `payload["edited_directly"] = true` in models_state.go.
//   - TestModelsIntegrityCmd_ModelsUseRepairs catches deleting the warning
//     or breaking the re-sign in SetConfigFields
//     (internal/config/store_write.go:376) — without the re-sign the
//     follow-up `sessions list` fails.
//   - TestModelsIntegrityCmd_LockedMissingKeyAborts catches removing the
//     anySettingsLockExists condition in
//     internal/config/settings_models_integrity.go (missing cache_rev with
//     a lock would wrongly be accepted).
package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tamperedCacheRev = "0000000000000000000000000000000000000000000000000000000000000000"

// seedTamperedModels overwrites the global rush.json with a models object
// signed with a bogus cache_rev, keeping the zai provider so Load succeeds.
func seedTamperedModels(t *testing.T, globalPath string) {
	t.Helper()
	fixture := `{"providers":{"zai":{"api_key":"test-zai-key"}},` +
		`"cache_rev":"` + tamperedCacheRev + `",` +
		`"models":{"smart":{"provider":"zai","model":"glm-4.6","reasoning_effort":"on"}}}`
	require.NoError(t, os.WriteFile(globalPath, []byte(fixture), 0o644))
}

// seedLockedModelsMissingRev overwrites the global rush.json with a settings
// lock (sync_rev) and a models object but NO cache_rev, keeping the zai
// provider so Load succeeds.
func seedLockedModelsMissingRev(t *testing.T, globalPath string) {
	t.Helper()
	fixture := `{"providers":{"zai":{"api_key":"test-zai-key"}},` +
		`"sync_rev":"` + config.HashPassword("pw") + `",` +
		`"models":{"smart":{"provider":"zai","model":"glm-4.6","reasoning_effort":"on"}}}`
	require.NoError(t, os.WriteFile(globalPath, []byte(fixture), 0o644))
}

// runRushTreeCapture runs args through the real root command capturing BOTH
// stdout and stderr; flag resets clear Changed like the neighbouring helpers.
func runRushTreeCapture(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	resetModelsUseFlags(t)
	resetModelsStateFlags(t)
	resetModelsUnsetFlags(t)
	origCwd, _ := rootCmd.PersistentFlags().GetString("cwd")
	origDataDir, _ := rootCmd.PersistentFlags().GetString("data-dir")
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("cwd", origCwd)
		if f := rootCmd.PersistentFlags().Lookup("cwd"); f != nil {
			f.Changed = false
		}
		_ = rootCmd.PersistentFlags().Set("data-dir", origDataDir)
		if f := rootCmd.PersistentFlags().Lookup("data-dir"); f != nil {
			f.Changed = false
		}
		rootCmd.SetArgs(nil)
	})
	_ = rootCmd.PersistentFlags().Set("cwd", "")
	if f := rootCmd.PersistentFlags().Lookup("cwd"); f != nil {
		f.Changed = false
	}
	_ = rootCmd.PersistentFlags().Set("data-dir", "")
	if f := rootCmd.PersistentFlags().Lookup("data-dir"); f != nil {
		f.Changed = false
	}

	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = outW
	os.Stderr = errW
	outDone := make(chan struct{})
	var outBuf bytes.Buffer
	go func() { _, _ = io.Copy(&outBuf, outR); close(outDone) }()
	errDone := make(chan struct{})
	var errBuf bytes.Buffer
	go func() { _, _ = io.Copy(&errBuf, errR); close(errDone) }()

	rootCmd.SetArgs(args)
	runErr := rootCmd.Execute()
	rootCmd.SetArgs(nil)

	_ = outW.Close()
	_ = errW.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr
	<-outDone
	<-errDone
	return outBuf.String(), errBuf.String(), runErr
}

// TestModelsIntegrityCmd_SessionsListAborts proves a tampered cache_rev makes
// a setupApp command abort with exactly config.ErrModelsEditedDirectly.
func TestModelsIntegrityCmd_SessionsListAborts(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	seedTamperedModels(t, globalPath)

	_, _, runErr := runRushTreeCapture(t, "sessions", "list")
	require.ErrorIs(t, runErr, config.ErrModelsEditedDirectly)
	require.EqualError(t, runErr, config.ErrModelsEditedDirectly.Error())
}

// TestModelsIntegrityCmd_ModelsStateWarns proves setupAppLite commands warn
// on stderr and proceed; the JSON payload flags edited_directly only on
// failure, and a clean store stays silent both ways.
func TestModelsIntegrityCmd_ModelsStateWarns(t *testing.T) {
	warning := "warning: the model settings were changed outside the rush models commands"

	globalPath := isolatedModelsEnv(t)
	seedTamperedModels(t, globalPath)

	// (a) text run: warning on stderr, output still rendered.
	textOut, textErrOut, runErr := runRushTreeCapture(t, "models", "state")
	require.NoError(t, runErr)
	assert.Contains(t, textErrOut, warning)
	assert.Contains(t, textOut, "EFFECTIVE")

	// (b) JSON run: payload carries edited_directly=true.
	jsonOut, _, runErr := runRushTreeCapture(t, "models", "state", "--json")
	require.NoError(t, runErr)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &payload))
	require.Equal(t, true, payload["edited_directly"])

	// (c) clean store: no warning, and no edited_directly key at all.
	require.NoError(t, os.WriteFile(globalPath,
		[]byte(`{"providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o644))
	_, cleanErrOut, runErr := runRushTreeCapture(t, "models", "state")
	require.NoError(t, runErr)
	assert.NotContains(t, cleanErrOut, warning)
	cleanJSON, _, runErr := runRushTreeCapture(t, "models", "state", "--json")
	require.NoError(t, runErr)
	assert.NotContains(t, strings.TrimSpace(cleanJSON), `"edited_directly"`)
	var cleanPayload map[string]any
	require.NoError(t, json.Unmarshal([]byte(cleanJSON), &cleanPayload))
	assert.NotContains(t, cleanPayload, "edited_directly")
}

// TestModelsIntegrityCmd_ModelsUseRepairs proves models use warns, re-signs
// the tampered store, and the next setupApp command succeeds again.
func TestModelsIntegrityCmd_ModelsUseRepairs(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	seedTamperedModels(t, globalPath)

	_, useErrOut, runErr := runRushTreeCapture(t, "models", "use", "--smart", "glm4_6", "--fast", "glm5_turbo")
	require.NoError(t, runErr)
	assert.Contains(t, useErrOut,
		"warning: the model settings were changed outside the rush models commands")

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var doc struct {
		CacheRev string `json:"cache_rev"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	require.NotEmpty(t, doc.CacheRev)
	require.NotEqual(t, tamperedCacheRev, doc.CacheRev, "models use must re-sign cache_rev")

	// The re-sign repaired the store: setupApp no longer aborts.
	_, _, runErr = runRushTreeCapture(t, "sessions", "list")
	require.NoError(t, runErr)

	// And a final models state is silent.
	_, stateErrOut, runErr := runRushTreeCapture(t, "models", "state")
	require.NoError(t, runErr)
	assert.NotContains(t, stateErrOut,
		"warning: the model settings were changed outside the rush models commands")
}

// TestModelsIntegrityCmd_LockedMissingKeyAborts proves a missing cache_rev
// with a settings lock is treated as tampering by setupApp, while
// setupAppLite still proceeds.
func TestModelsIntegrityCmd_LockedMissingKeyAborts(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	seedLockedModelsMissingRev(t, globalPath)

	_, _, runErr := runRushTreeCapture(t, "sessions", "list")
	require.ErrorIs(t, runErr, config.ErrModelsEditedDirectly)

	// setupAppLite does not abort on the same store.
	_, _, runErr = runRushTreeCapture(t, "models", "state")
	require.NoError(t, runErr)
}
