package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestDarwinPathconfCaseSensitivity(t *testing.T) {
	t.Parallel()
	require.Equal(t, configCaseSensitivityInsensitive, configDarwinCaseSensitivityFromPathconf(0, nil))
	require.Equal(t, configCaseSensitivitySensitive, configDarwinCaseSensitivityFromPathconf(1, nil))
	for _, value := range []int{-1, 2} {
		require.Equal(t, configCaseSensitivityUnknown, configDarwinCaseSensitivityFromPathconf(value, nil))
	}
	for _, value := range []int{-1, 0, 1} {
		require.Equal(t, configCaseSensitivityUnknown, configDarwinCaseSensitivityFromPathconf(value, os.ErrPermission))
	}
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
	root := normalizeReloadPath(t.TempDir())
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

func TestConfigCaseSensitivityMetadataIsFailClosed(t *testing.T) {
	require.Equal(t, configCaseSensitivityUnknown, configCaseSensitivityFromMetadata(false, false, false))
	require.Equal(t, configCaseSensitivitySensitive, configCaseSensitivityFromMetadata(false, true, false))
	require.Equal(t, configCaseSensitivityInsensitive, configCaseSensitivityFromMetadata(true, false, false))
	require.Equal(t, configCaseSensitivityInsensitive, configCaseSensitivityFromMetadata(false, true, true))
}

func TestConfigLinuxCaseSensitivityMetadata(t *testing.T) {
	tests := []struct {
		name     string
		fsType   uint64
		flags    uint32
		known    bool
		expected configCaseSensitivity
	}{
		{name: "FAT", fsType: configLinuxMSDOSSuperMagic, expected: configCaseSensitivityInsensitive},
		{name: "exFAT", fsType: configLinuxExFATSuperMagic, expected: configCaseSensitivityInsensitive},
		{name: "ext4 casefold", fsType: configLinuxExt4SuperMagic, flags: configLinuxFSCasefoldFlag, known: true, expected: configCaseSensitivityInsensitive},
		{name: "f2fs casefold", fsType: configLinuxF2FSSuperMagic, flags: configLinuxFSCasefoldFlag, known: true, expected: configCaseSensitivityInsensitive},
		{name: "ext4 sensitive", fsType: configLinuxExt4SuperMagic, known: true, expected: configCaseSensitivitySensitive},
		{name: "f2fs sensitive", fsType: configLinuxF2FSSuperMagic, known: true, expected: configCaseSensitivitySensitive},
		{name: "ext4 ioctl unknown", fsType: configLinuxExt4SuperMagic, expected: configCaseSensitivityUnknown},
		{name: "unknown filesystem", fsType: 0xfeed, known: true, expected: configCaseSensitivityUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.expected, configLinuxCaseSensitivityFromMetadata(test.fsType, test.flags, test.known))
		})
	}
}

func TestConfigFreeBSDCaseSensitivityMetadata(t *testing.T) {
	require.Equal(t, configCaseSensitivityInsensitive, configFreeBSDCaseSensitivityFromFilesystemName("msdosfs"))
	require.Equal(t, configCaseSensitivityUnknown, configFreeBSDCaseSensitivityFromFilesystemName("zfs"))
	require.Equal(t, configCaseSensitivityUnknown, configFreeBSDCaseSensitivityFromFilesystemName(""))
}

