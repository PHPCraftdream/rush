// Local byte-identical copies of OAuth-journal helpers that live in the
// root config package. slotmigrate sits below the root package in the
// dependency graph (io/slots imports slotmigrate), so these cannot be
// imported; they must be kept in sync with store_oauth_journal*.go and
// store_oauth_provider.go.
package slotmigrate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/catalog"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

var (
	errKtavOAuthJournalRead = errors.New("OAuth journal cannot be read")
	// Bounded retries mirror config's oauthJournalReadBackoffs.
	ktavOAuthJournalReadBackoffs = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	ktavOAuthJournalReadFile     = os.ReadFile
)

// oauthRefreshLockPath mirrors config.oauthRefreshLockPath: the SAME
// legacy-anchored refresh sidecar lock the recovery worker uses.
func oauthRefreshLockPath(path, provider string) string {
	digest := sha256.Sum256([]byte(provider))
	return fmt.Sprintf("%s.oauth-refresh-%x.lock", docread.NormalizeReloadPath(docread.ConfigWriteLockAnchor(path)), digest[:8])
}

// ktavOAuthProviderInfo carries only the fields the artifact migrator needs.
type ktavOAuthProviderInfo struct {
	id       string
	rotating bool
}

// ktavOAuthProviderDescriptors mirrors config.oauthProviderDescriptors.
// Keep in sync: copilot reuses its credential; hyper rotates.
func ktavOAuthProviderDescriptors() []ktavOAuthProviderInfo {
	return []ktavOAuthProviderInfo{
		{id: "openai-codex", rotating: true},
		{id: string(catwalk.InferenceProviderCopilot), rotating: false},
		{id: catalog.HyperName, rotating: true},
	}
}

// ktavOAuthJournalRecord is docread.OAuthJournalRecord.
type ktavOAuthJournalRecord = docread.OAuthJournalRecord

// readOAuthJournal mirrors config.readOAuthJournal: missing journals are
// normal; unreadable or corrupt journals are kept.
func readOAuthJournal(path string) (*ktavOAuthJournalRecord, error) {
	data, err := ktavOAuthJournalReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errKtavOAuthJournalRead
	}
	jsonBytes, decodeErr := docread.DecodeConfigDocument(path, data)
	if decodeErr != nil {
		return nil, docread.ErrOAuthJournalInvalid()
	}
	var record ktavOAuthJournalRecord
	if json.Unmarshal(jsonBytes, &record) != nil || !docread.ValidOAuthJournal(&record) {
		return nil, docread.ErrOAuthJournalInvalid()
	}
	return &record, nil
}

// readOAuthJournalWithRetry mirrors config.readOAuthJournalWithRetry: only
// journal I/O receives bounded, context-cancelable retries.
func readOAuthJournalWithRetry(ctx context.Context, path string) (*ktavOAuthJournalRecord, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := readOAuthJournal(path)
		if !errors.Is(err, errKtavOAuthJournalRead) || attempt >= len(ktavOAuthJournalReadBackoffs) {
			return record, err
		}
		wait := time.NewTimer(ktavOAuthJournalReadBackoffs[attempt])
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil, ctx.Err()
		case <-wait.C:
		}
	}
}
