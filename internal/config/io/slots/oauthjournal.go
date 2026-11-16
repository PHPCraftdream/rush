// OAuth journal on-disk format: tolerant reads and the digest-anchored
// journal path spellings.
package slots

import "github.com/PHPCraftdream/rush/internal/config/io/docread"

// oauthJournalDigest returns the documented 8-byte hex suffix for a
// provider's journal and sidecar names.
func oauthJournalDigest(providerID string) string {
	return docread.OAuthJournalDigest(providerID)
}

// oauthJournalLegacyPath returns the LEGACY JSON spelling of a provider's
// pending journal: rush.json.oauth-pending-<hash>.json. It anchors on the
// LEGACY slot name even for a rush.ktav config, so a migrated config still
// discovers journals written before migration.
func oauthJournalLegacyPath(configPath, providerID string) string {
	return docread.OAuthJournalLegacyPath(configPath, providerID)
}

// oauthJournalKtavPath returns the Ktav spelling of a provider's pending
// journal: rush.ktav.oauth-pending-<hash>.ktav, anchored on the ACTIVE
// Ktav sibling of the legacy config slot. (KTAV-4: automatic conversion
// of existing legacy JSON journals is deferred.)
func oauthJournalKtavPath(configPath, providerID string) string {
	return docread.OAuthJournalKtavPath(configPath, providerID)
}
