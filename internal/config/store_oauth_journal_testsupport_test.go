package config

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Registration is test-scoped; cleanup never touches another test's stores.
var oauthJournalTestStores sync.Map

func trackOAuthJournalTestStore(t *testing.T, store *ConfigStore) {
	t.Helper()
	value, loaded := oauthJournalTestStores.LoadOrStore(t, []*ConfigStore{})
	if !loaded {
		t.Cleanup(func() { oauthJournalTestStores.Delete(t) })
	}
	stores := append(value.([]*ConfigStore), store)
	oauthJournalTestStores.Store(t, stores)
	t.Cleanup(func() {
		if value, ok := oauthJournalTestStores.Load(t); ok {
			for _, current := range value.([]*ConfigStore) {
				if current == store {
					store.stopOAuthJournalWorkers()
					return
				}
			}
		}
	})
}

// Scoped owners consume registration before performing their bounded join.
func consumeOAuthJournalTestStore(t *testing.T, store *ConfigStore) {
	t.Helper()
	if value, ok := oauthJournalTestStores.Load(t); ok {
		var remaining []*ConfigStore
		for _, current := range value.([]*ConfigStore) {
			if current != store {
				remaining = append(remaining, current)
			}
		}
		oauthJournalTestStores.Store(t, remaining)
	}
}

func joinOAuthJournalTestStoresBeforeRestore(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if value, ok := oauthJournalTestStores.Load(t); ok {
			for _, store := range value.([]*ConfigStore) {
				store.stopOAuthJournalWorkers()
			}
		}
	})
}

func hookOAuthJournalIdle(t *testing.T, store *ConfigStore) <-chan struct{} {
	t.Helper()
	done := make(chan struct{}, 32)
	old := oauthJournalWorkerIdle
	oauthJournalWorkerIdle = func(current *ConfigStore, _ string) {
		if current == store {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	}
	t.Cleanup(func() { oauthJournalWorkerIdle = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
	return done
}

func waitOAuthJournalIdle(t *testing.T, done <-chan struct{}) {
	t.Helper()
	awaitCodexSignal(t, done)
}

func hookOAuthJournalFastRetry(t *testing.T) {
	t.Helper()
	old := oauthJournalWorkerWait
	oauthJournalWorkerWait = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}
	t.Cleanup(func() { oauthJournalWorkerWait = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
}

func oldSyncOAuthJournalParent(path string, hook func(string) error) error {
	if hook != nil {
		return hook(path)
	}
	return syncConfigParentOnDisk(path)
}

func requireOAuthJournalToken(t *testing.T, f codexRefreshFixture) {
	t.Helper()
	record, err := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, f.next, record.Token)
}
