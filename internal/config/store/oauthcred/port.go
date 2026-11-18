// Package oauthcred owns provider OAuth credential management (X4 move from
// package config): API key set/remove, OAuth token refresh with
// cross-session disk recovery, pending-rotation bookkeeping, provider
// descriptors, and the OAuth disk-sync poller. It reaches the root config
// store exclusively through the Port interface, so the dependency never
// inverts back into package config.
package oauthcred

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/codex"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/copilot"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/hyper"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
)

// RefreshFn is the signature of the indirected OAuth refresh/exchange calls
// (the root package's copilotRefreshTokenFn/codexRefreshTokenFn/
// hyperExchangeTokenFn test seams).
type RefreshFn = func(ctx context.Context, client *http.Client, refresh string) (*oauth.Token, error)

// Snapshot is the immutable published store snapshot, sampled through the
// port. Clone/SetConfig preserve the root copy-on-write publish contract.
type Snapshot interface {
	Config() *cfgmodel.Config
	KnownProviders() []catwalk.Provider
	Generation() uint64
	// Clone returns a shallow copy of the snapshot; SetConfig may then be
	// called on the clone before PublishSnapshot.
	Clone() Snapshot
	SetConfig(*cfgmodel.Config)
}

// WriteTarget is the config write-target seam (the root configWriteTarget /
// oauthjournal.WriteTarget shape).
type WriteTarget struct {
	Path         string
	SelectedPath string
	LockPath     string
	Expected     docread.Fingerprint
	Owner        int
	Enforce      bool
}

// Port is the root-store surface oauthcred is allowed to call. It exposes
// exactly the existing ConfigStore primitives (publishMu, loadSnapshot,
// configPath, diskWriteMu, diskWriteGeneration, the OAuth journal component,
// and the root refresh test seams) without adding any new locking.
type Port interface {
	// ViewSnap samples the published snapshot exactly once per callback.
	ViewSnap(fn func(Snapshot) error) error
	// PublishMutation runs fn with publishMu held.
	PublishMutation(fn func() error) error
	// MutateConfig applies a copy-on-write config mutation under publishMu.
	MutateConfig(mutate func(*cfgmodel.Config))
	// ScopePath resolves the logical slot to its physical path.
	ScopePath(scope cfgmodel.Scope) (string, error)
	GlobalDataPath() string
	WorkingDir() string
	// DiskWrite runs fn with diskWriteMu held.
	DiskWrite(fn func() error) error
	// NoteInitialLoadWrite requires diskWriteMu to be held and may be called
	// only from inside a write-lock callback.
	NoteInitialLoadWrite(path string, data []byte)
	// AutoReload reconciles memory after a committed disk write.
	AutoReload(ctx context.Context) error
	HasConfigField(scope cfgmodel.Scope, key string) bool
	SetConfigFields(scope cfgmodel.Scope, kv map[string]any) error
	RemoveConfigField(scope cfgmodel.Scope, key string) error
	// WithFileWriteLock runs fn under the root stable-target write lock.
	WithFileWriteLock(path string, fn func(WriteTarget) error) error

	// --- X4 additions: root primitives the moved bodies call directly ---
	LoadSnapshot() Snapshot
	// LockPublish/UnlockPublish expose the root publishMu for bodies that
	// hold it across a read-modify-publish cycle.
	LockPublish()
	UnlockPublish()
	// PublishSnapshot publishes a Clone()-derived snapshot (assigns the next
	// generation). The caller must hold publishMu.
	PublishSnapshot(next Snapshot)
	Generation() uint64
	// OAuthJournal returns the OAuth journal recovery component. Callers
	// marked *Locked require publishMu to be held by the caller.
	OAuthJournal() *oauthjournal.Store
	SetConfigFieldWithAuthority(scope cfgmodel.Scope, key string, value any, authority cfgmodel.SettingsWriteAuthority) error
	SetConfigFieldsWithAuthority(scope cfgmodel.Scope, kv map[string]any, authority cfgmodel.SettingsWriteAuthority) error
	RemoveConfigFieldWithAuthority(scope cfgmodel.Scope, key string, authority cfgmodel.SettingsWriteAuthority) error
	TryLockDiskWrite() bool
	UnlockDiskWrite()
	ResolveConfigWriteTarget(path string) (WriteTarget, error)
	VerifyWriteTarget(target WriteTarget) error
	ConfigWriteLockTimeout() time.Duration
	// CommitDiskWrite fences reloads: bumps the disk write generation after a
	// committed config write. The caller must already hold diskWriteMu.
	CommitDiskWrite()
	WaitCodexRefresh(ctx context.Context, delay time.Duration) error
	CodexRefreshLockWait() time.Duration
	CopilotRefreshTokenFn() RefreshFn
	CodexRefreshTokenFn() RefreshFn
	HyperExchangeTokenFn() RefreshFn
	// OAuthProviderDescriptorsOverride returns the root test seam override
	// (nil when unset).
	OAuthProviderDescriptorsOverride() func() []Descriptor
}

