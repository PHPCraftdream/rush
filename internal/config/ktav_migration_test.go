// Ktav migration transaction tests: success invariants with COMPLETE config
// equality and record lifecycle, existing-backup protection, forced lock
// contention, every plan-4.4 crash boundary with real startup recovery,
// source-generation validation, record authentication, the shared startup
// budget, workspace recovery, and secret-free diagnostics.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

func ktavSeedLegacy(t *testing.T, dir string) string {
	t.Helper()
	legacy := filepath.Join(dir, "rush.json")
	writeTestFile(t, legacy, `{"providers":{"p":{"api_key":"`+ktavTestSecret+`"}},"numeric_fixture":{"decimal":0.50,"large":9007199254740993}}`)
	return legacy
}

func ktavSeedLoadLegacy(t *testing.T, dir string) string {
	t.Helper()
	legacy := filepath.Join(dir, "rush.json")
	writeTestFile(t, legacy, `{"options":{"debug":true},"migration_test_secret":"`+ktavTestSecret+`"}`)
	return legacy
}

// ktavAssertMigrated proves K + B exist, the record is gone, the backup
// holds the original bytes, and the COMPLETE active config loaded from the
// Ktav deep-equals the config loaded from the backup.
func ktavAssertMigrated(t *testing.T, dir string) {
	t.Helper()
	legacy := filepath.Join(dir, "rush.json")
	require.FileExists(t, filepath.Join(dir, "rush.ktav"))
	require.FileExists(t, filepath.Join(dir, "rush.json.bak"))
	require.NoFileExists(t, legacy)
	require.NoFileExists(t, filepath.Join(dir, "rush.json.ktav-migration"))

	backup, err := os.ReadFile(filepath.Join(dir, "rush.json.bak"))
	require.NoError(t, err)
	require.Contains(t, string(backup), ktavTestSecret)

	raw, err := os.ReadFile(filepath.Join(dir, "rush.ktav"))
	require.NoError(t, err)
	backupTree, err := ktavcodec.ParseJSONLossless(backup)
	require.NoError(t, err)
	ktavTree, err := ktavcodec.DecodeKtav(raw)
	require.NoError(t, err)
	require.NoError(t, ktavcodec.Equivalent(backupTree, ktavTree), "active Ktav must preserve the original JSON document")
}

// ktavIsolateGlobals keeps every migration test away from the operator's
// real global config: pending-slot scans and gated startup migration both
// walk the well-known global slots.
func ktavIsolateGlobals(t *testing.T) {
	t.Helper()
	configDir, dataDir := isolateAllGlobalConfigPaths(t)
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
}

func TestMigrateLegacyConfigFile_Success(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)

	var seenSteps []string
	prevHook := ktavMigrationStepHook
	ktavMigrationStepHook = func(step string) error {
		seenSteps = append(seenSteps, step)
		if step == "after-record" {
			// The record must exist while the transaction is running.
			require.FileExists(t, filepath.Join(dir, "rush.json.ktav-migration"))
		}
		return nil
	}
	err := migrateLegacyConfigFile(context.Background(), legacy, dir)
	ktavMigrationStepHook = prevHook
	require.NoError(t, err)
	require.Contains(t, seenSteps, "after-record", "the record must be created during the transaction")

	ktavAssertMigrated(t, dir)

	// Migration is idempotent once complete.
	require.NoError(t, migrateLegacyConfigFile(context.Background(), legacy, dir))
	ktavAssertMigrated(t, dir)
}

func TestMigrateLegacyConfigFile_ExistingBackupNeverOverwritten(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)
	originalJSON, readOriginalErr := os.ReadFile(legacy)
	require.NoError(t, readOriginalErr)
	backup := filepath.Join(dir, "rush.json.bak")
	preciousBackup := `{"providers":{"p":{"api_key":"PRECIOUS-BACKUP"}}}`
	writeTestFile(t, backup, preciousBackup)

	err := migrateLegacyConfigFile(context.Background(), legacy, dir)
	require.ErrorIs(t, err, errKtavMigrationConflict)

	// Every file is preserved; the JSON stays active and unmodified.
	onDisk, readErr := os.ReadFile(backup)
	require.NoError(t, readErr)
	require.Equal(t, preciousBackup, string(onDisk), "a pre-existing backup must never be rewritten")
	jsonOnDisk, readErr := os.ReadFile(legacy)
	require.NoError(t, readErr)
	require.Equal(t, originalJSON, jsonOnDisk)
	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
}

