package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

// lockKtavFixtureJSON is a minimal valid settings document with one custom
// provider (base_url + models); config.Load errors with "no custom
// providers" when defaults are disabled, so the fixture carries one.
const lockKtavFixtureJSON = `{
  "providers": {
    "ktav-p": {
      "name": "Ktav P",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.openai.com/v1",
      "models": [{"id": "gpt-4o"}]
    }
  }
}`

// TestLock_KtavSlotFileNameInOutput pins the user-visible settings-file
// spelling in `rush lock` output to the ACTIVE file of the slot, derived at
// runtime via config.ActiveConfigFileName: "rush.json" when no Ktav sibling
// exists (byte-identical with today's gate-off default) and "rush.ktav"
// when the Ktav sibling exists on disk.
func TestLock_KtavSlotFileNameInOutput(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	defer restore()

	t.Run("gate_off_no_ktav_names_rush_json", func(t *testing.T) {
		// NOT parallel: ktavIsolateGlobalEnv uses t.Setenv.
		ktavIsolateGlobalEnv(t) // last write wins: fills HOME/USERPROFILE gaps
		_, workspace, dataDir := isolateLockEnv(t)
		out, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "pw")
		require.NoError(t, err)
		// Byte-identical pin: the legacy full path, exactly as before the
		// Ktav-aware derivation.
		require.Contains(t, out, filepath.Join(dataDir, "rush.json"))
		require.NotContains(t, out, "rush.ktav")
	})

	t.Run("ktav_sibling_selected_names_rush_ktav", func(t *testing.T) {
		if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			// The alternative spelling assertion is only meaningful where
			// the slot selection runs; on other platforms assert the
			// gate-off alternative instead of skipping.
			t.Run("gate_off_fallback", func(t *testing.T) {
				_, workspace, dataDir := isolateLockEnv(t)
				out, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "pw")
				require.NoError(t, err)
				require.Contains(t, out, "rush.json")
			})
			return
		}
		ktavIsolateGlobalEnv(t) // last write wins: fills HOME/USERPROFILE gaps
		_, workspace, dataDir := isolateLockEnv(t)
		ktavDoc, err := ktavcodec.ConvertJSONToKtav([]byte(lockKtavFixtureJSON))
		require.NoError(t, err)
		// os.WriteFile closes its handle before t.TempDir cleanup (Windows).
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.ktav"), ktavDoc, 0o600))
		// Resolve the real on-disk path so the comparison below is robust
		// to symlinked temp dirs (macOS /tmp, Windows 8.3 names).
		evalDataDir, err := filepath.EvalSymlinks(dataDir)
		require.NoError(t, err)
		out, _, err := runLockTree(t, workspace, dataDir, "lock", "--local", "pw")
		require.NoError(t, err)
		require.Contains(t, out, "rush.ktav")
		require.NotContains(t, out, "rush.json")
		require.True(t, strings.Contains(out, filepath.Join(evalDataDir, "rush.ktav")),
			"expected the printed active slot file %s in output:\n%s", filepath.Join(evalDataDir, "rush.ktav"), out)
	})
}
