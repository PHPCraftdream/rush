package config

import (
	"context"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/slotmigrate"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
)

// This file holds (a) the remaining temporary root forwarders (slots +
// slotmigrate, completed in a later pass) and (b) permanent exported facade
// API for external layers.

// io layer: cfgcommit.
type CommitOutcome = cfgcommit.CommitOutcome

// io layer: cfgcommit (vars preserve single sentinel error identity).
var (
	errConfigCommitCommitted           = cfgcommit.ErrConfigCommitCommitted()
	errConfigCommitUncertain           = cfgcommit.ErrConfigCommitUncertain()
	errConfigCommitDurabilityUncertain = cfgcommit.ErrConfigCommitDurabilityUncertain()
	errAtomicWriteCommitted            = cfgcommit.ErrAtomicWriteCommitted()
)

// io layer: fileid (var preserves single error identity).
var errConfigCommitVerification = fileid.ErrConfigCommitVerification()

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

// Exported facade API for external layers (serve/cmd, agent): forwards to io/cfgcommit.
func CommitOutcomeFromError(err error) (*CommitOutcome, bool) {
	return cfgcommit.CommitOutcomeFromError(err)
}

// Exported facade API for external layers (serve/cmd, agent): forwards to io/cfgcommit.
func SetBeforeConfigCommitForTest(hook func()) func() {
	return cfgcommit.SetBeforeConfigCommitForTest(hook)
}

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