func TestMigrateLegacyConfigFile_BackupOnlyFailsClosed(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	backup := filepath.Join(dir, "rush.json.bak")
	writeTestFile(t, backup, `{"providers":{"p":{"api_key":"`+ktavTestSecret+`"}}}`)

	err := migrateLegacyConfigFile(context.Background(), filepath.Join(dir, "rush.json"), dir)
	require.ErrorIs(t, err, errKtavBackupOnly)
	require.Contains(t, err.Error(), "manual")
	require.FileExists(t, backup)
	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
}

func TestMigrateLegacyConfigFile_UnconvertibleJSONLeavesSourceUntouched(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	original := `{"providers":{"p":{"api_key":"line1\rline2"}}}`
	writeTestFile(t, legacy, original)
	err := migrateLegacyConfigFile(context.Background(), legacy, dir)
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
	require.NoFileExists(t, filepath.Join(dir, "rush.json.bak"))
	require.NoFileExists(t, filepath.Join(dir, "rush.json.ktav-migration"))
	onDisk, readErr := os.ReadFile(legacy)
	require.NoError(t, readErr)
	require.Equal(t, original, string(onDisk))
}

func TestMigrateLegacyConfigFile_WaitsForLegacySidecarLock(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)

	lock, err := acquireConfigFileLock(context.Background(), legacy+".lock")
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- migrateLegacyConfigFile(context.Background(), legacy, dir)
	}()
	select {
	case err := <-done:
		_ = releaseConfigFileLock(legacy+".lock", lock)
		require.NoError(t, err)
		t.Fatal("migrator ran without waiting for the legacy sidecar lock")
	case <-time.After(150 * time.Millisecond):
		// Still blocked: correct.
	}
	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
	require.NoError(t, releaseConfigFileLock(legacy+".lock", lock))
	require.NoError(t, <-done)
	ktavAssertMigrated(t, dir)
}

