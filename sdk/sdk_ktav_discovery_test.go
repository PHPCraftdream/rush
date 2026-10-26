package sdk_test

// KTAV-4b SDK chunk: application mode must DISCOVER rush.ktav — and it
// must prefer rush.ktav over rush.json. sdk.go's openApplication reads
// configuration exclusively through config.Init (sdk/sdk.go,
// `store, err := config.Init(...)`), and config.Init's slot discovery
// selects <legacy>.ktav over <legacy>.json.
//
// To prove the PREFERENCE (not merely tolerance), the workspace is
// seeded with BOTH spellings pointing at DIFFERENT providers:
//
//   - rush.ktav  -> provider A (serverA, marker KTAV_DISCOVERY_OK, key A)
//   - rush.json  -> provider B (serverB, marker JSON_MUST_LOSE,  key B)
//
// If discovery selected rush.json, the run would carry B's marker, hit
// serverB, and authenticate with B's key. The assertions below pin that
// the run is served by provider A only: A's marker text, A's Bearer
// key, and serverB.totalRequests()==0.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestOpenApplicationModePrefersRushKtavOverRushJSON pins that
// application mode discovers rush.ktav IN PREFERENCE to rush.json.
func TestOpenApplicationModePrefersRushKtavOverRushJSON(t *testing.T) {
	// Uses t.Setenv (via ktavIsolateSDKGlobalEnv) — NOT parallel.
	ktavIsolateSDKGlobalEnv(t)

	// Canonical temp dir (Windows: 8.3-short-name-safe, symlink-resolved).
	realTmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workDir := filepath.Join(realTmp, "ktav-vs-json-workspace")
	require.NoError(t, os.MkdirAll(workDir, 0o755))

	serverA := newCredentialServer(t, "KTAV_DISCOVERY_OK")
	serverB := newCredentialServer(t, "JSON_MUST_LOSE")

	providerJSON := func(serverURL, apiKey string) []byte {
		return []byte(`{
  "disable_default_providers": true,
  "providers": {
    "probe-provider": {
      "id": "probe-provider",
      "name": "probe-provider",
      "type": "openai-compat",
      "base_url": "` + serverURL + `",
      "api_key": "` + apiKey + `",
      "discover_models": false,
      "models": [
        {"id": "probe-model", "name": "probe-model", "context_window": 200000, "default_max_tokens": 1000}
      ]
    }
  },
  "models": {
    "smart": {"provider": "probe-provider", "model": "probe-model"},
    "fast": {"provider": "probe-provider", "model": "probe-model"}
  }
}`)
	}

	// rush.ktav holds provider A (serverA); rush.json holds provider B.
	ktavJSON := providerJSON(serverA.srv.URL, "keyA")
	jsonJSON := providerJSON(serverB.srv.URL, "keyB")

	ktavBytes, err := ktavcodec.ConvertJSONToKtav(ktavJSON)
	require.NoError(t, err, "fixture JSON must convert to Ktav")
	require.NotEmpty(t, ktavBytes)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.ktav"), ktavBytes, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), jsonJSON, 0o600))

	// Pin the slot selection the config package exposes: with both
	// spellings on disk, the logical rush.json slot resolves to rush.ktav.
	require.Equal(t,
		filepath.Join(workDir, "rush.ktav"),
		filepath.Clean(config.ActiveConfigFileName(filepath.Join(workDir, "rush.json"))))

	client, err := sdk.Open(context.Background(), sdk.Options{WorkingDir: workDir})
	require.NoError(t, err, "application mode must load a rush.ktav+ rush.json workspace")
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
		"the run must be served by the rush.ktav provider (A), not the rush.json provider (B)")

	// Every provider request went to the rush.ktav provider, with key A.
	require.NotEmpty(t, serverA.hits(),
		"the rush.ktav-selected provider must receive the request")
	for auth := range serverA.hits() {
		require.Equal(t, "Bearer keyA", auth)
	}

	// The rush.json provider was never contacted: discovery selected
	// rush.ktav, not rush.json.
	require.Equal(t, 0, serverB.totalRequests(),
		"rush.json provider must receive zero requests when rush.ktav is present")

	// rush.json was not rewritten or migrated by discovery.
	got, readErr := os.ReadFile(filepath.Join(workDir, "rush.json"))
	require.NoError(t, readErr)
	require.Equal(t, string(jsonJSON), string(got),
		"discovering rush.ktav must not rewrite the rush.json")

	// No migration artifacts may appear either.
	for _, forbidden := range []string{"rush.json.bak", "rush.json.ktav-migration", "rush.ktav.bak", "rush.ktav.ktav-migration"} {
		_, statErr := os.Stat(filepath.Join(workDir, forbidden))
		require.ErrorIs(t, statErr, os.ErrNotExist,
			fmt.Sprintf("%s must not appear", forbidden))
	}
}
