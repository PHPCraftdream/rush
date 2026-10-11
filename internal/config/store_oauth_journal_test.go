package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// Revert-check: a successful rotation returns with a durable journal before config persistence.
func TestOAuthJournal_RefreshReturnsWithDurableJournal(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		return f.next, nil
	})

	entered, release := pauseCodexPendingPersist(t)
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, f.used, nil))
	_, err = os.Stat(oauthJournalPath(f.path, f.used.ID))
	require.NoError(t, err, "durable WAL must exist when refresh returns")
	awaitCodexSignal(t, entered)
	after, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.Equal(t, before, after, "refresh must return without editing the blocked config")
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)

	digest := sha256.Sum256([]byte(f.used.ID))
	journalPath := fmt.Sprintf("%s.oauth-pending-%x.json", normalizeReloadPath(f.path), digest[:8])
	require.Equal(t, filepath.Dir(normalizeReloadPath(f.path)), filepath.Dir(journalPath))
	data, err := os.ReadFile(journalPath)
	require.NoError(t, err, "successful refresh must leave a journal before config persistence succeeds")
	var journal struct {
		Token oauth.Token `json:"token"`
	}
	require.NoError(t, json.Unmarshal(data, &journal))
	require.Equal(t, *f.next, journal.Token, "journal must contain the complete rotated token B")
	assertCodexPending(t, f, true)
	require.NoError(t, f.store.ReloadFromDisk(ctx))
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	assertCodexPending(t, f, true)
}
