package config

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

var oauthJournalConfigSyncParent = syncConfigParent

type oauthJournalRecord = docread.OAuthJournalRecord

// Only journal I/O receives bounded, context-cancelable retries.
func readOAuthJournalWithRetry(ctx context.Context, path string) (*docread.OAuthJournalRecord, error) {
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
