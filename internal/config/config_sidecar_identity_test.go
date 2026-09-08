package config

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestDarwinVolumeCapabilityParser(t *testing.T) {
	buffer := make([]byte, configVolumeCapabilityAttributeSize)
	binary.LittleEndian.PutUint32(buffer[:4], configVolumeCapabilityAttributeSize)
	binary.LittleEndian.PutUint32(buffer[20:24], configVolumeCapabilityCaseSensitive)
	caseInsensitive, known := configParseDarwinVolumeCapabilities(buffer)
	require.True(t, known)
	require.True(t, caseInsensitive)

	binary.LittleEndian.PutUint32(buffer[4:8], configVolumeCapabilityCaseSensitive)
	caseInsensitive, known = configParseDarwinVolumeCapabilities(buffer)
	require.True(t, known)
	require.False(t, caseInsensitive)

	binary.LittleEndian.PutUint32(buffer[20:24], 0)
	caseInsensitive, known = configParseDarwinVolumeCapabilities(buffer)
	require.False(t, known)
	require.False(t, caseInsensitive)
}

func TestConfigAliasChainFingerprintIgnoresRegularLeafMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"first":true}`), 0o644))
	before := configAliasChainFingerprint(path)
	require.NoError(t, os.Chmod(path, 0o600))
	after := configAliasChainFingerprint(path)
	require.Equal(t, before, after)
}

func TestConfigWriteTargetDedupKeyUsesExistingFileIdentity(t *testing.T) {
	identity := configFileIdentity{device: 11, inode: 22, valid: true}
	left := configWriteTarget{expected: reloadFileFingerprint{exists: true, identity: identity}}
	right := configWriteTarget{expected: reloadFileFingerprint{exists: true, identity: identity}}
	different := configWriteTarget{expected: reloadFileFingerprint{
		exists: true, identity: configFileIdentity{device: 11, inode: 23, valid: true},
	}}

	require.Equal(t, configWriteTargetDedupKey(left), configWriteTargetDedupKey(right))
	require.NotEqual(t, configWriteTargetDedupKey(left), configWriteTargetDedupKey(different))
}

func TestConfigWriteTargetDedupKeyUsesExistingSidecarIdentity(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "rush.json.lock")
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	parent := configParentIdentity(filepath.Join(root, "rush.json"))
	target := configWriteTarget{
		path: filepath.Join(root, "rush.json"), lockPath: lockPath,
		selectedPath: filepath.Join(root, "rush.json"),
		expected:     reloadFileFingerprint{parentIdentity: parent},
	}
	key := configWriteTargetDedupKey(target)
	require.Contains(t, key, "sidecar:")
}

func TestConfigWriteTargetMissingLeafCaseCapability(t *testing.T) {
	root := t.TempDir()
	before := directoryEntryNames(t, root)
	parent := configParentIdentity(filepath.Join(root, "first.json"))
	if !parent.valid {
		t.Skip("parent identity unavailable")
	}
	left := configWriteTarget{
		path: filepath.Join(root, "Rush.json"), selectedPath: filepath.Join(root, "Rush.json"),
		expected: reloadFileFingerprint{parentIdentity: parent},
	}
	right := left
	right.path = filepath.Join(root, "rush.json")
	right.selectedPath = right.path
	if configDirectoryIsCaseInsensitive(root) {
		require.Equal(t, configWriteTargetDedupKey(left), configWriteTargetDedupKey(right))
	} else {
		require.NotEqual(t, configWriteTargetDedupKey(left), configWriteTargetDedupKey(right))
	}
	require.Equal(t, before, directoryEntryNames(t, root))
}

func TestMissingTargetIdentityChecksAreDiskless(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "Rush.json")
	lower := filepath.Join(root, "rush.json")
	for _, path := range []string{upper + ".lock", lower + ".lock"} {
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: upper,
		workspacePath:  lower,
	})
	store.workingDir = root
	before := directoryEntryNames(t, root)

	target, err := store.resolveConfigWriteTarget(upper)
	require.NoError(t, err)
	_ = configWriteTargetDedupKey(target)
	require.NoError(t, store.withMCPAdmissionLocks(func(*mcpLockedFiles) error { return nil }))

	require.Equal(t, before, directoryEntryNames(t, root))
}

func TestConfigWriteTargetPermissionTransitionSupportsFreshRMW(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rush.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"existing":true}`), 0o644))
	store1 := newTestConfigStore(testStoreOpts{globalDataPath: path})
	store2 := newTestConfigStore(testStoreOpts{globalDataPath: path})
	target1, err := store1.resolveConfigWriteTarget(path)
	require.NoError(t, err)
	target2, err := store2.resolveConfigWriteTarget(path)
	require.NoError(t, err)

	lock1, err := acquireConfigFileLock(nil, target1.lockPath)
	require.NoError(t, err)
	_, err = commitConfigFile(target1.selectedPath, target1.path, []byte(`{"existing":true,"first":true}`), 0o600,
		target1.expected, target1.owner, target1.enforce)
	require.NoError(t, err)
	require.NoError(t, lock1.Release())

	lock2, err := acquireConfigFileLock(nil, target2.lockPath)
	require.NoError(t, err)
	defer lock2.Release()
	require.NoError(t, verifyConfigTarget(target2))
	data, expected, err := readStableConfigFileOwned(target2.selectedPath, target2.owner, target2.enforce)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(data, &document))
	document["second"] = true
	data, err = json.Marshal(document)
	require.NoError(t, err)
	_, err = commitConfigFile(target2.selectedPath, target2.path, data, 0o600, expected, target2.owner, target2.enforce)
	require.NoError(t, err)

	final, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(final), `"first":true`)
	require.Contains(t, string(final), `"second":true`)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestMCPAdmissionDeduplicatesCaseSpellingsWhenFilesystemDoes(t *testing.T) {
	root := t.TempDir()
	if !configDirectoryIsCaseInsensitive(root) {
		t.Skip("filesystem is case-sensitive")
	}
	upper := filepath.Join(root, "Rush.json")
	lower := filepath.Join(root, "rush.json")
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: upper,
		workspacePath:  lower,
	})
	store.workingDir = root

	acquisitions := 0
	configTestHooks.Lock()
	previous := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*session.FileLock, error) {
		acquisitions++
		return session.AcquireFileLockContext(ctx, path)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previous
		configTestHooks.Unlock()
	})

	require.NoError(t, store.withMCPAdmissionLocks(func(files *mcpLockedFiles) error {
		require.Len(t, files.records, 1)
		return nil
	}))
	require.Equal(t, 1, acquisitions)
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "server", MCPConfig{
		Type: MCPHttp, URL: "http://case-alias.example",
	}))
	data, err := os.ReadFile(upper)
	require.NoError(t, err)
	require.Contains(t, string(data), "case-alias.example")
}

func directoryEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
