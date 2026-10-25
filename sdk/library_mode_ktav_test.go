package sdk_test

// KTAV-4b SDK chunk: library mode MUST stay diskless with respect to
// configuration. openLibrary (sdk/library_mode.go) builds its Config via
// buildLibraryConfig + config.NewLibraryStore and never calls config.Load,
// config.Init, or the Ktav migration — even when the package-global
// ktavAutoMigrate gate (config.SetKtavAutoMigrateForTest) is forced ON.
//
// Unlike a vacuous seed, EVERY config file the SDK could possibly
// discover (rush.json AND rush.ktav, in the working dir AND in the
// home dir — the latter reached via HOME/USERPROFILE) is seeded with a
// REAL provider whose base_url points at the "operator" httptest
// server. Because the operator URL is now actually inside every seeded
// config, operator.totalRequests()==0 is a meaningful assertion: were
// library mode to read, parse, or migrate any of those files and fall
// back to the configured provider, the operator would receive a request.
// And because each seeded file is in a different encoding/spelling, any
// read would also leave migration artifacts (.bak / .ktav-migration) or
// rewritten bytes behind — asserted below.
//
// Note on "the library-mode config does not contain that provider":
// sdk.Client exposes no config accessor, so this cannot be asserted by
// inspecting the client. It is proven BEHAVIORALLY instead: zero
// requests reach the operator provider from any seeded config, while
// the LibraryConfig-supplied provider demonstrably served the run.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestOpenLibraryMode_NeverMigratesOrReadsConfigFiles pins library mode's
// zero-config-I/O contract even with Ktav auto-migration enabled.
func TestOpenLibraryMode_NeverMigratesOrReadsConfigFiles(t *testing.T) {
	// NOT parallel: SetKtavAutoMigrateForTest mutates a package-global.
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	// Strict global-env isolation FIRST, then point HOME/USERPROFILE at
	// the dedicated seeded home dir (overriding the helper's value) so
	// every global-config lookup lands in a directory we control and
	// have seeded with the operator provider.
	ktavIsolateSDKGlobalEnv(t)

	realTmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	home := filepath.Join(realTmp, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// The operator server: its URL is baked into every seeded config
	// file, making zero-request assertions meaningful.
	operator := newCredentialServer(t, "RUSH_JSON_MUST_NOT_BE_HIT")
	library := newCredentialServer(t, "LIBRARY_MODE_STILL_WORKS")

	// A valid custom provider whose base_url IS the operator server.
	providerJSON := []byte(`{
  "disable_default_providers": true,
  "providers": {
    "operator-provider": {
      "id": "operator-provider",
      "name": "operator-provider",
      "type": "openai-compat",
      "base_url": "` + operator.srv.URL + `",
      "api_key": "sk-operator-secret",
      "discover_models": false,
      "models": [
        {"id": "operator-model", "name": "operator-model", "context_window": 200000, "default_max_tokens": 1000}
      ]
    }
  },
  "models": {
    "smart": {"provider": "operator-provider", "model": "operator-model"},
    "fast": {"provider": "operator-provider", "model": "operator-model"}
  }
}`)
	providerKtav, err := ktavcodec.ConvertJSONToKtav(providerJSON)
	require.NoError(t, err, "fixture JSON must convert to Ktav")
	require.NotEmpty(t, providerKtav)

	// Seed BOTH spellings in BOTH discoverable locations.
	workDir := filepath.Join(realTmp, "lib-workspace")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), providerJSON, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.ktav"), providerKtav, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "rush.json"), append([]byte(nil), providerJSON...), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "rush.ktav"), append([]byte(nil), providerKtav...), 0o600))

	// Open the library-mode client and prove it still serves a run.
	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		WorkingDir:    workDir,
		LibraryConfig: libraryConfigFor(library.srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	res := runLibraryPrompt(t, client, "")
	require.Equal(t, "LIBRARY_MODE_STILL_WORKS", res.FinalText)
	require.NotEmpty(t, library.hits(),
		"the LibraryConfig provider must receive the requests")

	// The operator provider saw zero requests — meaningful now, because
	// every seeded config file (both spellings, both locations) names
	// the operator as its only provider.
	require.Equal(t, 0, operator.totalRequests(),
		"library mode must not read rush.json/rush.ktav even with auto-migrate on")

	// All four seeded files' bytes are unchanged.
	seeded := map[string][]byte{
		filepath.Join(workDir, "rush.json"): providerJSON,
		filepath.Join(workDir, "rush.ktav"): providerKtav,
		filepath.Join(home, "rush.json"):    providerJSON,
		filepath.Join(home, "rush.ktav"):    providerKtav,
	}
	for path, want := range seeded {
		got, readErr := os.ReadFile(path)
		require.NoError(t, readErr, "%s must remain readable", path)
		require.Equal(t, string(want), string(got),
			"library mode must not rewrite or migrate %s", path)
	}

	// No new config entries beyond the seeded spellings: only the four
	// seeded files and the library-mode .rush data directory are allowed.
	for _, dir := range []string{workDir, home} {
		entries, lsErr := os.ReadDir(dir)
		require.NoError(t, lsErr)
		for _, e := range entries {
			name := e.Name()
			switch name {
			case "rush.json", "rush.ktav", ".rush":
				continue
			default:
				require.Failf(t, "unexpected entry in config dir",
					"%s appeared in %s (GOOS %s)", name, dir, runtime.GOOS)
			}
		}
	}

	// Each forbidden migration artifact, checked explicitly.
	for _, forbidden := range []string{
		"rush.json.bak",
		"rush.json.ktav-migration",
		"rush.ktav.bak",
		"rush.ktav.ktav-migration",
	} {
		_, statErr := os.Stat(filepath.Join(workDir, forbidden))
		require.ErrorIs(t, statErr, os.ErrNotExist, "%s must not appear", forbidden)
		_, statErr = os.Stat(filepath.Join(home, forbidden))
		require.ErrorIs(t, statErr, os.ErrNotExist, "%s in home must not appear", forbidden)
	}
}
