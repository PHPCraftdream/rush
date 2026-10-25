package sdk_test

// KTAV-4b SDK chunk: application mode must DISCOVER rush.ktav. sdk.go's
// openApplication reads configuration exclusively through config.Init
// (sdk/sdk.go, `store, err := config.Init(...)`), and config.Init's slot
// discovery selects <legacy>.ktav over <legacy>.json, so a workspace
// holding ONLY rush.ktav must be served exactly like one holding
// rush.json — with no hardcoded rush.json read or write anywhere in the
// SDK Open path (verified by grep before this test was written).
//
// The fixture writes a rush.ktav (Ktav-encoded via ktavcodec, converted
// from the same JSON shape every other sdk test uses, because config.Load
// fails with "no custom providers" when default providers are disabled)
// and NOTHING else, opens an application-mode client, and asserts the
// run is served by the rush.ktav-selected provider.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestOpenApplicationModeDiscoversRushKtav pins that application mode
// discovers and serves runs from a rush.ktav-only workspace.
func TestOpenApplicationModeDiscoversRushKtav(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	// Canonical temp dir (Windows: 8.3-short-name-safe, symlink-resolved).
	realTmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workDir := filepath.Join(realTmp, "ktav-only-workspace")
	require.NoError(t, os.MkdirAll(workDir, 0o755))

	server := newCredentialServer(t, "KTAV_DISCOVERY_OK")

	rushJSON := []byte(`{
  "disable_default_providers": true,
  "providers": {
    "ktav-provider": {
      "id": "ktav-provider",
      "name": "ktav-provider",
      "type": "openai-compat",
      "base_url": "` + server.srv.URL + `",
      "api_key": "ktav-key",
      "discover_models": false,
      "models": [
        {"id": "ktav-model", "name": "ktav-model", "context_window": 200000, "default_max_tokens": 1000}
      ]
    }
  },
  "models": {
    "smart": {"provider": "ktav-provider", "model": "ktav-model"},
    "fast": {"provider": "ktav-provider", "model": "ktav-model"}
  }
}`)
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(rushJSON)
	require.NoError(t, err, "fixture JSON must convert to Ktav")
	require.NotEmpty(t, ktavBytes)
	// The workspace holds ONLY rush.ktav — no rush.json anywhere.
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.ktav"), ktavBytes, 0o600))

	// Pin the slot selection the config package exposes: with only
	// rush.ktav on disk, the logical rush.json slot resolves to rush.ktav.
	require.Equal(t,
		filepath.Join(workDir, "rush.ktav"),
		filepath.Clean(config.ActiveConfigFileName(filepath.Join(workDir, "rush.json"))))

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err, "application mode must load a rush.ktav-only workspace")
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	var buf bytes.Buffer
	req := sdk.RunRequest{
		Prompt:      "reply with exactly the marker text and nothing else",
		Mode:        sdk.RunModeJSON,
		Stdout:      &buf,
		HideSpinner: true,
	}
	res, runErr := client.Run(context.Background(), req)
	require.NoError(t, runErr, "run failed (output %q)", buf.String())
	require.Equal(t, "KTAV_DISCOVERY_OK", res.FinalText,
		"the run must be served by the rush.ktav-selected provider")

	// Every provider request went to the rush.ktav provider, with its key.
	require.NotEmpty(t, server.hits(), "the rush.ktav-selected provider must receive the request")
	for auth := range server.hits() {
		require.Equal(t, "Bearer ktav-key", auth)
	}

	// No legacy rush.json may be synthesized by discovery.
	_, statErr := os.Stat(filepath.Join(workDir, "rush.json"))
	require.ErrorIs(t, statErr, os.ErrNotExist,
		"discovering rush.ktav must not materialize a rush.json")
}