// Store is the OAuth credential component over one core store port.
type Store struct {
	port Port

	// pendingOAuth holds in-flight rotation records awaiting publication.
	// Guarded by the root publishMu (via port LockPublish/UnlockPublish).
	pendingOAuth map[string]PendingRotation

	// credentialWriteMu serializes credential disk writes through their
	// in-memory publication (root-level contract preserved).
	credentialWriteMu sync.Mutex
	// credentialSetBeforePublish is a test seam; guarded by credentialWriteMu.
	credentialSetBeforePublish func()

	oauthDiskSyncMu sync.Mutex
	// oauthDiskSyncStarted is guarded by oauthDiskSyncMu.
	oauthDiskSyncStarted bool
}

// NewStore builds the OAuth credential component over the given core port.
// A nil port yields a store usable only for descriptor enumeration.
func NewStore(p Port) *Store { return &Store{port: p} }

// PendingRotation records a rotation whose publication is still pending:
// the predecessor token, the rotated token, and the journal record.
type PendingRotation struct {
	Predecessor oauth.Token
	Token       oauth.Token
	Record      docread.OAuthJournalRecord
}

// PendingRotationForTest returns the pending rotation for provider, if any.
// The caller must hold the root publishMu.
func (s *Store) PendingRotationForTest(t testing.TB, provider string) (PendingRotation, bool) {
	if t != nil {
		t.Helper()
	}
	entry, ok := s.pendingOAuth[provider]
	return entry, ok
}

// SetCredentialSetBeforePublishForTest installs the credentialSetBeforePublish
// test seam; restore with a nil function.
func (s *Store) SetCredentialSetBeforePublishForTest(t testing.TB, fn func()) {
	if t != nil {
		t.Helper()
	}
	s.credentialSetBeforePublish = fn
}

// TryLockCredentialWriteMuForTest probes the credential serialization mutex
// (call UnlockCredentialWriteMuForTest if it returns true).
func (s *Store) TryLockCredentialWriteMuForTest(t testing.TB) bool {
	if t != nil {
		t.Helper()
	}
	return s.credentialWriteMu.TryLock()
}

// UnlockCredentialWriteMuForTest releases the credential serialization mutex.
func (s *Store) UnlockCredentialWriteMuForTest(t testing.TB) {
	if t != nil {
		t.Helper()
	}
	s.credentialWriteMu.Unlock()
}

// PendingDigestsLocked returns the predecessor/burned refresh-token digests
// of the pending rotation for provider. publishMu must be held.
func (s *Store) PendingDigestsLocked(provider string) []string {
	var digests []string
	if pending, ok := s.pendingOAuth[provider]; ok {
		digests = append(digests, pending.Record.PredecessorRefreshTokenSHA256)
		digests = append(digests, pending.Record.BurnedRefreshTokenSHA256...)
	}
	return digests
}

