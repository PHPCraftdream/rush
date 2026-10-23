// Ktav logical-slot discovery tests: slot selection matrix, unchanged merge
// order across mixed formats, raw-byte fingerprints, ownership rejection,
// the (D) .ktav write guard, deferred migration on EACCES, and the empty
// (Windows) system slot.
package config

import (
	"bytes"
	"crypto/sha256"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

const ktavTestSecret = "sk-SUPERSECRET-ktav-42"

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func testKtavBytes(t *testing.T, jsonSrc string) []byte {
	t.Helper()
	ktav, err := ktavcodec.ConvertJSONToKtav([]byte(jsonSrc))
	require.NoError(t, err)
	return ktav
}

func captureSlogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func resetBothFormatsWarnings() {
	ktavBothFormatsWarned = sync.Map{}
}

func indexOfPath(paths []string, needle string) int {
	for i, p := range paths {
		if p == needle {
			return i
		}
	}
	return -1
}

func mustDecodeTestKtav(t *testing.T, raw []byte) []byte {
	t.Helper()
	tree, err := ktavcodec.DecodeKtav(raw)
	require.NoError(t, err)
	jsonBytes, err := ktavcodec.ToJSON(tree)
	require.NoError(t, err)
	return jsonBytes
}

func TestKtavSlotSelectionMatrix(t *testing.T) {
	ktavIsolateGlobals(t)
	jsonBody := `{"providers":{"p":{"api_key":"json-value"}}}`
	ktavBody := `{"providers":{"p":{"api_key":"ktav-value"}}}`

	t.Run("both exist selects ktav and warns with paths only", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "rush.json"), jsonBody)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "rush.ktav"), testKtavBytes(t, ktavBody), 0o600))
		resetBothFormatsWarnings()
		logs := captureSlogOutput(t)
		require.Equal(t, filepath.Join(dir, "rush.ktav"), selectConfigSlot(filepath.Join(dir, "rush.json")))
		require.Contains(t, logs.String(), "Both config formats exist")
		require.Contains(t, logs.String(), filepath.Join(dir, "rush.ktav"))
		require.Contains(t, logs.String(), filepath.Join(dir, "rush.json"))
		require.NotContains(t, logs.String(), "ktav-value")
		require.NotContains(t, logs.String(), "json-value")
	})

	t.Run("ktav only selects ktav", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "rush.ktav"), testKtavBytes(t, ktavBody), 0o600))
		require.Equal(t, filepath.Join(dir, "rush.ktav"), selectConfigSlot(filepath.Join(dir, "rush.json")))
	})

	t.Run("json only selects json", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "rush.json"), jsonBody)
		require.Equal(t, filepath.Join(dir, "rush.json"), selectConfigSlot(filepath.Join(dir, "rush.json")))
	})

	t.Run("bak only is not restored", func(t *testing.T) {
		dir := t.TempDir()
		// The retained backup of rush.json is rush.json.bak (never
		// rush.json.json.bak).
		writeTestFile(t, filepath.Join(dir, "rush.json.bak"), jsonBody)
		require.Equal(t, filepath.Join(dir, "rush.json"), selectConfigSlot(filepath.Join(dir, "rush.json")))
		require.Equal(t, filepath.Join(dir, "rush.json.bak"), backupPathFor(filepath.Join(dir, "rush.json")))
		err := checkBackupOnlySlot(filepath.Join(dir, "rush.json"))
		require.ErrorIs(t, err, errKtavBackupOnly)
		require.Contains(t, err.Error(), "rush.json.bak")
	})

	t.Run("hidden backup spelling is recognized", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, ".rush.json.bak"), jsonBody)
		require.Equal(t, filepath.Join(dir, ".rush.json.bak"), backupPathFor(filepath.Join(dir, ".rush.json")))
		require.ErrorIs(t, checkBackupOnlySlot(filepath.Join(dir, ".rush.json")), errKtavBackupOnly)
	})

	t.Run("invalid ktav fails closed without JSON fallback", func(t *testing.T) {
		dir := t.TempDir()
		legacy := filepath.Join(dir, "rush.json")
		ktavPath := filepath.Join(dir, "rush.ktav")
		writeTestFile(t, legacy, jsonBody)
		// The invalid bytes CARRY the secret: the diagnostic must not echo
		// the parser input.
		require.NoError(t, os.WriteFile(ktavPath, []byte("not ktav at all {{{ "+ktavTestSecret), 0o600))
		require.Equal(t, ktavPath, selectConfigSlot(legacy))
		logs := captureSlogOutput(t)
		_, _, _, _, err := loadConfigCandidateStable([]string{ktavPath})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid Ktav")
		require.NotContains(t, err.Error(), ktavTestSecret)
		require.NotContains(t, logs.String(), ktavTestSecret)
	})
	t.Run("backup-only hidden ancestor blocks visible ancestor fallback", func(t *testing.T) {
		root := t.TempDir()
		working := filepath.Join(root, "child", "project")
		_, err := exec.LookPath("git")
		require.NoError(t, err)
		gitHome := t.TempDir()
		t.Setenv("HOME", gitHome)
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(gitHome, ".gitconfig"))
		gitInit := platform.Command(t.Context(), "git", "init", "-q")
		gitInit.Dir = root
		require.NoError(t, gitInit.Run())
		require.NoError(t, os.MkdirAll(working, 0o755))
		boundary := projectBoundary(working)
		require.Equal(t, normalizeDiscoveryPath(root), normalizeDiscoveryPath(boundary))
		backup := filepath.Join(root, ".rush.json.bak")
		writeTestFile(t, backup, `{"options":{"debug":true}}`)
		err = checkAuthoritativeBackupOnly(working)
		require.ErrorIs(t, err, errKtavBackupOnly)
		require.NoError(t, os.Remove(backup))
		visible := filepath.Join(root, "rush.json")
		writeTestFile(t, visible, `{"options":{"debug":true}}`)
		paths := lookupConfigCandidates(working)
		require.Contains(t, paths, normalizeDiscoveryPath(visible))
		require.Equal(t, visible, selectConfigSlot(visible))
		require.NoError(t, checkAuthoritativeBackupOnly(working))
	})
}