// TestMigrateLegacyConfigFile_ConcurrentMigrators starts two migrators at
// once; both must succeed and exactly one K and one B may exist.
func TestMigrateLegacyConfigFile_ConcurrentMigrators(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)

	const migrators = 2
	lock, lockErr := acquireConfigFileLock(context.Background(), legacy+".lock")
	require.NoError(t, lockErr)
	attempts := make(chan struct{}, migrators)
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		if normalizeReloadPath(path) == normalizeReloadPath(legacy+".lock") {
			attempts <- struct{}{}
		}
		return filelock.AcquireFileLockContext(ctx, path)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.Unlock()
	})
	done := make(chan error, migrators)
	for range migrators {
		go func() { done <- migrateLegacyConfigFile(context.Background(), legacy, dir) }()
	}
	drained := 0
	defer func() {
		if lock != nil {
			_ = releaseConfigFileLock(legacy+".lock", lock)
		}
		for ; drained < migrators; drained++ {
			<-done
		}
	}()
	select {
	case <-attempts:
	case <-time.After(5 * time.Second):
		t.Fatal("first migrator did not attempt lock acquisition")
	}
	select {
	case <-attempts:
	case <-time.After(5 * time.Second):
		t.Fatal("second migrator did not attempt lock acquisition")
	}
	select {
	case err := <-done:
		drained++
		t.Fatalf("migrator completed while the sidecar lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
	require.NoError(t, releaseConfigFileLock(legacy+".lock", lock))
	lock = nil
	for ; drained < migrators; drained++ {
		require.NoError(t, <-done)
	}
	ktavAssertMigrated(t, dir)
}

func TestMigrateLegacyConfigFile_CrashBoundariesRecovers(t *testing.T) {
	type boundaryCase struct {
		name       string
		step       string
		hasRecord  bool
		expectJSON bool
		expectKtav bool
		expectBak  bool
	}
	cases := []boundaryCase{
		{name: "before staging", step: "before-staging", expectJSON: true},
		{name: "during staging leaves partial temp", step: "during-staging", expectJSON: true},
		{name: "after record before publish", step: "after-record", hasRecord: true, expectJSON: true},
		{name: "after K rename before sync", step: "after-ktav-rename", hasRecord: true, expectJSON: true, expectKtav: true},
		{name: "after K durable before archival", step: "after-publish", hasRecord: true, expectJSON: true, expectKtav: true},
		{name: "during backup rename", step: "during-backup-rename", hasRecord: true, expectJSON: true, expectKtav: true},
		{name: "after backup rename before sync", step: "after-backup-rename", hasRecord: true, expectKtav: true, expectBak: true},
		{name: "after archival before record cleanup", step: "after-archive", hasRecord: true, expectKtav: true, expectBak: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ktavIsolateGlobals(t)
			dir := t.TempDir()
			legacy := ktavSeedLoadLegacy(t, dir)
			original, err := os.ReadFile(legacy)
			require.NoError(t, err)

			previousHook := ktavMigrationStepHook
			ktavMigrationStepHook = func(step string) error {
				if step == tc.step {
					return errKtavMigrationCrashInjected
				}
				return nil
			}
			migrationErr := migrateLegacyConfigFile(context.Background(), legacy, dir)
			ktavMigrationStepHook = previousHook
			require.ErrorIs(t, migrationErr, errKtavMigrationCrashInjected)

			if tc.step == "during-staging" {
				tempFiles, globErr := filepath.Glob(filepath.Join(dir, ".rush.ktav.*.tmp"))
				require.NoError(t, globErr)
				require.NotEmpty(t, tempFiles)
				partial, readErr := os.ReadFile(tempFiles[0])
				require.NoError(t, readErr)
				require.NotEmpty(t, partial)
				require.Less(t, len(partial), len(testKtavBytes(t, string(original))))
			}
			require.Equal(t, tc.expectJSON, fileExists(legacy))
			require.Equal(t, tc.expectKtav, fileExists(filepath.Join(dir, "rush.ktav")))
			require.Equal(t, tc.expectBak, fileExists(filepath.Join(dir, "rush.json.bak")))
			require.Equal(t, tc.hasRecord, fileExists(filepath.Join(dir, "rush.json.ktav-migration")))

			if tc.hasRecord {
				ktavLoadMigrationTestConfig(t, dir, false)
			} else {
				ktavLoadMigrationTestConfig(t, dir, false)
				require.FileExists(t, legacy)
				require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
				ktavLoadMigrationTestConfig(t, dir, true)
			}
			ktavAssertMigrated(t, dir)
			require.NoError(t, migrateLegacyConfigFile(context.Background(), legacy, dir))
			ktavAssertMigrated(t, dir)
		})
	}
}

// TestMigrateLegacyConfigFile_StaleTempIsIgnoredAsConfig pins the
// "during staging" crash row: a leftover temp must never be treated as a
// config document and must not block recovery.
func TestMigrateLegacyConfigFile_StaleTempIsIgnoredAsConfig(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)
	staleTemp := filepath.Join(dir, ".rush.json.stalegeneration.tmp")
	writeTestFile(t, staleTemp, `{"providers":{"p":{"api_key":"STALE-TEMP"}}}`)

	require.NoError(t, migrateLegacyConfigFile(context.Background(), legacy, dir))
	ktavAssertMigrated(t, dir)

	paths := lookupConfigCandidates(dir)
	require.NotContains(t, paths, normalizeDiscoveryPath(staleTemp))
}

