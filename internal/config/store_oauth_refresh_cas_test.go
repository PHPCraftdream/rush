package config

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// Revert-check: refreshCodexOAuthLeader (:162) must refuse stale memory CAS before disk CAS.
func TestOAuthRefresh_MemoryReplacementPreventsStaleDiskWrite(t *testing.T) {
	f := newCodexRefreshFixture(t)
	replacement := *f.next
	replacement.AccessToken = "replacement-memory-access"
	replacement.RefreshToken = "replacement-memory-refresh"
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		pc := f.used
		pc.OAuthToken = &replacement
		f.store.SetProviderRuntimeConfig(pc.ID, pc)
		return f.next, nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	assertCodexRefreshTokens(t, f, &replacement, f.used.OAuthToken)
}

// Revert-check: refreshCodexOAuthLeader (:148) must not poll disk for transport errors.
func TestOAuthRefresh_TransportFailureDoesNotPoll(t *testing.T) {
	f := newCodexRefreshFixture(t)
	transportErr := errors.New("injected transport failure")
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return nil, transportErr })
	old := codexRefreshPollWait
	polls := 0
	codexRefreshPollWait = func(context.Context, time.Duration) error { polls++; return context.DeadlineExceeded }
	t.Cleanup(func() { codexRefreshPollWait = old })
	require.ErrorIs(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil), transportErr)
	require.Zero(t, polls)
	assertCodexRefreshTokens(t, f, f.used.OAuthToken, f.used.OAuthToken)
}
