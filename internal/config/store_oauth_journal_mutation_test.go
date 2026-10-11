package config

import (
	"context"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// Revert-check: initiator return must not clear pending before any worker runs.
func TestOAuthJournal_ReturnRetainsPendingBeforeWorker(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	old := oauthJournalBeforeWorker
	entered := make(chan struct{})
	oauthJournalBeforeWorker = func(ctx context.Context) { close(entered); <-ctx.Done() }
	t.Cleanup(func() { oauthJournalBeforeWorker = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	assertCodexPending(t, f, true)
	require.NoError(t, f.store.ReloadFromDisk(context.Background()))
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	f.store.stopOAuthJournalWorkers()
}
