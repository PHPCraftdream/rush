// Format-aware OAuth journal paths: journals follow the config slot's disk
// format, and the refresh sidecars anchor on the legacy JSON spelling.
package slots

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
