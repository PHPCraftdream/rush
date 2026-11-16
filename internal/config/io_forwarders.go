package config

import (
	"context"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/slotmigrate"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
)

// Forwarders that keep the root package's unexported names compiling while
// the implementation lives in the io packages. Each forwarder is temporary:
// the io layer named in its comment will consume its callers directly in a
// later pass and delete the forwarder.

// io layer: cfgcommit.
func syncConfigParent(path string) error { return cfgcommit.SyncConfigParent(path) }

// io layer: cfgcommit.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return cfgcommit.AtomicWriteFile(path, data, perm)
}

// io layer: cfgcommit.
func syncConfigParentOnDisk(path string) error { return cfgcommit.SyncConfigParentOnDisk(path) }

// io layer: cfgcommit.
func acquireConfigFileLock(ctx context.Context, path string) (*filelock.FileLock, error) {
	return cfgcommit.AcquireConfigFileLock(ctx, path)
}

// io layer: cfgcommit.
func releaseConfigFileLock(path string, lock *filelock.FileLock) error {
	return cfgcommit.ReleaseConfigFileLock(path, lock)
}

// io layer: cfgcommit.
func configContextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return cfgcommit.ConfigContextWithTimeout(parent, timeout)
}

// io layer: cfgcommit.
func runConfigAfterMCPCommitHook(path string) { cfgcommit.RunConfigAfterMCPCommitHook(path) }

// io layer: cfgcommit.
func runConfigBeforeMCPReconcileHook(path string, data []byte) {
	cfgcommit.RunConfigBeforeMCPReconcileHook(path, data)
}

// io layer: cfgcommit.
func commitConfigFile(selectedPath, commitPath string, data []byte, perm os.FileMode, expected docread.Fingerprint, expectedOwner int, enforceOwner bool, migrationHooks ...func(string) error) (docread.Fingerprint, error) {
	return cfgcommit.CommitConfigFile(selectedPath, commitPath, data, perm, expected, expectedOwner, enforceOwner, migrationHooks...)
}

// io layer: cfgcommit.
func newCommitOutcome(path string, committed, reconciled bool, causes ...error) error {
	return cfgcommit.NewCommitOutcome(path, committed, reconciled, causes...)
}

// io layer: cfgcommit.
type CommitOutcome = cfgcommit.CommitOutcome

// io layer: cfgcommit.
func CommitOutcomeFromError(err error) (*CommitOutcome, bool) {
	return cfgcommit.CommitOutcomeFromError(err)
}

// io layer: cfgcommit (vars preserve single sentinel error identity).
var (
	errConfigCommitCommitted           = cfgcommit.ErrConfigCommitCommitted()
	errConfigCommitUncertain           = cfgcommit.ErrConfigCommitUncertain()
	errConfigCommitDurabilityUncertain = cfgcommit.ErrConfigCommitDurabilityUncertain()
	errAtomicWriteCommitted            = cfgcommit.ErrAtomicWriteCommitted()
)

// io layer: fileid (var preserves single error identity).
var errConfigCommitVerification = fileid.ErrConfigCommitVerification()

// io layer: cfgcommit.
func configEquivalentLeafNames(dir, left, right string) bool {
	return cfgcommit.ConfigEquivalentLeafNames(dir, left, right)
}

// io layer: cfgcommit.
func configDirectoryIsCaseInsensitive(dir string) bool {
	return cfgcommit.ConfigDirectoryIsCaseInsensitive(dir)
}

// configWriteTargetDedupKey converts the root configWriteTarget into the
// cfgcommit target and delegates. io layer: cfgcommit.
func configWriteTargetDedupKey(target configWriteTarget) string {
	ct := cfgcommit.NewConfigWriteTarget(target.path, target.selectedPath, target.lockPath, target.expected, target.owner, target.enforce)
	return cfgcommit.ConfigWriteTargetDedupKey(ct)
}

