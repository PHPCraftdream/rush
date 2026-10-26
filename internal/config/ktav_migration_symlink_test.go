// Coverage for the KTAV-6 symlink migration: a symlinked legacy rush.json
// is MIGRATED at Load — rush.ktav is published as a regular file beside the
// LINK path, the link is removed, the physical target in the dotfiles
// repository is left untouched (it IS the backup; no .bak is created), a
// dangling or non-regular target defers with zero side effects, and a
// Symlink-marked migration record is retired idempotently by crash recovery.
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

// TestKtavLoadMigratesSymlinkedLegacyConfig verifies that Load migrates a
// symlinked rush.json in place of deferring: the link is removed, rush.ktav
// appears beside the link path only, the physical target keeps its exact
// bytes, no record and no .bak survive, the loaded store resolves the
// custom provider, and a second Load is idempotent.
//
// Hand-trace (first assertion after setup is require.FileExists(dir/
// "rush.ktav")): migrateKtavSymlinkedConfig publishes rush.ktav beside the
// link via commitConfigFile, then removes only the link
// (ktav_migration_symlink.go). On Windows os.Symlink may fail without
// privileges — the test skips in that case (both traces below).
//
// Mutation: making runKtavSymlinkMigrationState return (false, nil) for the
// fresh gate (restoring the old defer) leaves the link in place and no
// rush.ktav — the Lstat(logical) and FileExists(rush.ktav) assertions fail.
// Mutation: removing the link in migrateKtavSymlinkedConfig and renaming
// the physical target instead would change physical bytes — the
// byte-identical assertion fails.
func TestKtavLoadMigratesSymlinkedLegacyConfig(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	_, _ = isolateAllGlobalConfigPaths(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	physical := filepath.Join(dir, "dotfiles", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(physical), 0o755))
	original := []byte(`{"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"test-key","models":[{"id":"model","name":"Model"}]}},"options":{"disable_default_providers":true}}`)
	require.NoError(t, os.WriteFile(physical, original, 0o600))
	logical := filepath.Join(dir, "rush.json")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("logical rush.json symlink unavailable: %v", err)
	}

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NotNil(t, store)

	// rush.ktav is published beside the LINK path, and nothing else exists.
	require.FileExists(t, ktavSiblingOf(logical))
	require.NoFileExists(t, migrationRecordPathFor(logical))
	require.NoFileExists(t, backupPathFor(logical))
	require.NoFileExists(t, filepath.Join(dir, "dotfiles", "rush.ktav"))

	// The link itself is gone.
	_, err = os.Lstat(logical)
	require.ErrorIs(t, err, os.ErrNotExist)

	// The physical target is byte-identical: it was never written.
	persisted, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, original, persisted)

	// The published document decodes as Ktav and the loaded store resolves
	// the custom provider from it.
	ktavRaw, err := os.ReadFile(ktavSiblingOf(logical))
	require.NoError(t, err)
	_, err = ktavcodec.DecodeKtav(ktavRaw)
	require.NoError(t, err)
	pc, ok := store.Config().Providers.Get("custom")
	require.True(t, ok)
	require.Equal(t, "https://example.invalid/v1", pc.BaseURL)

	// Idempotency: a second Load succeeds and creates no new artifacts.
	store2, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NotNil(t, store2)
	require.FileExists(t, ktavSiblingOf(logical))
	require.NoFileExists(t, migrationRecordPathFor(logical))
	require.NoFileExists(t, backupPathFor(logical))
}