// TestMigrateLegacyConfigFile_SourceChangedBeforeArchival proves the
// converted generation is authoritative: JSON rewritten between publication
// and archival stops the migration with the JSON active and every entry
// preserved.
func TestMigrateLegacyConfigFile_SourceChangedBeforeArchival(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)
	rewritten := `{"providers":{"p":{"api_key":"CHANGED-AFTER-PUBLISH"}}}`

	prevHook := ktavMigrationStepHook
	ktavMigrationStepHook = func(step string) error {
		if step == "after-publish" {
			if writeErr := os.WriteFile(legacy, []byte(rewritten), 0o600); writeErr != nil {
				return writeErr
			}
			return nil
		}
		return nil
	}
	err := migrateLegacyConfigFile(context.Background(), legacy, dir)
	ktavMigrationStepHook = prevHook
	require.ErrorIs(t, err, errKtavMigrationConflict)

	// Recovery must refuse to archive the CHANGED generation.
	err = migrateLegacyConfigFile(context.Background(), legacy, dir)
	require.ErrorIs(t, err, errKtavMigrationConflict)

	onDisk, readErr := os.ReadFile(legacy)
	require.NoError(t, readErr)
	require.Equal(t, rewritten, string(onDisk), "the changed JSON must stay active")
	require.FileExists(t, filepath.Join(dir, "rush.ktav"))
	require.FileExists(t, filepath.Join(dir, "rush.json.ktav-migration"))
	require.NoFileExists(t, filepath.Join(dir, "rush.json.bak"))
}

func TestMigrateLegacyConfigFile_SourceChangedDuringArchival(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)
	original, err := os.ReadFile(legacy)
	require.NoError(t, err)
	changed := []byte(`{"providers":{"p":{"api_key":"CHANGED-DURING-ARCHIVAL"}}}`)
	previousHook := ktavMigrationStepHook
	ktavMigrationStepHook = func(step string) error {
		if step == "during-backup-rename" {
			return os.WriteFile(legacy, changed, 0o600)
		}
		return nil
	}
	err = migrateLegacyConfigFile(context.Background(), legacy, dir)
	ktavMigrationStepHook = previousHook
	require.ErrorIs(t, err, errKtavMigrationConflict)
	require.Contains(t, err.Error(), "rush.json")
	require.Contains(t, err.Error(), "rush.json.bak")
	require.NotContains(t, err.Error(), ktavTestSecret)

	ktavBytes, readErr := os.ReadFile(filepath.Join(dir, "rush.ktav"))
	require.NoError(t, readErr)
	originalTree, parseErr := ktavcodec.ParseJSONLossless(original)
	require.NoError(t, parseErr)
	convertedTree, decodeErr := ktavcodec.DecodeKtav(ktavBytes)
	require.NoError(t, decodeErr)
	require.NoError(t, ktavcodec.Equivalent(originalTree, convertedTree))
	backupBytes, readErr := os.ReadFile(filepath.Join(dir, "rush.json.bak"))
	require.NoError(t, readErr)
	require.Equal(t, changed, backupBytes)
	require.NoFileExists(t, legacy)
	require.FileExists(t, filepath.Join(dir, "rush.json.ktav-migration"))
}