func TestKtavEmptySystemSlotNeverProbed(t *testing.T) {
	ktavIsolateGlobals(t)
	// Windows has no system config: systemConfigPath is "" and must never
	// probe a relative ".ktav" next to the process CWD.
	require.Equal(t, "", selectConfigSlot(""))
	// On Windows systemConfigPath is empty; elsewhere it is absolute.
	require.Equal(t, runtime.GOOS == "windows", systemConfigPath == "")
	dir := t.TempDir()
	for _, path := range lookupConfigCandidates(dir) {
		require.True(t, filepath.IsAbs(path), "discovery produced a relative candidate: %s", path)
	}
}

func TestKtavMergeOrderUnchangedMixedFormats(t *testing.T) {
	configDir, dataDir := isolateAllGlobalConfigPaths(t)
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	project := t.TempDir()

	// GlobalConfig() resolves <RUSH_GLOBAL_CONFIG>/rush.json — write the
	// Ktav sibling at exactly the location discovery selects.
	globalJSON := filepath.Join(configDir, "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(globalJSON), 0o700))
	require.NoError(t, os.WriteFile(ktavSiblingOf(globalJSON),
		testKtavBytes(t, `{"providers":{"p":{"api_key":"global-ktav"}}}`), 0o600))
	writeTestFile(t, filepath.Join(project, "rush.json"),
		`{"providers":{"p":{"api_key":"project-json","base_url":"https://p.example"}}}`)

	paths := lookupConfigCandidates(project)
	require.Contains(t, paths, normalizeDiscoveryPath(ktavSiblingOf(globalJSON)))
	require.Contains(t, paths, normalizeDiscoveryPath(filepath.Join(project, "rush.json")))
	// Discovery order is lowest priority first: global before project.
	require.Less(t,
		indexOfPath(paths, normalizeDiscoveryPath(ktavSiblingOf(globalJSON))),
		indexOfPath(paths, normalizeDiscoveryPath(filepath.Join(project, "rush.json"))))

	cfg, loaded, _, _, err := loadConfigCandidateStable(paths)
	require.NoError(t, err)
	provider, ok := cfg.Providers.Get("p")
	require.True(t, ok)
	// Project merges LAST, so its value wins — order is unchanged.
	require.Equal(t, "project-json", provider.APIKey)
	require.Contains(t, loaded, normalizeDiscoveryPath(ktavSiblingOf(globalJSON)))
	require.NotContains(t, loaded, normalizeDiscoveryPath(filepath.Join(dataDir, "rush.json")))
}

func TestKtavVisibleBeatsHiddenAcrossFormats(t *testing.T) {
	ktavIsolateGlobals(t)
	cases := []struct {
		name        string
		visibleFile string
		visibleBody string
		hiddenFile  string
		hiddenBody  string
		wantValue   string
	}{
		{
			name:        "visible json beats hidden ktav",
			visibleFile: "rush.json", visibleBody: `{"providers":{"p":{"api_key":"visible-json"}}}`,
			hiddenFile: ".rush.ktav", hiddenBody: `{"providers":{"p":{"api_key":"hidden-ktav"}}}`,
			wantValue: "visible-json",
		},
		{
			name:        "visible ktav beats hidden json",
			visibleFile: "rush.ktav", visibleBody: `{"providers":{"p":{"api_key":"visible-ktav"}}}`,
			hiddenFile: ".rush.json", hiddenBody: `{"providers":{"p":{"api_key":"hidden-json"}}}`,
			wantValue: "visible-ktav",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if strings.HasSuffix(tc.visibleFile, ".json") {
				writeTestFile(t, filepath.Join(dir, tc.visibleFile), tc.visibleBody)
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(dir, tc.visibleFile), testKtavBytes(t, tc.visibleBody), 0o600))
			}
			if strings.HasSuffix(tc.hiddenFile, ".json") {
				writeTestFile(t, filepath.Join(dir, tc.hiddenFile), tc.hiddenBody)
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(dir, tc.hiddenFile), testKtavBytes(t, tc.hiddenBody), 0o600))
			}

			paths := lookupConfigCandidates(dir)
			visibleIdx := indexOfPath(paths, normalizeDiscoveryPath(filepath.Join(dir, tc.visibleFile)))
			hiddenIdx := indexOfPath(paths, normalizeDiscoveryPath(filepath.Join(dir, tc.hiddenFile)))
			require.Greater(t, visibleIdx, hiddenIdx,
				"the visible file must merge LAST (highest priority), preserving historical precedence")
			cfg, _, _, _, err := loadConfigCandidateStable(paths)
			require.NoError(t, err)
			provider, ok := cfg.Providers.Get("p")
			require.True(t, ok)
			require.Equal(t, tc.wantValue, provider.APIKey)
		})
	}
}