// DeletePendingOAuthIfTokenLocked deletes the pending rotation for provider
// iff its token matches. publishMu must be held.
func (s *Store) DeletePendingOAuthIfTokenLocked(provider string, token *oauth.Token) bool {
	if pending, ok := s.pendingOAuth[provider]; ok && reflect.DeepEqual(&pending.Token, token) {
		delete(s.pendingOAuth, provider)
		return true
	}
	return false
}

// TokenProviderConfig applies token to the provider entry (journal port seam).
func (s *Store) TokenProviderConfig(providerID string, pc cfgmodel.ProviderConfig, token *oauth.Token) cfgmodel.ProviderConfig {
	return s.oauthTokenProviderConfig(providerID, pc, token)
}

// ErrCredentialCAS returns the shared credential-CAS error.
func (s *Store) ErrCredentialCAS() error { return errOAuthCredentialCAS }

// Exported wrappers over the moved unexported methods, for root-side
// delegators/forwarders (signatures preserved from the pre-move root).

func (s *Store) SetOAuthTokenIfCurrent(scope cfgmodel.Scope, providerID string, expected, token *oauth.Token) (bool, error) {
	return s.setOAuthTokenIfCurrent(scope, providerID, expected, token)
}

func (s *Store) ApplyOAuthToken(providerID string, expected, token *oauth.Token) error {
	return s.applyOAuthToken(providerID, expected, token)
}

func (s *Store) ApplyOAuthTokenWithPendingPredecessor(providerID string, expected, token, predecessor *oauth.Token, pending bool, records ...*docread.OAuthJournalRecord) error {
	return s.applyOAuthTokenWithPendingPredecessor(providerID, expected, token, predecessor, pending, records...)
}

func (s *Store) SyncOAuthFromDisk(ctx context.Context, st *DiskSyncState) {
	s.syncOAuthFromDisk(ctx, st)
}

func (s *Store) AdoptCodexDiskAuthority(ctx context.Context, path, provider string, used, expected, disk *oauth.Token, readErr error, logoutAllowed bool) (bool, error) {
	return s.adoptCodexDiskAuthority(ctx, path, provider, used, expected, disk, readErr, logoutAllowed)
}

func (s *Store) SetOAuthTokenIfCurrentWithLock(scope cfgmodel.Scope, providerID string, expected, token *oauth.Token, withLock func(string, func(WriteTarget) error) error) (bool, error) {
	return s.setOAuthTokenIfCurrentWithLock(scope, providerID, expected, token, withLock)
}

func (s *Store) WithCodexConfigWriteLock(ctx context.Context, path string, pinned WriteTarget, fn func(WriteTarget) error) error {
	return s.withCodexConfigWriteLock(ctx, path, pinned, fn)
}

// ID returns the descriptor provider ID.
func (d Descriptor) ID() string { return d.id }

// Rotating reports whether the provider burns refresh tokens on rotation.
func (d Descriptor) Rotating() bool { return d.rotating }

// Descriptors enumerates the OAuth provider descriptors, honoring the root
// override seam when one is installed.
func (s *Store) Descriptors() []Descriptor { return s.oauthProviderDescriptors() }

// nil-safe port access for descriptor enumeration over a port-less store.
func (s *Store) descriptorsOverride() func() []Descriptor {
	if s.port == nil {
		return nil
	}
	return s.port.OAuthProviderDescriptorsOverride()
}

func (s *Store) copilotRefreshTokenFn() RefreshFn {
	if s.port == nil {
		return copilot.RefreshTokenWithClient
	}
	return s.port.CopilotRefreshTokenFn()
}

func (s *Store) codexRefreshTokenFn() RefreshFn {
	if s.port == nil {
		return codex.Refresh
	}
	return s.port.CodexRefreshTokenFn()
}

func (s *Store) hyperExchangeTokenFn() RefreshFn {
	if s.port == nil {
		return hyper.ExchangeTokenWithClient
	}
	return s.port.HyperExchangeTokenFn()
}

// WaitCodexRefresh is the shared interruptible sleep helper (moved with the
// refresh machinery; the root package forwards to it).
func WaitCodexRefresh(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