func TestMigrateLegacyConfigFile_ForgedRecordFailsClosed(t *testing.T) {
	ktavIsolateGlobals(t)

	t.Run("wrong destination party", func(t *testing.T) {
		dir := t.TempDir()
		legacy := ktavSeedLegacy(t, dir)
		prevHook := ktavMigrationStepHook
		ktavMigrationStepHook = func(step string) error {
			if step == "after-record" {
				return errKtavMigrationCrashInjected
			}
			return nil
		}
		err := migrateLegacyConfigFile(context.Background(), legacy, dir)
		ktavMigrationStepHook = prevHook
		require.ErrorIs(t, err, errKtavMigrationCrashInjected)
		recordPath := migrationRecordPathFor(legacy)
		var record map[string]any
		content, readErr := os.ReadFile(recordPath)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(content, &record))
		record["destination"] = "other.ktav"
		mutated, marshalErr := json.Marshal(record)
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(recordPath, mutated, 0o600))
		err = migrateLegacyConfigFile(context.Background(), legacy, dir)
		require.ErrorIs(t, err, errKtavMigrationRecord)
		require.FileExists(t, recordPath)
		require.FileExists(t, legacy)
		require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
	})

	t.Run("tampered staged digest", func(t *testing.T) {
		dir := t.TempDir()
		legacy := ktavSeedLegacy(t, dir)
		previousHook := ktavMigrationStepHook
		ktavMigrationStepHook = func(step string) error {
			if step == "after-archive" {
				return errKtavMigrationCrashInjected
			}
			return nil
		}
		err := migrateLegacyConfigFile(context.Background(), legacy, dir)
		ktavMigrationStepHook = previousHook
		require.ErrorIs(t, err, errKtavMigrationCrashInjected)
		recordPath := migrationRecordPathFor(legacy)
		var record map[string]any
		content, readErr := os.ReadFile(recordPath)
		require.NoError(t, readErr)
		require.NoError(t, json.Unmarshal(content, &record))
		record["staged_digest"] = strings.Repeat("0", 64)
		mutated, marshalErr := json.Marshal(record)
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(recordPath, mutated, 0o600))
		err = migrateLegacyConfigFile(context.Background(), legacy, dir)
		require.ErrorIs(t, err, errKtavMigrationRecord)
		require.FileExists(t, recordPath)
		require.FileExists(t, filepath.Join(dir, "rush.ktav"))
		require.FileExists(t, filepath.Join(dir, "rush.json.bak"))
	})

	t.Run("foreign owner rejected on open", func(t *testing.T) {
		dir := t.TempDir()
		legacy := ktavSeedLegacy(t, dir)
		previousHook := ktavMigrationStepHook
		ktavMigrationStepHook = func(step string) error {
			if step == "after-record" {
				return errKtavMigrationCrashInjected
			}
			return nil
		}
		migrationErr := migrateLegacyConfigFile(context.Background(), legacy, dir)
		ktavMigrationStepHook = previousHook
		require.ErrorIs(t, migrationErr, errKtavMigrationCrashInjected)
		recordPath := migrationRecordPathFor(legacy)
		owner, enforce, err := ownerPolicyForMigration(legacy, dir)
		require.NoError(t, err)
		if !enforce {
			_, err = loadKtavMigrationRecord(recordPath, legacy, dir)
			require.NoError(t, err)
			return
		}
		foreignOwner := owner + 1
		if foreignOwner == 0 {
			foreignOwner++
		}
		if chownErr := os.Chown(recordPath, foreignOwner, -1); chownErr != nil {
			if runtime.GOOS == "windows" {
				_, err = loadKtavMigrationRecord(recordPath, legacy, dir)
				require.NoError(t, err)
				return
			}
			_, _, openedErr := readStableConfigFileOwned(recordPath, foreignOwner, true)
			require.ErrorIs(t, openedErr, errConfigOwnerMismatch)
			_, err = loadKtavMigrationRecord(recordPath, legacy, dir)
			require.NoError(t, err)
			return
		}
		t.Cleanup(func() { _ = os.Chown(recordPath, owner, -1) })
		_, err = loadKtavMigrationRecord(recordPath, legacy, dir)
		require.ErrorIs(t, err, errKtavMigrationRecord)
	})
}

// TestKtavStartupResumesInterruptedTransaction proves the REAL startup path
// finishes a recorded transaction even with the auto-migrate gate OFF.
func TestKtavStartupResumesInterruptedTransaction(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	// Use an options-only project fixture; no provider config or migration
	// write-back can mutate the published Ktav during Load.
	legacy := ktavSeedLoadLegacy(t, dir)

	prevHook := ktavMigrationStepHook
	ktavMigrationStepHook = func(step string) error {
		if step == "after-publish" {
			return errKtavMigrationCrashInjected
		}
		return nil
	}
	_ = migrateLegacyConfigFile(context.Background(), legacy, dir)
	ktavMigrationStepHook = prevHook
	require.FileExists(t, filepath.Join(dir, "rush.json.ktav-migration"))

	ktavLoadMigrationTestConfig(t, dir, false)
	ktavAssertMigrated(t, dir)
}

