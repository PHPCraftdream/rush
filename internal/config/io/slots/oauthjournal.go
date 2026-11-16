// OAuth journal on-disk format: record shape, validation, tolerant reads and
// the digest-anchored journal path spellings.
package slots

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
)

// Config verification and journal writes may share a parent directory.
var oauthJournalConfigSyncParent = syncConfigParent

// Only the predecessor refresh-token digest is persisted, never its secret.
type oauthJournalRecord struct {
	PredecessorRefreshTokenSHA256 string       `json:"predecessor_refresh_token_sha256"`
	Token                         *oauth.Token `json:"token"`
	BurnedRefreshTokenSHA256      []string     `json:"burned_refresh_token_sha256,omitempty"`
}

var errOAuthJournalInvalid = errors.New("invalid OAuth journal")

func validOAuthJournal(record *oauthJournalRecord) bool {
	if record == nil || record.Token == nil || record.Token.AccessToken == "" || record.Token.RefreshToken == "" || record.Token.ExpiresAt <= 0 {
		return false
	}
	if record.PredecessorRefreshTokenSHA256 != "" {
		digest, err := hex.DecodeString(record.PredecessorRefreshTokenSHA256)
		if err != nil || len(digest) != sha256.Size {
			return false
		}
	}
	for _, burned := range record.BurnedRefreshTokenSHA256 {
		digest, err := hex.DecodeString(burned)
		if err != nil || len(digest) != sha256.Size {
			return false
		}
	}
	return true
}

// Only journal I/O receives bounded, context-cancelable retries.
func readOAuthJournalWithRetry(ctx context.Context, path string) (*oauthJournalRecord, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := readOAuthJournal(path)
		if !errors.Is(err, errOAuthJournalRead) || attempt >= len(oauthJournalReadBackoffs) {
			return record, err
		}
		if err := oauthJournalReadWait(ctx, oauthJournalReadBackoffs[attempt]); err != nil {
			return nil, err
		}
	}
}

func decodeOAuthJournalDiskToken(data []byte, providerID string) (*oauth.Token, error) {
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil || document == nil {
		return nil, errOAuthJournalDisk
	}
	providersData, exists := document["providers"]
	if !exists {
		return nil, nil
	}
	var providers map[string]json.RawMessage
	if json.Unmarshal(providersData, &providers) != nil || providers == nil {
		return nil, errOAuthJournalDisk
	}
	providerData, exists := providers[providerID]
	if !exists {
		return nil, nil
	}
	var provider map[string]json.RawMessage
	if json.Unmarshal(providerData, &provider) != nil || provider == nil {
		return nil, errOAuthJournalDisk
	}
	tokenData, exists := provider["oauth"]
	if !exists {
		return nil, nil
	}
	var token *oauth.Token
	if json.Unmarshal(tokenData, &token) != nil {
		return nil, errOAuthJournalDisk
	}
	if token != nil && token.AccessToken == "" {
		return nil, errOAuthJournalDisk
	}
	return token, nil
}

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