// TestKtavLoadDefersDanglingSymlinkedLegacyConfig verifies that a symlink
// whose target does not exist defers with zero side effects: Load succeeds,
// the link stays in place, and no rush.ktav, record, or backup appears on
// either side of the link.
//
// Hand-trace (first assertion is the Lstat mode check): os.Stat on the
// dangling link fails in migrateKtavSymlinkedConfig, so
// warnMigrationDeferredOnce fires and the function returns nil before any
// artifact is written. On Windows os.Symlink may fail without privileges —
// the test skips in that case.
//
// Mutation: committing the migration record before target validation in
// migrateKtavSymlinkedConfig strands migrationRecordPathFor(logical) — the
// require.NoFileExists(record) fails.
func TestKtavLoadDefersDanglingSymlinkedLegacyConfig(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	_, _ = isolateAllGlobalConfigPaths(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dotfiles := filepath.Join(dir, "dotfiles")
	require.NoError(t, os.MkdirAll(dotfiles, 0o755))
	logical := filepath.Join(dir, "rush.json")
	if err := os.Symlink(filepath.Join(dotfiles, "missing.json"), logical); err != nil {
		t.Skipf("logical rush.json symlink unavailable: %v", err)
	}

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NotNil(t, store)

	linkInfo, err := os.Lstat(logical)
	require.NoError(t, err)
	require.NotEqual(t, os.FileMode(0), linkInfo.Mode()&os.ModeSymlink)
	require.NoFileExists(t, ktavSiblingOf(logical))
	require.NoFileExists(t, migrationRecordPathFor(logical))
	require.NoFileExists(t, backupPathFor(logical))
	entries, err := os.ReadDir(dotfiles)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// TestKtavSymlinkMigrationCrashRecovery drives runKtavMigrationState
// directly against the two interrupted symlink-migration states: (a) record
// + rush.ktav + link still present, and (b) record + rush.ktav with the
// link already gone. Both must retire the record idempotently and leave the
// physical target untouched.
//
// Hand-trace (first assertion of (a) is the NoError from
// runKtavMigrationState): runKtavSymlinkMigrationState sees recordExists
// with record.Symlink and hands off to finishKtavSymlinkMigration, which
// verifies StagedDigest against rush.ktav and — while the link exists —
// SourceDigest/SourceID/ParentID against the target through the link,
// removes the link, then removes the record.
//
// Mutation: dropping the os.Remove(legacy) link removal in
// finishKtavSymlinkMigration leaves the link — the Lstat assertion fails
// while the record is still retired; reversing removal order (record first)
// makes case (a) strand the link.
func TestKtavSymlinkMigrationCrashRecovery(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	_, _ = isolateAllGlobalConfigPaths(t)

	original := []byte(`{"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"test-key","models":[{"id":"model","name":"Model"}]}},"options":{"disable_default_providers":true}}`)

	// writeCrashArtifacts materializes the crash state: physical target,
	// symlink, published rush.ktav, and a Symlink-marked migration record
	// whose identity fields come from reading the source through the link —
	// exactly what finishKtavSymlinkMigration re-verifies.
	writeCrashArtifacts := func(t *testing.T, dir string, keepLink bool) (logical, physical, ktavPath, recordPath string) {
		t.Helper()
		physical = filepath.Join(dir, "dotfiles", "config.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(physical), 0o755))
		require.NoError(t, os.WriteFile(physical, original, 0o600))
		logical = filepath.Join(dir, "rush.json")
		if err := os.Symlink(physical, logical); err != nil {
			t.Skipf("logical rush.json symlink unavailable: %v", err)
		}
		ktavBytes, err := ktavcodec.ConvertJSONToKtav(original)
		require.NoError(t, err)
		ktavPath = ktavSiblingOf(logical)
		require.NoError(t, os.WriteFile(ktavPath, ktavBytes, 0o600))
		_, sourceFP, err := readStableConfigFileOwned(logical, 0, false)
		require.NoError(t, err)
		record := ktavMigrationRecord{
			Version:      ktavMigrationRecordVersion,
			Source:       filepath.Base(logical),
			Destination:  filepath.Base(ktavPath),
			Backup:       filepath.Base(backupPathFor(logical)),
			SourceDigest: hex.EncodeToString(sourceFP.digest[:]),
			StagedDigest: fmt.Sprintf("%x", sha256.Sum256(ktavBytes)),
			SourceID:     fmt.Sprintf("%v", sourceFP.identity),
			ParentID:     fmt.Sprintf("%v", sourceFP.parentIdentity),
			Symlink:      true,
		}
		recordBytes, err := json.Marshal(record)
		require.NoError(t, err)
		recordPath = migrationRecordPathFor(logical)
		require.NoError(t, os.WriteFile(recordPath, append(recordBytes, '\n'), 0o600))
		if !keepLink {
			require.NoError(t, os.Remove(logical))
		}
		return logical, physical, ktavPath, recordPath
	}

	// (a) crash after publishing rush.ktav and the record, before removing
	// the link: recovery verifies the digests through the link, removes the
	// link, and retires the record.
	dirA, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	logical, physical, ktavPath, recordPath := writeCrashArtifacts(t, dirA, true)
	require.NoError(t, runKtavMigrationState(context.Background(), logical, dirA))
	_, err = os.Lstat(logical)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoFileExists(t, recordPath)
	require.FileExists(t, ktavPath)
	persisted, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, original, persisted)

	// (b) crash after the link was already removed: recovery retires the
	// record and leaves rush.ktav in place.
	dirB, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	logical, physical, ktavPath, recordPath = writeCrashArtifacts(t, dirB, false)
	require.NoError(t, runKtavMigrationState(context.Background(), logical, dirB))
	require.NoFileExists(t, recordPath)
	require.FileExists(t, ktavPath)
	persisted, err = os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, original, persisted)
}

// TestKtavSymlinkRecordParentAnchor pins the invariant that a
// Symlink-marked migration record loads even when the legacy path's parent
// identity diverges from the record path's parent identity, which happens
// on Windows where configParentIdentity resolves a symlinked leaf through
// physicalWindowsConfigPath and therefore reports the link TARGET's
// directory, not the directory holding the link.
//
// Mutation: dropping the `!record.Symlink &&` gate in loadKtavMigrationRecord
// re-enables the legacy-parent clause and this test fails on Windows
// (CI-proven failure mode: "malformed or bound to a different slot"); it
// stays green on POSIX, where the two parents coincide. Mutation: making
// Windows configParentIdentity stop resolving the leaf link flips `diverged`
// to false on Windows and the require.True fails — the test then documents
// that the skip is redundant, not that it is wrong.
func TestKtavSymlinkRecordParentAnchor(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	_, _ = isolateAllGlobalConfigPaths(t)

	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	original := []byte(`{"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"test-key","models":[{"id":"model","name":"Model"}]}},"options":{"disable_default_providers":true}}`)
	physical := filepath.Join(dir, "dotfiles", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(physical), 0o755))
	require.NoError(t, os.WriteFile(physical, original, 0o600))
	logical := filepath.Join(dir, "rush.json")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("logical rush.json symlink unavailable: %v", err)
	}
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(original)
	require.NoError(t, err)
	ktavPath := ktavSiblingOf(logical)
	require.NoError(t, os.WriteFile(ktavPath, ktavBytes, 0o600))
	_, sourceFP, err := readStableConfigFileOwned(logical, 0, false)
	require.NoError(t, err)
	record := ktavMigrationRecord{
		Version:      ktavMigrationRecordVersion,
		Source:       filepath.Base(logical),
		Destination:  filepath.Base(ktavPath),
		Backup:       filepath.Base(backupPathFor(logical)),
		SourceDigest: hex.EncodeToString(sourceFP.digest[:]),
		StagedDigest: fmt.Sprintf("%x", sha256.Sum256(ktavBytes)),
		SourceID:     fmt.Sprintf("%v", sourceFP.identity),
		ParentID:     fmt.Sprintf("%v", sourceFP.parentIdentity),
		Symlink:      true,
	}
	recordBytes, err := json.Marshal(record)
	require.NoError(t, err)
	recordPath := migrationRecordPathFor(logical)
	require.NoError(t, os.WriteFile(recordPath, append(recordBytes, '\n'), 0o600))

	// Hand-trace (first assertion is require.NoError from
	// loadKtavMigrationRecord) on the WINDOWS path: recordFP.parentIdentity
	// is the identity of dir/ (the record is a regular file beside the
	// link), configParentIdentity(recordPath) is also dir/ — the recordPath
	// anchor passes. The legacy clause is skipped because record.Symlink is
	// true, so the link-target divergence (configParentIdentity(logical)
	// resolves through the leaf to dotfiles/) cannot reject the record.
	// On POSIX configParentIdentity(logical) is lexically dir/, so the
	// legacy clause would also pass — the test is green on both OSes.
	loaded, err := loadKtavMigrationRecord(recordPath, logical, ktavPath, dir)
	require.NoError(t, err)
	require.True(t, loaded.Symlink)

	// Pin the OS divergence explicitly: on Windows the symlinked legacy
	// leaf resolves to the target's directory, diverging from the record
	// (link) directory; on POSIX both are lexically dir/.
	diverged := configParentIdentity(logical) != configParentIdentity(recordPath)
	if runtime.GOOS == "windows" {
		require.True(t, diverged, "configParentIdentity of a symlinked legacy path must resolve through the leaf on Windows — the Symlink skip in loadKtavMigrationRecord is load bearing")
	} else {
		require.False(t, diverged)
	}
}