// TestKtavMigrateWorkspaceIfNeededResumesRecord covers the separately
// resolved workspace/data-directory slot's recovery under the same rules.
func TestKtavMigrateWorkspaceIfNeededResumesRecord(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)

	prevHook := ktavMigrationStepHook
	ktavMigrationStepHook = func(step string) error {
		if step == "after-record" {
			return errKtavMigrationCrashInjected
		}
		return nil
	}
	_ = migrateLegacyConfigFile(context.Background(), legacy, dir)
	ktavMigrationStepHook = prevHook
	require.FileExists(t, filepath.Join(dir, "rush.json.ktav-migration"))

	savedGate := ktavAutoMigrate
	ktavAutoMigrate = false
	t.Cleanup(func() { ktavAutoMigrate = savedGate })
	require.NoError(t, ktavMigrateWorkspaceIfNeeded(context.Background(), dir, dir))
	ktavAssertMigrated(t, dir)
}

func TestMigrateLegacyConfigFile_NoSecretsInDiagnostics(t *testing.T) {
	ktavIsolateGlobals(t)
	logs := captureSlogOutput(t)

	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)
	// Force a conflict: an unexpected backup beside the active JSON.
	writeTestFile(t, filepath.Join(dir, "rush.json.bak"), `{"x":1}`)
	err := migrateLegacyConfigFile(context.Background(), legacy, dir)
	require.ErrorIs(t, err, errKtavMigrationConflict)
	require.NotContains(t, err.Error(), ktavTestSecret)

	invalidDir := t.TempDir()
	invalidKtav := filepath.Join(invalidDir, "rush.ktav")
	require.NoError(t, os.WriteFile(invalidKtav, []byte("$$$ not ktav "+ktavTestSecret+" $$$"), 0o600))
	_, _, _, _, loadErr := loadConfigCandidateStable([]string{invalidKtav})
	require.Error(t, loadErr)
	require.NotContains(t, loadErr.Error(), ktavTestSecret)

	require.NotContains(t, logs.String(), ktavTestSecret)
}

func TestMigrateLegacyConfigFile_StagedKtavMode(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)
	require.NoError(t, migrateLegacyConfigFile(context.Background(), legacy, dir))
	info, err := os.Stat(filepath.Join(dir, "rush.ktav"))
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	} else {
		// Windows does not enforce Unix mode bits; the alternative guarantee
		// is that the published file is readable by the test user.
		data, readErr := os.ReadFile(filepath.Join(dir, "rush.ktav"))
		require.NoError(t, readErr)
		require.NotEmpty(t, data)
	}
}

// TestKtavMigrateStartupSlotsRespectsGate proves the gate: OFF means no
// migration of a JSON-only slot, ON migrates it. Global paths are isolated
// BEFORE the gate is enabled.
func TestKtavMigrateStartupSlotsRespectsGate(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)

	savedGate := ktavAutoMigrate
	t.Cleanup(func() { ktavAutoMigrate = savedGate })

	ktavAutoMigrate = false
	require.NoError(t, ktavMigrateStartupSlots(context.Background(), dir))
	require.FileExists(t, legacy)
	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))

	ktavAutoMigrate = true
	require.NoError(t, ktavMigrateStartupSlots(context.Background(), dir))
	ktavAssertMigrated(t, dir)
}

