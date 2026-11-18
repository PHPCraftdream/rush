package oauthcred

import (
	"context"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
)

// NewRotatingDescriptorForTest constructs a rotating provider descriptor for tests only.
func NewRotatingDescriptorForTest(id string, preserveAccountID bool, exchange RefreshFn) Descriptor {
	return Descriptor{
		id:                id,
		rotating:          true,
		preserveAccountID: preserveAccountID,
		exchange:          exchange,
	}
}

// WithExchangeForTest returns a copy with only the exchange function replaced.
func (d Descriptor) WithExchangeForTest(exchange RefreshFn) Descriptor {
	d.exchange = exchange
	return d
}

// LoadTokenFromDiskForTest loads the token from disk for tests only.
func (s *Store) LoadTokenFromDiskForTest(scope cfgmodel.Scope, provider string) (*oauth.Token, error) {
	return s.loadTokenFromDisk(scope, provider)
}

// RecordPendingOAuthLockedForTest records a pending rotation for tests only.
// The caller must hold the publish lock.
func (s *Store) RecordPendingOAuthLockedForTest(provider string, predecessor, token *oauth.Token) {
	s.recordPendingOAuthLocked(provider, predecessor, token)
}

// CodexRefreshActiveForTest reports whether this store has an active refresh
// for provider, for tests only. It locks the active refresh registry.
func (s *Store) CodexRefreshActiveForTest(provider string) bool {
	codexRefreshActive.Lock()
	defer codexRefreshActive.Unlock()
	_, active := codexRefreshActive.calls[codexRefreshKey{s, provider}]
	return active
}

// SetCodexRefreshWaiterHookForTest installs a refresh waiter hook for tests only.
// Use in serial tests before starting refresh workers; restore after they join.
func SetCodexRefreshWaiterHookForTest(fn func()) func() {
	prev := codexRefreshWaiterHook
	codexRefreshWaiterHook = fn
	return func() { codexRefreshWaiterHook = prev }
}

// SetCodexRefreshPollWaitForTest overrides only the refresh disk poll wait for
// tests. A nil hook uses the port wait. Use in serial tests before starting
// refresh workers; restore after they join.
func SetCodexRefreshPollWaitForTest(fn func(context.Context, time.Duration) error) func() {
	prev := codexRefreshPollWait
	codexRefreshPollWait = fn
	return func() { codexRefreshPollWait = prev }
}

// SetCodexRefreshPollTimingForTest sets the poll budget and interval for tests
// only. Use in serial tests before starting refresh workers; restore after they join.
func SetCodexRefreshPollTimingForTest(budget, interval time.Duration) func() {
	prevBudget, prevInterval := codexRefreshPollBudget, codexRefreshPollInterval
	codexRefreshPollBudget, codexRefreshPollInterval = budget, interval
	return func() { codexRefreshPollBudget, codexRefreshPollInterval = prevBudget, prevInterval }
}

// SettledForTest exposes the settled flag for tests only. Do not read it
// concurrently with updates to the state.
func (st *DiskSyncState) SettledForTest() bool { return st.settled }

// TickForTest exposes the tick count for tests only. Do not read it
// concurrently with updates to the state.
func (st *DiskSyncState) TickForTest() uint64 { return st.tick }

// OAuthDiskSyncStartedForTest reports the raw started flag for tests only,
// including after shutdown. It locks the disk sync start mutex.
func (s *Store) OAuthDiskSyncStartedForTest() bool {
	s.oauthDiskSyncMu.Lock()
	defer s.oauthDiskSyncMu.Unlock()
	return s.oauthDiskSyncStarted
}

// OAuthDiskSyncIntervalForTest returns the disk sync interval for tests only.
func OAuthDiskSyncIntervalForTest() time.Duration { return oauthDiskSyncInterval }

// OAuthDiskSyncDelayForTest samples the jittered disk sync delay for tests only.
func OAuthDiskSyncDelayForTest() time.Duration { return oauthDiskSyncDelay() }

// SetOAuthDiskSyncWaitForTest overrides only the disk sync wait for tests.
// A nil hook uses the port wait. Use in serial tests before starting disk sync;
// restore after disk sync stops.
func SetOAuthDiskSyncWaitForTest(fn func(context.Context, time.Duration) error) func() {
	prev := oauthDiskSyncWait
	oauthDiskSyncWait = fn
	return func() { oauthDiskSyncWait = prev }
}

// SetOAuthDiskSyncStatForTest overrides the disk sync stat for tests only.
// Use in serial tests before starting disk sync; restore after disk sync stops.
func SetOAuthDiskSyncStatForTest(fn func(string) (os.FileInfo, error)) func() {
	prev := oauthDiskSyncStat
	oauthDiskSyncStat = fn
	return func() { oauthDiskSyncStat = prev }
}
