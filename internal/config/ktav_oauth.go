// Format-aware OAuth journal paths: journals follow the config slot's disk
// format, and the refresh sidecars anchor on the legacy JSON spelling.
package config

import (
	"crypto/sha256"
	"encoding/hex"
)

// oauthJournalDigest returns the documented 8-byte hex suffix for a
// provider's journal and sidecar names.
func oauthJournalDigest(providerID string) string {
	digest := sha256.Sum256([]byte(providerID))
	return hex.EncodeToString(digest[:8])
}

// oauthJournalLegacyPath returns the LEGACY JSON spelling of a provider's
// pending journal: rush.json.oauth-pending-<hash>.json. It anchors on the
// LEGACY slot name even for a rush.ktav config, so a migrated config still
// discovers journals written before migration.
func oauthJournalLegacyPath(configPath, providerID string) string {
	return normalizeReloadPath(legacySlotOf(configPath)) + ".oauth-pending-" + oauthJournalDigest(providerID) + ".json"
}

// oauthJournalKtavPath returns the Ktav spelling of a provider's pending
// journal: rush.ktav.oauth-pending-<hash>.ktav, anchored on the ACTIVE
// Ktav sibling of the legacy config slot. (KTAV-4: automatic conversion
// of existing legacy JSON journals is deferred.)
func oauthJournalKtavPath(configPath, providerID string) string {
	return normalizeReloadPath(ktavSiblingOf(legacySlotOf(configPath))) + ".oauth-pending-" + oauthJournalDigest(providerID) + ".ktav"
}

// selectOAuthJournalPath resolves the journal path one operation uses. An
// existing Ktav journal always wins over the legacy JSON spelling, like
// config slots. When only the legacy journal exists it stays the target —
// automatic conversion is KTAV-4 work — and when NEITHER exists, a WRITE
// creates the Ktav spelling only when newConfigFileIsKtav() is true; reads
// then simply observe the absent legacy name. The legacy candidate anchors
// on the LEGACY rush.json spelling so a rush.ktav config discovers journals
// written before migration. An invalid preferred journal is an error
// upstream (and is quarantined), never re-parsed as the other format.
func selectOAuthJournalPath(configPath, providerID string, forWrite bool) string {
	legacy := oauthJournalLegacyPath(configPath, providerID)
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
