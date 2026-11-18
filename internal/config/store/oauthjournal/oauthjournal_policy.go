package oauthjournal

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/cfgcommit"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

var OAuthJournalConfigSyncParent = cfgcommit.SyncConfigParent

// Only journal I/O receives bounded, context-cancelable retries.
func ReadJournalWithRetry(ctx context.Context, path string) (*docread.OAuthJournalRecord, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := ReadJournal(path)
		if !errors.Is(err, ErrOAuthJournalRead) || attempt >= len(OAuthJournalReadBackoffs) {
			return record, err
		}
		if err := OAuthJournalReadWait(ctx, OAuthJournalReadBackoffs[attempt]); err != nil {
			return nil, err
		}
	}
}

func DecodeOAuthJournalDiskToken(data []byte, providerID string) (*oauth.Token, error) {
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil || document == nil {
		return nil, ErrOAuthJournalDisk
	}
	providersData, exists := document["providers"]
	if !exists {
		return nil, nil
	}
	var providers map[string]json.RawMessage
	if json.Unmarshal(providersData, &providers) != nil || providers == nil {
		return nil, ErrOAuthJournalDisk
	}
	providerData, exists := providers[providerID]
	if !exists {
		return nil, nil
	}
	var provider map[string]json.RawMessage
	if json.Unmarshal(providerData, &provider) != nil || provider == nil {
		return nil, ErrOAuthJournalDisk
	}
	tokenData, exists := provider["oauth"]
	if !exists {
		return nil, nil
	}
	var token *oauth.Token
	if json.Unmarshal(tokenData, &token) != nil {
		return nil, ErrOAuthJournalDisk
	}
	if token != nil && token.AccessToken == "" {
		return nil, ErrOAuthJournalDisk
	}
	return token, nil
}
