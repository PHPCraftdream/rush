package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthRound8_OwnedEqualDiskUnsyncableConfigJournalsFirst(t *testing.T) {
	runOAuthRound8OwnedEqualDisk(t, false)
}

func TestOAuthRound8_IntermittentConfigSyncDefersOwnedCompletion(t *testing.T) {
	runOAuthRound8OwnedEqualDisk(t, true)
}

// Direct worker attempts acquire the production refresh sidecar.
func runOAuthRound8OwnedEqualDisk(t *testing.T, intermittent bool) {
	t.Helper()
	a := &oauth.Token{AccessToken: "fixture-A-access", RefreshToken: "fixture-A-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	b := &oauth.Token{AccessToken: "fixture-B-access", RefreshToken: "fixture-B-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	provider := "openai-codex"
	path := filepath.Join(t.TempDir(), "rush.json")
	writeCodexRefreshDisk(t, path, b)
	pc := ProviderConfig{ID: provider, OAuthToken: a}
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{provider: pc})},
		globalDataPath: path,
	})
	record := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(a), Token: b}
	entry := store.InstallOAuthOutbox(path, ScopeGlobal, provider, record)
	require.NoError(t, store.ApplyOAuthTokenWithPendingPredecessor(provider, a, b, a, true))
	journal := oauthJournalPath(path, provider)
	_, err := os.Stat(journal)
	require.True(t, os.IsNotExist(err))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	oldSync, oldWrite := oauthjournal.OAuthJournalConfigSyncParent, oauthjournal.OAuthJournalAtomicWrite
	t.Cleanup(func() {
		oauthjournal.OAuthJournalConfigSyncParent, oauthjournal.OAuthJournalAtomicWrite = oldSync, oldWrite
	})
	var events []string
	verificationCalls, writes := 0, 0
	repaired := false
	oauthjournal.OAuthJournalConfigSyncParent = func(parent string) error {
		verificationCalls++
		events = append(events, "config-verification")
		if !repaired && (!intermittent || verificationCalls == 1) {
			return os.ErrPermission
		}
		return oldSync(parent)
	}
	oauthjournal.OAuthJournalAtomicWrite = func(target string, data []byte, mode os.FileMode) error {
		if target == journal {
			writes++
			events = append(events, "journal-write")
		}
		return oldWrite(target, data, mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done, attemptErr := store.OAuthJournalWorkerAttempt(ctx, ScopeGlobal, provider, 0)
	assert.False(t, done, "failed verification cannot complete ownership")
	assert.Error(t, attemptErr)
	assert.Equal(t, 1, writes, "healthy journal must persist owned B on the first attempt")
	assert.NotEmpty(t, events)
	if len(events) > 0 {
		assert.Equal(t, "journal-write", events[0], "owned journal must precede config verification")
	}
	wal, readErr := readOAuthJournal(journal)
	assert.NoError(t, readErr)
	assert.Equal(t, record, wal, "verification failure must retain durable B")
	assert.Same(t, entry, store.OAuthOutboxCurrent(path, provider))
	store.PublishMuForTest().Lock()
	pending, pendingOK := store.OAuthCredStore().PendingRotationForTest(t, provider)
	store.PublishMuForTest().Unlock()
	assert.True(t, pendingOK, "verification failure must retain pending B")
	assert.Equal(t, *b, pending.Token)
	live, _ := store.Config().Providers.Get(provider)
	assert.Equal(t, b, live.OAuthToken)
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after)

	if !intermittent {
		return
	}
	repaired = true
	done, attemptErr = store.OAuthJournalWorkerAttempt(ctx, ScopeGlobal, provider, 1)
	assert.NoError(t, attemptErr)
	assert.True(t, done, "only the later verified attempt may complete ownership")
	assert.GreaterOrEqual(t, verificationCalls, 2)
	assert.Nil(t, store.OAuthOutboxCurrent(path, provider))
	store.PublishMuForTest().Lock()
	_, pendingOK = store.OAuthCredStore().PendingRotationForTest(t, provider)
	store.PublishMuForTest().Unlock()
	assert.False(t, pendingOK)
	_, err = os.Stat(journal)
	assert.True(t, os.IsNotExist(err), "verified completion removes the active journal")
}
