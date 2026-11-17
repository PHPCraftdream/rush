package config

import (
	"context"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/slotmigrate"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
)

// Test-only forwarders preserving the root package's historical unexported
// names for root *_test.go files. Each carries the removing layer in its
// comment; delete the forwarder when root tests call the io layer directly.

// io layer: slotmigrate.
var errKtavMigrationCrashInjected = slotmigrate.ErrKtavMigrationCrashInjected()

// io layer: slotmigrate.
func runKtavMigrationState(ctx context.Context, legacy, workingDir string) error {
	return slotmigrate.RunKtavMigrationState(ctx, legacy, workingDir)
}

// io layer: slotmigrate.
func migrateKtavArtifact(ctx context.Context, legacy, workingDir string) error {
	return slotmigrate.MigrateKtavArtifact(ctx, legacy, workingDir)
}

// io layer: slotmigrate.
func loadKtavMigrationRecord(recordPath, legacy, dest, workingDir string) (*ktavMigrationRecord, error) {
	return slotmigrate.LoadKtavMigrationRecord(recordPath, legacy, dest, workingDir)
}

// io layer: slotmigrate.
func ownerPolicyForMigration(path, workingDir string) (int, bool, error) {
	return slotmigrate.OwnerPolicyForMigration(path, workingDir)
}

// io layer: slotmigrate.
func applyKtavMigrationModelsIntegrity(source, ktavBytes []byte) ([]byte, error) {
	return slotmigrate.ApplyKtavMigrationModelsIntegrity(source, ktavBytes)
}

// io layer: slotmigrate.
type ktavMigrationRecord = slotmigrate.KtavMigrationRecord

// io layer: slotmigrate.
const ktavMigrationRecordVersion = slotmigrate.KtavMigrationRecordVersion

// io layer: slotmigrate.
func setKtavMigrationBudgetForTest(d time.Duration) (time.Duration, func()) {
	return slotmigrate.SetKtavMigrationBudgetForTest(d)
}

// io layer: slotmigrate.
func getKtavMigrationBudgetForTest() time.Duration {
	return slotmigrate.GetKtavMigrationBudgetForTest()
}

// io layer: slotmigrate.
func setKtavMigrationStepHookForTest(hook func(step string) error) (func(string) error, func()) {
	return slotmigrate.SetKtavMigrationStepHookForTest(hook)
}

// io layer: slotmigrate.
func getKtavMigrationStepHookForTest() func(string) error {
	return slotmigrate.GetKtavMigrationStepHookForTest()
}

// io layer: slotmigrate.
func setKtavArtifactStatFileForTest(fn func(name string) (os.FileInfo, error)) (func(string) (os.FileInfo, error), func()) {
	return slotmigrate.SetKtavArtifactStatFileForTest(fn)
}

// io layer: slotmigrate.
func getKtavArtifactStatFileForTest() func(string) (os.FileInfo, error) {
	return slotmigrate.GetKtavArtifactStatFileForTest()
}

// io layer: slotmigrate.
func setKtavArtifactChtimesForTest(fn func(name string, atime, mtime time.Time) error) (func(string, time.Time, time.Time) error, func()) {
	return slotmigrate.SetKtavArtifactChtimesForTest(fn)
}

// io layer: slotmigrate.
func getKtavArtifactChtimesForTest() func(string, time.Time, time.Time) error {
	return slotmigrate.GetKtavArtifactChtimesForTest()
}

// io layer: slotmigrate (seam MISSING in slotmigrate — the parallel
// production worker must export SetKtavArtifactMigrationStepHookForTest).
func setKtavArtifactMigrationStepHookForTest(hook func(step string) error) (func(string) error, func()) {
	return slotmigrate.SetKtavArtifactMigrationStepHookForTest(hook)
}

// test-only forwarder; io layer: slotmigrate.
func ktavMigrateStartupSlots(ctx context.Context, workingDir string) error {
	return slotmigrate.KtavMigrateStartupSlots(ctx, workingDir)
}

// test-only forwarder; io layer: slotmigrate.
func ktavMigrateWorkspaceIfNeeded(ctx context.Context, workingDir, dataDir string) error {
	return slotmigrate.KtavMigrateWorkspaceIfNeeded(ctx, workingDir, dataDir)
}

// test-only forwarder; io layer: slotmigrate.
func ktavMigrateOAuthJournalsAtStartup(ctx context.Context, configSlots []string, workingDir string) {
	slotmigrate.KtavMigrateOAuthJournalsAtStartup(ctx, configSlots, workingDir)
}

// test-only forwarder; io layer: slotmigrate.
func ktavMigrateCacheArtifactsStartup(ctx context.Context, workingDir string) {
	slotmigrate.KtavMigrateCacheArtifactsStartup(ctx, workingDir)
}

// test-only forwarder; io layer: slotmigrate.
func ktavMigrateOAuthJournal(ctx context.Context, configPath, providerID, workingDir string) error {
	return slotmigrate.KtavMigrateOAuthJournal(ctx, configPath, providerID, workingDir)
}

// test-only forwarder; io layer: slotmigrate.
func migrateLegacyConfigFile(ctx context.Context, legacy, workingDir string) error {
	return slotmigrate.MigrateLegacyConfigFile(ctx, legacy, workingDir)
}

// io layer: slots. The test seam lives here; KtavAutoMigrateEnabled remains
// permanent exported facade API in io_forwarders.go.

// test-only forwarder; io layer: slots.
func writeTargetSlot(legacyPath string) string { return slots.WriteTargetSlot(legacyPath) }

// test-only forwarder; io layer: slots.
func readCacheDocument(legacyJSON string) ([]byte, error) {
	return slots.ReadCacheDocument(legacyJSON)
}