// TestKtavMigrationBudgetIsSharedAndBounded proves Load shares one deadline
// across the global and project JSON slots while deferring lock timeouts.
func TestKtavMigrationBudgetIsSharedAndBounded(t *testing.T) {
	dir := t.TempDir()
	ktavSetupLoadMigrationTest(t, true)
	globalJSON := GlobalConfigData()
	projectJSON := filepath.Join(dir, "rush.json")
	writeTestFile(t, projectJSON, `{"options":{"debug":true}}`)
	projectDataDir := filepath.Join(dir, "workspace-data")
	require.NoError(t, os.MkdirAll(projectDataDir, 0o700))
	writeTestFile(t, filepath.Join(projectDataDir, "rush.json"), `{"options":{"debug":true}}`)
	require.NoError(t, os.MkdirAll(filepath.Dir(globalJSON), 0o700))
	writeTestFile(t, globalJSON, `{"options":{"debug":true}}`)

	savedBudget := ktavMigrationBudget
	ktavMigrationBudget = 60 * time.Millisecond
	t.Cleanup(func() { ktavMigrationBudget = savedBudget })
	var deadlines []time.Time
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		if filepath.Clean(path) == filepath.Clean(globalJSON+".lock") || filepath.Clean(path) == filepath.Clean(projectJSON+".lock") {
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			deadlines = append(deadlines, deadline)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return filelock.AcquireFileLockContext(ctx, path)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.Unlock()
	})

	logs := captureSlogOutput(t)
	started := time.Now()
	store, err := Load(dir, projectDataDir, false)
	elapsed := time.Since(started)
	require.NoError(t, err)
	require.NotNil(t, store)
	require.GreaterOrEqual(t, len(deadlines), 2, "global and project slots should be visited")
	require.Equal(t, deadlines[0], deadlines[1], "all slot attempts share Load's deadline")
	require.Less(t, elapsed, 2*time.Second)
	require.Contains(t, logs.String(), "deferred")
	require.FileExists(t, projectJSON)
	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
}

func TestKtavMigrationRecordHasNoConfigValues(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := ktavSeedLegacy(t, dir)
	prevHook := ktavMigrationStepHook
	ktavMigrationStepHook = func(step string) error {
		if step == "after-record" {
			return errKtavMigrationCrashInjected
		}
		return nil
	}
	err := migrateLegacyConfigFile(context.Background(), legacy, dir)
	ktavMigrationStepHook = prevHook
	require.ErrorIs(t, err, errKtavMigrationCrashInjected)

	record, readErr := os.ReadFile(filepath.Join(dir, "rush.json.ktav-migration"))
	require.NoError(t, readErr)
	require.NotContains(t, string(record), ktavTestSecret)
	require.Contains(t, string(record), "source_digest")
	require.Contains(t, string(record), `"destination":"rush.ktav"`)
	require.False(t, errors.Is(err, errKtavBackupOnly))
}

// TestProjectMigrationEnforcesOwnership proves the migration derives the
// same enforced owner policy the reader uses for a project slot.
func TestProjectMigrationEnforcesOwnership(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rush.json")
	owner, enforce, err := ownerPolicyForMigration(path, "")
	require.NoError(t, err)
	parentInfo, statErr := os.Stat(dir)
	require.NoError(t, statErr)
	actualOwner, actualOwnerKnown := configFileOwner(parentInfo)
	if actualOwnerKnown {
		require.Equal(t, actualOwner, owner)
	}
	if runtime.GOOS != "windows" {
		require.True(t, enforce, "a project slot must not silently disable owner checks")
	}

	legacy := ktavSeedLegacy(t, dir)
	require.NoError(t, migrateLegacyConfigFile(context.Background(), legacy, dir))
	ktavAssertMigrated(t, dir)
}

// TestReloadBackupOnlyFailsClosed mirrors the initial-load fail-closed rule
// on the reload path: the active file vanishing while its backup remains
// must fail the reload, never publish an empty config.
func TestReloadBackupOnlyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	writeTestFile(t, legacy, `{"options":{"debug":true}}`)
	store := ktavLoadMigrationTestConfig(t, dir, false)
	before := store.Config()
	backup := filepath.Join(dir, "rush.json.bak")
	require.NoError(t, os.Rename(legacy, backup))
	err := store.ReloadFromDisk(context.Background())
	require.ErrorIs(t, err, errKtavBackupOnly)
	require.FileExists(t, backup)
	require.Same(t, before, store.Config())
	require.True(t, store.Config().Options.Debug)
}
