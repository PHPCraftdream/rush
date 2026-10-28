package config

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestSetProviderAPIKeySerializesDiskWriteAndPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json")
	initial := &oauth.Token{AccessToken: "initial", RefreshToken: "initial-refresh"}
	store := newTestConfigStore(testStoreOpts{
		config: &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"p": {ID: "p", APIKey: initial.AccessToken, OAuthToken: initial},
		})},
		globalDataPath: path,
	})
	trackOAuthJournalTestStore(t, store)

	firstAtPublish := make(chan struct{})
	secondAtPublish := make(chan struct{})
	releaseFirst := make(chan struct{})
	var hooks atomic.Int32
	store.credentialSetBeforePublish = func() {
		switch hooks.Add(1) {
		case 1:
			close(firstAtPublish)
			<-releaseFirst
		case 2:
			close(secondAtPublish)
		}
	}

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		firstDone <- store.SetProviderAPIKey(ScopeGlobal, "p", &oauth.Token{AccessToken: "first", RefreshToken: "first-refresh"})
	}()
	<-firstAtPublish
	// Detect whether the first setter holds the serialization lock while its
	// publication is paused. If it does not, let the second setter reach its
	// post-disk-write hook before releasing the first; this deterministically
	// reproduces the stale publication ordering without scheduler delays.
	unprotected := store.credentialWriteMu.TryLock()
	if unprotected {
		store.credentialWriteMu.Unlock()
	}
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		secondDone <- store.SetProviderAPIKey(ScopeGlobal, "p", &oauth.Token{AccessToken: "second", RefreshToken: "second-refresh"})
	}()
	<-secondStarted
	if unprotected {
		<-secondAtPublish
		// Completion proves the second disk write and memory publication have
		// both happened before the first setter is released.
		require.NoError(t, <-secondDone)
	}
	close(releaseFirst)
	require.NoError(t, <-firstDone)
	if !unprotected {
		require.NoError(t, <-secondDone)
	}

	pc, ok := store.loadSnapshot().config.Providers.Get("p")
	require.True(t, ok)
	require.Equal(t, "second", pc.APIKey)
	require.Equal(t, "second", pc.OAuthToken.AccessToken)
	require.Equal(t, "second-refresh", pc.OAuthToken.RefreshToken)
	disk, err := store.loadTokenFromDisk(ScopeGlobal, "p")
	require.NoError(t, err)
	require.Equal(t, "second", disk.AccessToken)
	require.Equal(t, "second-refresh", disk.RefreshToken)
}