func TestKtavFingerprintIsRawBytes(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	raw := testKtavBytes(t, `{"providers":{"p":{"api_key":"`+ktavTestSecret+`"}}}`)
	require.NoError(t, os.WriteFile(ktavPath, raw, 0o600))

	_, _, fingerprints, _, err := loadConfigCandidateStable([]string{ktavPath})
	require.NoError(t, err)
	fp := fingerprints[ktavPath]
	require.True(t, fp.exists)
	require.Equal(t, sha256.Sum256(raw), fp.digest)
	require.Equal(t, int64(len(raw)), fp.size)
	require.NotEqual(t, fp.digest, sha256.Sum256(mustDecodeTestKtav(t, raw)))
}

func TestKtavWorkspacePathPrefersKtav(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	require.Equal(t, filepath.Join(dir, "rush.json"), workspaceConfigPath(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rush.ktav"), testKtavBytes(t, `{}`), 0o600))
	require.Equal(t, filepath.Join(dir, "rush.ktav"), workspaceConfigPath(dir))
}

func TestKtavWriteGuardNeverModifiesKtavTarget(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	raw := testKtavBytes(t, `{"providers":{"p":{"api_key":"untouched"}}}`)
	require.NoError(t, os.WriteFile(ktavPath, raw, 0o600))

	store := &ConfigStore{workingDir: dir, globalDataPath: ktavPath}

	err := store.SetConfigFields(ScopeGlobal, map[string]any{"providers.p.api_key": "rewritten"})
	require.ErrorIs(t, err, errKtavWriteNotSupported)
	err = store.RemoveConfigField(ScopeGlobal, "providers.p.api_key")
	require.ErrorIs(t, err, errKtavWriteNotSupported)

	onDisk, readErr := os.ReadFile(ktavPath)
	require.NoError(t, readErr)
	require.Equal(t, string(raw), string(onDisk))
	require.NotContains(t, string(onDisk), "rewritten")
}

func TestKtavWriteGuardErrorNamesTheFile(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, `{}`), 0o600))
	err := ensureKtavWriteSupported(ktavPath)
	require.ErrorIs(t, err, errKtavWriteNotSupported)
	require.Contains(t, err.Error(), "rush.ktav")
	require.Contains(t, err.Error(), "not supported")
}

