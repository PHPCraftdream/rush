package sdk_test

// KTAV-4b SDK chunk: library mode MUST stay diskless with respect to
// configuration. openLibrary (sdk/library_mode.go) builds its Config via
// buildLibraryConfig + config.NewLibraryStore and never calls config.Load,
// config.Init, or the Ktav migration — even when the package-global
// ktavAutoMigrate gate (config.SetKtavAutoMigrateForTest) is forced ON.
//
// This test seeds temp dirs with a rush.json, opens a library-mode client
// with the migration gate flipped on, and asserts that no config file is
// read, migrated, or created: the rush.json bytes are unchanged, and no
// rush.ktav / *.bak / *.ktav-migration appears in any temp dir. The
// rush.json-selected provider receives zero requests (the same pin as
// library_mode_test.go's TestOpenLibraryMode_IgnoresRushJSON).

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// TestOpenLibraryMode_NeverMigratesOrReadsConfigFiles pins library mode's
// zero-config-I/O contract even with Ktav auto-migration enabled.
func TestOpenLibraryMode_NeverMigratesOrReadsConfigFiles(t *testing.T) {
	// NOT parallel: SetKtavAutoMigrateForTest mutates a package-global.
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	isolateGlobalConfigForWorkdirTest(t)

	realTmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	// A private "home" so a stray global-config read/write lands somewhere
	// observable. HOME/USERPROFILE both point at it (Windows honors
	// USERPROFILE, Unix honors HOME).
	home := filepath.Join(realTmp, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Seed BOTH observable dirs with a rush.json whose bytes we capture.
	workDir := filepath.Join(realTmp, "lib-workspace")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	seedRushJSON := []byte(`{"disable_default_providers":false,"providers":{},"models":{}}`)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), seedRushJSON, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "rush.json"), seedRushJSON, 0o600))
	homeRushJSON := append([]byte(nil), seedRushJSON...)

	// The provider rush.json would select must receive zero requests.
	operator := newCredentialServer(t, "RUSH_JSON_MUST_NOT_BE_HIT")
	library := newCredentialServer(t, "LIBRARY_MODE_STILL_WORKS")

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
	require.NotEmpty(t, library.hits())

	// The rush.json provider saw zero requests.
	require.Equal(t, 0, operator.totalRequests(),
		"library mode must not read rush.json even with auto-migrate on")

	// rush.json bytes unchanged in the working dir.
	got, readErr := os.ReadFile(filepath.Join(workDir, "rush.json"))
	require.NoError(t, readErr)
	require.Equal(t, string(seedRushJSON), string(got),
		"library mode must not rewrite or migrate the workspace rush.json")

	// No new config file appears in the working dir: only the seeded
	// rush.json and the library-mode .rush data directory are allowed.
	entries, lsErr := os.ReadDir(workDir)
	require.NoError(t, lsErr)
	for _, e := range entries {
		name := e.Name()
		switch name {
		case "rush.json", ".rush":
			continue
		default:
			require.Failf(t, "unexpected entry in working dir",
				"%s appeared in %s (GOOS %s)", name, workDir, runtime.GOOS)
		}
	}

	// Each forbidden config spelling, checked explicitly.
	for _, forbidden := range []string{
		"rush.ktav",
		"rush.json.bak",
		"rush.json.ktav-migration",
	} {
		_, statErr := os.Stat(filepath.Join(workDir, forbidden))
		require.ErrorIs(t, statErr, os.ErrNotExist, "%s must not appear", forbidden)
		_, statErr = os.Stat(filepath.Join(home, forbidden))
		require.ErrorIs(t, statErr, os.ErrNotExist, "%s in home must not appear", forbidden)
	}

	// The home rush.json is untouched too.
	gotHome, readErr := os.ReadFile(filepath.Join(home, "rush.json"))
	require.NoError(t, readErr)
	require.Equal(t, string(homeRushJSON), string(gotHome),
		"library mode must not touch the home rush.json")
}