// test-only forwarder; io layer: slots.
func writeCacheDocument(legacyJSON string, jsonBytes []byte, perm os.FileMode, writeJSON func(path string, data []byte, perm os.FileMode) error) error {
	return slots.WriteCacheDocument(legacyJSON, jsonBytes, perm, writeJSON)
}

// test-only forwarder; io layer: slots.
func selectOAuthJournalPath(configPath, providerID string, forWrite bool) string {
	return slots.SelectOAuthJournalPath(configPath, providerID, forWrite)
}

// io layer: fswap (unix-only seam; see io_testsupport_rename_unix_test.go).
// io layer: fileid (windows-only seam; see io_testsupport_windows_test.go).

// io layer: fileid.
func writeStableConfigFile(path string, data []byte) error {
	return fileid.WriteStableConfigFile(path, data)
}

// io layer: slots.
func newConfigFileIsKtav() bool { return slots.NewConfigFileIsKtav() }

// io layer: fileid.
func configFileOwner(info os.FileInfo) (int, bool) {
	return fileid.ConfigFileOwner(info)
}

// io layer: casefold (configCaseSensitivityUnknown already exists in facade.go).
const (
	configCaseSensitivitySensitive   = casefold.ConfigCaseSensitivitySensitive
	configCaseSensitivityInsensitive = casefold.ConfigCaseSensitivityInsensitive
)

// io layer: casefold.
func configCaseSensitivityFromMetadata(filesystemInsensitive, filesystemSensitive, directoryCasefold bool) configCaseSensitivity {
	return casefold.ConfigCaseSensitivityFromMetadata(filesystemInsensitive, filesystemSensitive, directoryCasefold)
}

// io layer: cfgcommit.
func configWriteTargetDedupKeyWithCaseFold(target configWriteTarget, caseFold func(string, string) (string, bool)) string {
	ct := cfgcommit.NewConfigWriteTarget(target.path, target.selectedPath, target.lockPath, target.expected, target.owner, target.enforce)
	return cfgcommit.ConfigWriteTargetDedupKeyWithCaseFold(ct, caseFold)
}

// test-only forwarder; io layer: cfgcommit.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return cfgcommit.AtomicWriteFile(path, data, perm)
}

// test-only forwarder; io layer: cfgcommit.
func syncConfigParentOnDisk(path string) error { return cfgcommit.SyncConfigParentOnDisk(path) }

// test-only forwarder; io layer: cfgcommit.
func acquireConfigFileLock(ctx context.Context, path string) (*filelock.FileLock, error) {
	return cfgcommit.AcquireConfigFileLock(ctx, path)
}

// test-only forwarder; io layer: cfgcommit.
func releaseConfigFileLock(path string, lock *filelock.FileLock) error {
	return cfgcommit.ReleaseConfigFileLock(path, lock)
}

// test-only forwarder; io layer: cfgcommit.
func commitConfigFile(selectedPath, commitPath string, data []byte, perm os.FileMode, expected docread.Fingerprint, expectedOwner int, enforceOwner bool, migrationHooks ...func(string) error) (docread.Fingerprint, error) {
	return cfgcommit.CommitConfigFile(selectedPath, commitPath, data, perm, expected, expectedOwner, enforceOwner, migrationHooks...)
}

// test-only forwarder; io layer: cfgcommit.
func configDirectoryIsCaseInsensitive(dir string) bool {
	return cfgcommit.ConfigDirectoryIsCaseInsensitive(dir)
}

// io layer: casefold.
type configCaseDirectoryEntry = casefold.CaseDirectoryEntry

// io layer: casefold.
type configCaseDirectorySnapshot = casefold.CaseDirectorySnapshot

// io layer: casefold.
func configCaseSensitivityFromDirectorySnapshots(
	dir string,
	readSnapshot func() (casefold.CaseDirectorySnapshot, error),
	probe func(string) (fileid.FileIdentity, error),
) configCaseSensitivity {
	return casefold.ConfigCaseSensitivityFromDirectorySnapshots(dir, readSnapshot, probe)
}

// io layer: casefold.
func configLinuxCaseSensitivityFromMetadata(filesystemType uint64, flags uint32, flagsKnown bool) configCaseSensitivity {
	return casefold.LinuxCaseSensitivityFromMetadata(filesystemType, flags, flagsKnown)
}

// io layer: casefold.
func configDarwinCaseSensitivityFromPathconf(value int, err error) configCaseSensitivity {
	return casefold.DarwinCaseSensitivityFromPathconf(value, err)
}

// io layer: casefold.
func configFreeBSDCaseSensitivityFromFilesystemName(name string) configCaseSensitivity {
	return casefold.FreeBSDCaseSensitivityFromFilesystemName(name)
}

// io layer: casefold.
func configCaseDirectoryBudgetExceeded(names []string) bool {
	return casefold.CaseDirectoryBudgetExceeded(names)
}

// io layer: casefold.
var errConfigCaseDirectoryBudgetExceeded = casefold.ErrCaseDirectoryBudgetExceeded()

// io layer: casefold.
const (
	configLinuxExt4SuperMagic       = casefold.LinuxExt4SuperMagic
	configLinuxF2FSSuperMagic       = casefold.LinuxF2FSSuperMagic
	configLinuxMSDOSSuperMagic      = casefold.LinuxMSDOSSuperMagic
	configLinuxExFATSuperMagic      = casefold.LinuxExFATSuperMagic
	configLinuxFSCasefoldFlag       = casefold.LinuxFSCasefoldFlag
	configCaseDirectoryMaxEntries   = casefold.CaseDirectoryMaxEntries
	configCaseDirectoryMaxNameBytes = casefold.CaseDirectoryMaxNameBytes
)