func TestConfigBehavioralCaseSensitivityProbe(t *testing.T) {
	identity := configFileIdentity{device: 7, inode: 11, valid: true}
	directory := configFileIdentity{device: 3, inode: 5, valid: true}
	snapshot := func(entries ...configCaseDirectoryEntry) configCaseDirectorySnapshot {
		return configCaseDirectorySnapshot{identity: directory, entries: entries}
	}
	entry := configCaseDirectoryEntry{name: "Rush.json", identity: identity}
	stable := []configCaseDirectorySnapshot{snapshot(entry), snapshot(entry), snapshot(entry)}

	t.Run("one entry resolves alternate", func(t *testing.T) {
		probed := make([]string, 0, 2)
		got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
			return snapshot(entry), nil
		}, func(path string) (configFileIdentity, error) {
			probed = append(probed, path)
			return identity, nil
		})
		require.Equal(t, configCaseSensitivityInsensitive, got)
		require.Equal(t, []string{filepath.Join("/config", "Rush.json"), filepath.Join("/config", "rush.json")}, probed)
	})

	t.Run("missing alternate is sensitive", func(t *testing.T) {
		probes := 0
		got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
			return snapshot(entry), nil
		}, func(string) (configFileIdentity, error) {
			probes++
			if probes == 1 {
				return identity, nil
			}
			return configFileIdentity{}, os.ErrNotExist
		})
		require.Equal(t, configCaseSensitivitySensitive, got)
	})

	t.Run("no ASCII probe is unknown", func(t *testing.T) {
		nonASCII := stable
		nonASCII[0] = snapshot(configCaseDirectoryEntry{name: "конфиг", identity: identity})
		nonASCII[1] = nonASCII[0]
		nonASCII[2] = nonASCII[0]
		got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
			return nonASCII[0], nil
		}, func(string) (configFileIdentity, error) {
			return identity, nil
		})
		require.Equal(t, configCaseSensitivityUnknown, got)
	})

	t.Run("case variant hardlinks are ambiguous", func(t *testing.T) {
		variant := configCaseDirectoryEntry{name: "rush.json", identity: identity}
		ambiguous := []configCaseDirectorySnapshot{snapshot(entry, variant), snapshot(entry, variant), snapshot(entry, variant)}
		reads := 0
		got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
			result := ambiguous[reads]
			reads++
			return result, nil
		}, func(string) (configFileIdentity, error) {
			return identity, nil
		})
		require.Equal(t, configCaseSensitivityUnknown, got)
	})

	t.Run("probe errors are unknown", func(t *testing.T) {
		got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
			return snapshot(entry), nil
		}, func(string) (configFileIdentity, error) {
			return identity, os.ErrPermission
		})
		require.Equal(t, configCaseSensitivityUnknown, got)
	})

	t.Run("probe exposes a raced variant", func(t *testing.T) {
		variant := configCaseDirectoryEntry{name: "rush.json", identity: identity}
		raced := false
		probe := func(string) (configFileIdentity, error) {
			raced = true
			return identity, nil
		}
		got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
			if raced {
				return snapshot(entry, variant), nil
			}
			return snapshot(entry), nil
		}, probe)
		require.Equal(t, configCaseSensitivityUnknown, got)
	})

	t.Run("probe observes post-probe ABA metadata", func(t *testing.T) {
		changed := snapshot(entry)
		changed.modTime = 1
		probed := false
		got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
			if probed {
				return changed, nil
			}
			return snapshot(entry), nil
		}, func(string) (configFileIdentity, error) {
			probed = true
			return identity, nil
		})
		require.Equal(t, configCaseSensitivityUnknown, got)
	})

	t.Run("probe observes directory metadata race", func(t *testing.T) {
		changed := snapshot(entry)
		changed.modTime = 1
		reads := []configCaseDirectorySnapshot{snapshot(entry), changed, snapshot(entry)}
		read := 0
		got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
			result := reads[read]
			read++
			return result, nil
		}, func(string) (configFileIdentity, error) {
			return identity, nil
		})
		require.Equal(t, configCaseSensitivityUnknown, got)
	})
}

func TestConfigCaseDirectoryProbeBudget(t *testing.T) {
	boundary := make([]string, configCaseDirectoryMaxEntries)
	for i := range boundary {
		boundary[i] = "x"
	}
	require.False(t, configCaseDirectoryBudgetExceeded(boundary))
	boundary = append(boundary, "x")
	require.True(t, configCaseDirectoryBudgetExceeded(boundary))
	require.False(t, configCaseDirectoryBudgetExceeded([]string{strings.Repeat("x", configCaseDirectoryMaxNameBytes)}))
	require.True(t, configCaseDirectoryBudgetExceeded([]string{strings.Repeat("x", configCaseDirectoryMaxNameBytes+1)}))

	probeCalls := 0
	got := configCaseSensitivityFromDirectorySnapshots("/config", func() (configCaseDirectorySnapshot, error) {
		return configCaseDirectorySnapshot{}, errConfigCaseDirectoryBudgetExceeded
	}, func(string) (configFileIdentity, error) {
		probeCalls++
		return configFileIdentity{}, os.ErrPermission
	})
	require.Equal(t, configCaseSensitivityUnknown, got)
	require.Zero(t, probeCalls)
}