// configWriteTargetDedupKeyWithCaseSensitivity converts the root
// configWriteTarget and delegates. io layer: cfgcommit.
func configWriteTargetDedupKeyWithCaseSensitivity(target configWriteTarget, sensitivity configCaseSensitivity) string {
	ct := cfgcommit.NewConfigWriteTarget(target.path, target.selectedPath, target.lockPath, target.expected, target.owner, target.enforce)
	return cfgcommit.ConfigWriteTargetDedupKeyWithCaseSensitivity(ct, sensitivity)
}

// io layer: slots.
func writeTargetSlot(legacyPath string) string { return slots.WriteTargetSlot(legacyPath) }

// io layer: slots.
func readCacheDocument(legacyJSON string) ([]byte, error) {
	return slots.ReadCacheDocument(legacyJSON)
}

// io layer: slots.
func writeCacheDocument(legacyJSON string, jsonBytes []byte, perm os.FileMode, writeJSON func(path string, data []byte, perm os.FileMode) error) error {
	return slots.WriteCacheDocument(legacyJSON, jsonBytes, perm, writeJSON)
}

// io layer: slots.
func selectOAuthJournalPath(configPath, providerID string, forWrite bool) string {
	return slots.SelectOAuthJournalPath(configPath, providerID, forWrite)
}

// io layer: slots.
func ActiveConfigFileName(legacyPath string) string { return slots.ActiveConfigFileName(legacyPath) }

// io layer: slotmigrate.
func ktavMigrateStartupSlots(ctx context.Context, workingDir string) error {
	return slotmigrate.KtavMigrateStartupSlots(ctx, workingDir)
}

// io layer: slotmigrate.
func ktavMigrateWorkspaceIfNeeded(ctx context.Context, workingDir, dataDir string) error {
	return slotmigrate.KtavMigrateWorkspaceIfNeeded(ctx, workingDir, dataDir)
}

// io layer: slotmigrate.
func ktavMigrateOAuthJournalsAtStartup(ctx context.Context, configSlots []string, workingDir string) {
	slotmigrate.KtavMigrateOAuthJournalsAtStartup(ctx, configSlots, workingDir)
}

// io layer: slotmigrate.
func ktavMigrateCacheArtifactsStartup(ctx context.Context, workingDir string) {
	slotmigrate.KtavMigrateCacheArtifactsStartup(ctx, workingDir)
}

// io layer: slotmigrate.
func ktavMigrateOAuthJournal(ctx context.Context, configPath, providerID, workingDir string) error {
	return slotmigrate.KtavMigrateOAuthJournal(ctx, configPath, providerID, workingDir)
}

// io layer: slots. Exported wrapper for serve/cmd production callers.
func ReadConfigDocumentLight(legacyPath string) ([]byte, error) {
	return slots.ReadConfigDocumentLight(legacyPath)
}

// io layer: slots. Exported wrapper for serve/cmd production callers.
func KtavAutoMigrateEnabled() bool { return slots.KtavAutoMigrateEnabled() }

// io layer: slotmigrate.
func RunKtavMigrationStateLocked(ctx context.Context, legacy, workingDir string) error {
	return slotmigrate.RunKtavMigrationStateLocked(ctx, legacy, workingDir)
}

// io layer: slotmigrate. Root production reads the budget through this
// accessor because Go cannot forward a variable.
func ktavMigrationBudget() time.Duration { return slotmigrate.KtavMigrationBudget() }

// io layer: slotmigrate.
func migrateLegacyConfigFile(ctx context.Context, legacy, workingDir string) error {
	return slotmigrate.MigrateLegacyConfigFile(ctx, legacy, workingDir)
}

// io layer: cfgcommit.
func SetBeforeConfigCommitForTest(hook func()) func() {
	return cfgcommit.SetBeforeConfigCommitForTest(hook)
}

// io layer: cfgcommit.
func cloneCommitOutcome(outcome *CommitOutcome, reconciled bool) *CommitOutcome {
	return cfgcommit.CloneCommitOutcome(outcome, reconciled)
}
