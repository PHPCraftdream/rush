// Format-aware OAuth journal paths: journals follow the config slot's disk
// format, and the refresh sidecars anchor on the legacy JSON spelling.
package config

import (
	"crypto/sha256"
	"encoding/hex"
)

// oauthJournalKtavPath returns the Ktav spelling of a provider's pending
// journal: rush.ktav.oauth-pending-<hash>.ktav, anchored on the ACTIVE
// Ktav sibling of the legacy config slot. (KTAV-4: automatic conversion
// of existing legacy JSON journals is deferred.)
func oauthJournalKtavPath(configPath, providerID string) string {
	legacy := legacySlotOf(configPath)
	digest := sha256.Sum256([]byte(providerID))
	return normalizeReloadPath(ktavSiblingOf(legacy)) + ".oauth-pending-" + hex.EncodeToString(digest[:8]) + ".ktav"
}

// selectOAuthJournalPath resolves the journal path one operation uses. An
// existing Ktav journal always wins over the legacy JSON spelling, like
// config slots. When only the legacy journal exists it stays the target —
// automatic conversion is KTAV-4 work — and when NEITHER exists, a WRITE
// creates the Ktav spelling only when newConfigFileIsKtav() is true; reads
// then simply observe the absent legacy name. An invalid preferred journal
// is an error upstream (and is quarantined), never re-parsed as the other
// format.
func selectOAuthJournalPath(configPath, providerID string, forWrite bool) string {
	legacy := oauthJournalPath(configPath, providerID)
	ktav := oauthJournalKtavPath(configPath, providerID)
	if ktavEntryExists(ktav) {
		return ktav
	}
	if ktavEntryExists(legacy) {
		return legacy
	}
	if forWrite && newConfigFileIsKtav() {
		return ktav
	}
	return legacy
}
