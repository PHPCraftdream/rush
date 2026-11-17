package config

import (
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
)

// This file holds only rule-18 identity state, root-target converters, and
// permanent exported facade API. No temporary forwarders remain.

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

// io layer: slots. Exported wrapper for serve/cmd production callers.
func ReadConfigDocumentLight(legacyPath string) ([]byte, error) {
	return slots.ReadConfigDocumentLight(legacyPath)
}

// Exported facade API for external layers (serve/cmd): forwards to io/slots.
func ActiveConfigFileName(legacyPath string) string {
	return slots.ActiveConfigFileName(legacyPath)
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

// io layer: slots. Exported wrapper for agent test callers.
func SetKtavAutoMigrateForTest(on bool) func() {
	return slots.SetKtavAutoMigrateForTest(on)
}
