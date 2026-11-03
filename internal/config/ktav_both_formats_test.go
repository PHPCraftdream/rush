package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

func setupBothFormats(t *testing.T) (string, string, string) {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	t.Setenv(GlobalDirEnv, t.TempDir())
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	ktav := filepath.Join(dir, "rush.ktav")
	writeTestFile(t, legacy, `{"options":{"debug":true}}`)
	require.NoError(t, os.WriteFile(ktav, testKtavBytes(t, `{"options":{"debug":false}}`), 0o600))
	resetBothFormatsWarnings()
	return dir, ktav, legacy
}

func TestBothFormatsKtavNewerLogsDebugOnly(t *testing.T) {
	_, ktav, legacy := setupBothFormats(t)
	base := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(legacy, base, base))
	require.NoError(t, os.Chtimes(ktav, base.Add(time.Second), base.Add(time.Second)))
	logs := captureDebugSlogOutput(t)
	selectConfigSlot(legacy)
	require.Contains(t, logs.String(), "level=DEBUG")
	require.NotContains(t, logs.String(), "level=WARN")
}

func TestBothFormatsEqualMtimeLogsDebug(t *testing.T) {
	_, ktav, legacy := setupBothFormats(t)
	stamp := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(legacy, stamp, stamp))
	require.NoError(t, os.Chtimes(ktav, stamp, stamp))
	logs := captureDebugSlogOutput(t)
	selectConfigSlot(legacy)
	require.Contains(t, logs.String(), "level=DEBUG")
	require.NotContains(t, logs.String(), "level=WARN")
}

func TestBothFormatsNewerJSONWarnsOnceAcrossDiscovery(t *testing.T) {
	_, ktav, legacy := setupBothFormats(t)
	base := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(ktav, base, base))
	require.NoError(t, os.Chtimes(legacy, base.Add(time.Second), base.Add(time.Second)))
	logs := captureDebugSlogOutput(t)
	selectConfigSlot(legacy)
	selectConfigSlot(legacy)
	require.Equal(t, 1, strings.Count(logs.String(), "level=WARN"))
	require.Contains(t, logs.String(), "rush.json changed after rush.ktav (by hand or an older rush); its edits are ignored")
}

func TestBothFormatsCacheLogsDebug(t *testing.T) {
	_, ktav, legacy := setupBothFormats(t)
	base := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(legacy, base.Add(time.Second), base.Add(time.Second)))
	require.NoError(t, os.Chtimes(ktav, base, base))
	logs := captureDebugSlogOutput(t)
	_, err := readCacheDocument(legacy)
	require.NoError(t, err)
	require.Contains(t, logs.String(), "level=DEBUG")
	require.NotContains(t, logs.String(), "level=WARN")
}

func TestBothFormatsDefaultSlogSuppressesHealthyDebug(t *testing.T) {
	_, ktav, legacy := setupBothFormats(t)
	base := time.Unix(1_700_000_000, 0)
	require.NoError(t, os.Chtimes(legacy, base, base))
	require.NoError(t, os.Chtimes(ktav, base.Add(time.Second), base.Add(time.Second)))

	self, err := os.Executable()
	require.NoError(t, err)
	cmd := platform.Command(t.Context(), self, "-test.run=^TestBothFormatsDefaultSlogHelper$")
	cmd.Env = isolatedBothFormatsHelperEnv(legacy, t.TempDir(), t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "helper stdout: %s", stdout.String())
	require.Empty(t, stderr.String(), "healthy Debug must be suppressed by the actual default handler")
}

func isolatedBothFormatsHelperEnv(configFile, dataDir, configDir string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR", "RUSH_BOTH_FORMATS_HELPER", "RUSH_BOTH_FORMATS_CONFIG":
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"RUSH_BOTH_FORMATS_HELPER=1",
		"RUSH_BOTH_FORMATS_CONFIG="+configFile,
		"RUSH_GLOBAL_DATA="+dataDir,
		"RUSH_GLOBAL_CONFIG="+configDir,
		"RUSH_GLOBAL_DIR="+dataDir,
	)
}

func TestBothFormatsDefaultSlogHelper(t *testing.T) {
	if os.Getenv("RUSH_BOTH_FORMATS_HELPER") != "1" {
		return
	}
	selectConfigSlot(os.Getenv("RUSH_BOTH_FORMATS_CONFIG"))
}