func TestConfigWriteTargetMissingLeafUsesInjectedCaseDecision(t *testing.T) {
	root := t.TempDir()
	parent := configParentIdentity(filepath.Join(root, "first.json"))
	require.True(t, parent.valid)
	left := configWriteTarget{
		path: filepath.Join(root, "Rush.json"), lockPath: filepath.Join(root, "Rush.json.lock"),
		selectedPath: filepath.Join(root, "Rush.json"),
		expected:     reloadFileFingerprint{parentIdentity: parent},
	}
	right := left
	right.path = filepath.Join(root, "rush.json")
	right.lockPath = filepath.Join(root, "rush.json.lock")
	right.selectedPath = right.path
	before := directoryEntryNames(t, root)
	caseFold := func(_ string, leaf string) (string, bool) {
		return configFoldCaseLeaf(leaf, configCaseSensitivityInsensitive)
	}
	caseSensitive := func(_ string, leaf string) (string, bool) {
		return configFoldCaseLeaf(leaf, configCaseSensitivitySensitive)
	}
	unknown := func(_ string, leaf string) (string, bool) {
		return configFoldCaseLeaf(leaf, configCaseSensitivityUnknown)
	}

	require.Equal(t,
		configWriteTargetDedupKeyWithCaseFold(left, caseFold),
		configWriteTargetDedupKeyWithCaseFold(right, caseFold),
	)
	require.NotEqual(t,
		configWriteTargetDedupKeyWithCaseFold(left, caseSensitive),
		configWriteTargetDedupKeyWithCaseFold(right, caseSensitive),
	)
	require.NotEqual(t,
		configWriteTargetDedupKeyWithCaseFold(left, unknown),
		configWriteTargetDedupKeyWithCaseFold(right, unknown),
	)
	require.Equal(t, before, directoryEntryNames(t, root))
}

func TestConfigCaseVariantHardlinksDoNotProveCaseInsensitivity(t *testing.T) {
	root := t.TempDir()
	before := directoryEntryNames(t, root)
	decision := configCaseSensitivityFromMetadata(false, true, false)
	caseFold := func(_ string, leaf string) (string, bool) {
		return configFoldCaseLeaf(leaf, decision)
	}
	left := configWriteTarget{
		path: filepath.Join(root, "Rush.json"), lockPath: filepath.Join(root, "Rush.json.lock"),
		expected: reloadFileFingerprint{parentIdentity: configParentIdentity(filepath.Join(root, "Rush.json"))},
	}
	right := left
	right.path = filepath.Join(root, "rush.json")
	right.lockPath = filepath.Join(root, "rush.json.lock")
	require.NotEqual(t, configWriteTargetDedupKeyWithCaseFold(left, caseFold), configWriteTargetDedupKeyWithCaseFold(right, caseFold))
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

	lock1, err := acquireConfigFileLock(t.Context(), target1.lockPath)
	require.NoError(t, err)
	_, err = commitConfigFile(target1.selectedPath, target1.path, []byte(`{"existing":true,"first":true}`), 0o600,
		target1.expected, target1.owner, target1.enforce)
	require.NoError(t, err)
	require.NoError(t, lock1.Release())

	lock2, err := acquireConfigFileLock(t.Context(), target2.lockPath)
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

func TestMCPAdmissionUsesInjectedCaseDecisionForMissingAliases(t *testing.T) {
	tests := []struct {
		name            string
		sensitivity     configCaseSensitivity
		expectedGroups  int
		expectedRecords int
	}{
		{name: "insensitive", sensitivity: configCaseSensitivityInsensitive, expectedGroups: 1, expectedRecords: 1},
		{name: "sensitive", sensitivity: configCaseSensitivitySensitive, expectedGroups: 2, expectedRecords: 2},
		{name: "unknown", sensitivity: configCaseSensitivityUnknown, expectedGroups: 2, expectedRecords: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
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
				return session.AcquireFileLockContext(ctx, filepath.Join(root, fmt.Sprintf("injected-lock-%d", acquisitions)))
			}
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.acquireConfigLock = previous
				configTestHooks.Unlock()
			})

			foldCalls := 0
			caseSensitivity := func(_ string) configCaseSensitivity {
				foldCalls++
				return test.sensitivity
			}
			targets := make([]configWriteTarget, 0, 2)
			for _, path := range []string{upper, lower} {
				target, err := store.resolveConfigWriteTarget(path)
				require.NoError(t, err)
				targets = append(targets, target)
			}
			groups, _, _ := configMCPAdmissionLockGroups(targets, caseSensitivity)
			require.Len(t, groups, test.expectedGroups)
			foldCalls = 0
			err := store.withMCPLocksUsingCaseSensitivity(context.Background(), func(files *mcpLockedFiles) error {
				require.Len(t, files.records, test.expectedRecords)
				return nil
			}, caseSensitivity)
			require.NoError(t, err)
			require.Equal(t, test.expectedGroups, acquisitions)
			require.Equal(t, 1, foldCalls)
		})
	}
}

