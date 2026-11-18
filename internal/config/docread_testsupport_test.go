package config

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

// These private forwarders preserve the package-local names used by root
// config tests before document reading moved into docread. Production callers
// should use docread directly.
func lookupConfigCandidates(cwd string) []string {
	return docread.LookupConfigCandidates(cwd)
}

func configDocumentBytes(documents []stableConfigDocument) ([][]byte, []string, map[string]reloadFileFingerprint) {
	return docread.ConfigDocumentBytes(documents)
}

func eligibleConfigCandidate(path string, owner int) string {
	return docread.EligibleConfigCandidate(path, owner)
}

func pathAlreadyLoaded(loadedPaths []string, path string) bool {
	return docread.PathAlreadyLoaded(loadedPaths, path)
}

func configOwnerPolicyForPath(path, workingDir string) (int, bool, error) {
	return docread.ConfigOwnerPolicyForPath(path, workingDir)
}

func configAliasChainFingerprint(path string) [sha256.Size]byte {
	return docread.ConfigAliasChainFingerprint(path)
}

func reloadStalenessState(paths []string, fingerprints map[string]reloadFileFingerprint) ([]string, map[string]fileSnapshot) {
	return docread.ReloadStalenessState(paths, fingerprints)
}

func readReloadFingerprint(path string) (reloadFileFingerprint, error) {
	return docread.ReadReloadFingerprint(path)
}

func reloadFingerprintsChanged(before map[string]reloadFileFingerprint) bool {
	return docread.ReloadFingerprintsChangedWithOwner(before, nil)
}

func trackedSlotFingerprint(path string, fingerprints map[string]reloadFileFingerprint) (reloadFileFingerprint, error) {
	return docread.TrackedSlotFingerprint(path, fingerprints)
}

func normalizeReloadPath(path string) string {
	return docread.NormalizeReloadPath(path)
}

func normalizeDiscoveryPath(path string) string {
	return docread.NormalizeDiscoveryPath(path)
}

func readStableConfigDocuments(paths []string) ([]stableConfigDocument, error) {
	return docread.ReadStableConfigDocumentsWithOwner(paths, nil)
}

func readStableConfigFileOwned(path string, expectedOwner int, enforceOwner bool) ([]byte, reloadFileFingerprint, error) {
	return docread.ReadStableConfigFileOwned(path, expectedOwner, enforceOwner)
}

func readStableConfigFile(path string) ([]byte, reloadFileFingerprint, error) {
	return docread.ReadStableConfigFile(path)
}

func mcpJSONCandidatePaths(workingDir string) []string {
	return docread.McpJSONCandidatePaths(workingDir)
}

func mcpEntryFromJSON(data []byte, name string) (map[string]json.RawMessage, bool) {
	return docread.McpEntryFromJSON(data, name)
}

func decodeConfigDocument(path string, data []byte) ([]byte, error) {
	return docread.DecodeConfigDocument(path, data)
}

func ktavSiblingOf(path string) string { return docread.KtavSiblingOf(path) }

func backupPathFor(path string) string { return docread.BackupPathFor(path) }

func migrationRecordPathFor(path string) string { return docread.MigrationRecordPathFor(path) }

func isKtavConfigPath(path string) bool { return docread.IsKtavConfigPath(path) }

func selectConfigSlot(path string) string { return docread.SelectConfigSlot(path) }

func workspaceConfigPath(path string) string { return docread.WorkspaceConfigPath(path) }

func matchesLogicalSlot(path, slot string) bool { return docread.MatchesLogicalSlot(path, slot) }

func checkBackupOnlySlot(path string) error { return docread.CheckBackupOnlySlot(path) }

func checkAuthoritativeBackupOnly(workingDir string) error {
	return docread.CheckAuthoritativeBackupOnly(workingDir)
}

func ktavEntryExists(path string) bool { return docread.KtavEntryExists(path) }

func configWriteLockAnchor(path string) string { return docread.ConfigWriteLockAnchor(path) }

func fileExists(path string) bool { return docread.FileExists(path) }

const ktavConfigExt = ".ktav"

var (
	errKtavBackupOnly        = docread.ErrKtavBackupOnly()
	errKtavMigrationConflict = docread.ErrKtavMigrationConflict()
	errKtavMigrationRecord   = docread.ErrKtavMigrationRecord()
)

type settingsDocument = docread.SettingsDocument

func settingsDocumentFormatOf(path string) docread.SettingsDocumentFormat {
	return docread.SettingsDocumentFormatOf(path)
}

func decodeSettingsDocument(path string, raw []byte, fp reloadFileFingerprint) (docread.SettingsDocument, error) {
	return docread.DecodeSettingsDocument(path, raw, fp)
}

func readSettingsDocument(path string) (docread.SettingsDocument, error) {
	return docread.ReadSettingsDocument(path)
}

func encodeSettingsDocument(data []byte, format docread.SettingsDocumentFormat) ([]byte, error) {
	return docread.EncodeSettingsDocument(data, format)
}

func oauthJournalDigest(providerID string) string {
	return docread.OAuthJournalDigest(providerID)
}

func oauthJournalLegacyPath(configPath, providerID string) string {
	return docread.OAuthJournalLegacyPath(configPath, providerID)
}

func oauthJournalKtavPath(configPath, providerID string) string {
	return docread.OAuthJournalKtavPath(configPath, providerID)
}

func validOAuthJournal(record *docread.OAuthJournalRecord) bool {
	return docread.ValidOAuthJournal(record)
}

func mergeExternalMCPServersFromDocuments(cfg *Config, rush, external []stableConfigDocument) {
	docread.MergeExternalMCPServersFromDocuments(cfg, rush, external)
}