func TestKtavOwnershipRejectionWithoutJSONFallback(t *testing.T) {
	ktavIsolateGlobals(t)
	if runtime.GOOS == "windows" {
		root := t.TempDir()
		ktav := filepath.Join(root, "rush.ktav")
		require.NoError(t, os.WriteFile(ktav, testKtavBytes(t, `{"providers":{"p":{"api_key":"windows-owned"}}}`), 0o600))
		cfg, loaded, _, _, err := loadConfigCandidateStable([]string{ktav})
		require.NoError(t, err)
		require.Contains(t, loaded, normalizeDiscoveryPath(ktav))
		provider, ok := cfg.Providers.Get("p")
		require.True(t, ok)
		require.Equal(t, "windows-owned", provider.APIKey)
		return
	}
	root := t.TempDir()
	ktav := filepath.Join(root, "rush.ktav")
	writeTestFile(t, filepath.Join(root, "rush.json"), `{"providers":{"p":{"api_key":"json"}}}`)
	require.NoError(t, os.WriteFile(ktav, testKtavBytes(t, `{"providers":{"p":{"api_key":"foreign"}}}`), 0o600))

	owner, err := fsext.Owner(root)
	require.NoError(t, err)
	// The rejection predicate itself: a candidate whose ACTUAL owner differs
	// from the slot's expected owner is dropped. No skip: this assertion
	// holds in every unprivileged environment.
	require.Equal(t, "", eligibleConfigCandidate(ktav, owner+1))
	// The discovery-level property when chown IS available: a foreign-owned
	// Ktav is rejected and the slot must not silently downgrade to JSON.
	foreignOwner := 0
	if owner == foreignOwner {
		foreignOwner = 1
	}
	foreignOwned := false
	if chownErr := os.Chown(ktav, foreignOwner, -1); chownErr == nil {
		foreignOwned = true
		candidates := lookupConfigCandidates(root)
		require.NotContains(t, candidates, normalizeDiscoveryPath(ktav))
		require.NotContains(t, candidates, normalizeDiscoveryPath(filepath.Join(root, "rush.json")))
		_, _, _, _, loadErr := loadConfigCandidateStableForWorkingDir([]string{ktav}, root)
		require.Error(t, loadErr)
	} else {
		require.Equal(t, "", eligibleConfigCandidate(ktav, foreignOwner))
	}
	// The opened-handle reader check rejects a foreign owner; this is
	// independent of whether chown capability is available.
	policyOwner, enforce, policyErr := configOwnerPolicyForPath(ktav, root)
	require.NoError(t, policyErr)
	require.True(t, enforce)
	require.Equal(t, owner, policyOwner)
	if foreignOwned {
		_, _, _, _, loadErr := loadConfigCandidateStableForWorkingDir([]string{ktav}, root)
		require.Error(t, loadErr)
	}
}
