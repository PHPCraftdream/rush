// Format-aware OAuth journal paths: journals follow the config slot's disk
// format, and the refresh sidecars anchor on the legacy JSON spelling.
package slots

import "github.com/PHPCraftdream/rush/internal/config/io/docread"

// selectOAuthJournalPath resolves the journal path one operation uses. An
// existing Ktav journal always wins over the legacy JSON spelling, like
// config slots. When only the legacy journal exists it stays the target —
// automatic conversion is KTAV-4 work — and when NEITHER exists, a WRITE
// creates the Ktav spelling only when NewConfigFileIsKtav() is true; reads
// then simply observe the absent legacy name. The legacy candidate anchors
// on the LEGACY rush.json spelling so a rush.ktav config discovers journals
// written before migration. An invalid preferred journal is an error
// upstream (and is quarantined), never re-parsed as the other format.
func selectOAuthJournalPath(configPath, providerID string, forWrite bool) string {
	legacy := docread.OAuthJournalLegacyPath(configPath, providerID)
	ktav := docread.OAuthJournalKtavPath(configPath, providerID)
	if docread.KtavEntryExists(ktav) {
		return ktav
	}
	if docread.KtavEntryExists(legacy) {
		return legacy
	}
	if forWrite && NewConfigFileIsKtav() {
		return ktav
	}
	return legacy
}

// SelectOAuthJournalPath resolves the journal path one operation uses.
// Exported wrapper over selectOAuthJournalPath.
func SelectOAuthJournalPath(configPath, providerID string, forWrite bool) string {
	return selectOAuthJournalPath(configPath, providerID, forWrite)
}