func TestMCPAdmissionPinsCaseSensitivityPerParent(t *testing.T) {
	root := t.TempDir()
	parent := configParentIdentity(filepath.Join(root, "Rush.json"))
	require.True(t, parent.valid)
	targets := []configWriteTarget{
		{path: filepath.Join(root, "Rush.json"), lockPath: filepath.Join(root, "Rush.json.lock"), selectedPath: filepath.Join(root, "Rush.json"), expected: reloadFileFingerprint{parentIdentity: parent}},
		{path: filepath.Join(root, "rush.json"), lockPath: filepath.Join(root, "rush.json.lock"), selectedPath: filepath.Join(root, "rush.json"), expected: reloadFileFingerprint{parentIdentity: parent}},
	}
	calls := 0
	detector := func(string) configCaseSensitivity {
		calls++
		if calls == 1 {
			return configCaseSensitivityInsensitive
		}
		return configCaseSensitivityUnknown
	}
	groups, _, _ := configMCPAdmissionLockGroups(targets, detector)
	require.Len(t, groups, 1)
	require.Equal(t, 1, calls)

	distinctTargets := []configWriteTarget{
		{path: filepath.Join(root, "one", "Rush.json"), lockPath: filepath.Join(root, "one", "Rush.json.lock"), selectedPath: filepath.Join(root, "one", "Rush.json"), expected: reloadFileFingerprint{parentIdentity: configFileIdentity{device: 9, inode: 1, valid: true}}},
		{path: filepath.Join(root, "two", "rush.json"), lockPath: filepath.Join(root, "two", "rush.json.lock"), selectedPath: filepath.Join(root, "two", "rush.json"), expected: reloadFileFingerprint{parentIdentity: configFileIdentity{device: 9, inode: 2, valid: true}}},
	}
	calls = 0
	groups, _, _ = configMCPAdmissionLockGroups(distinctTargets, detector)
	require.Len(t, groups, 2)
	require.Equal(t, 2, calls)

	invalidTargets := []configWriteTarget{
		{path: filepath.Join(root, "Rush.json"), lockPath: filepath.Join(root, "Rush.json.lock"), selectedPath: filepath.Join(root, "Rush.json")},
		{path: filepath.Join(root, "rush.json"), lockPath: filepath.Join(root, "rush.json.lock"), selectedPath: filepath.Join(root, "rush.json")},
	}
	calls = 0
	groups, _, _ = configMCPAdmissionLockGroups(invalidTargets, detector)
	require.Len(t, groups, 2)
	require.Zero(t, calls)
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
