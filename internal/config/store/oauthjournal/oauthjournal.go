// Package oauthjournal holds the OAuth journal recovery component: the
// pending-rotation WAL outbox, the crash-recovery worker, and quarantine
// handling. All root-store access goes through Port.
package oauthjournal

import (
	"context"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
)

// OAuthDescriptor describes one provider the recovery worker must consider.
type OAuthDescriptor struct {
	ID       string
	Rotating bool
}

// WriteTarget mirrors the root store's configWriteTarget: one resolved
// physical config write destination as seen by the write-lock machinery.
type WriteTarget struct {
	Path         string
	SelectedPath string
	LockPath     string
	Expected     docread.Fingerprint
	Owner        int
	Enforce      bool
}

// View is a read-only sample of the root store's published snapshot.
type View interface {
	Config() *cfgmodel.Config
}

// MutationScope is handed to PublishMutation callbacks with the root
// publishMu held for the whole callback.
type MutationScope interface {
	Current() View
	// Publish publishes the next generation via publishLocked.
	Publish(cfg *cfgmodel.Config)
	// PendingOAuthDigests returns the predecessor and burned refresh digests
	// of the root pendingOAuth entry for provider.
	PendingOAuthDigests(provider string) []string
	// DeletePendingOAuthIfToken deletes the root pendingOAuth entry for
	// provider only when its token matches; publishMu must be held.
	DeletePendingOAuthIfToken(provider string, token *oauth.Token) bool
}

// Port exposes exactly the root ConfigStore primitives this component needs.
type Port interface {
	// ViewSnap samples the store's published snapshot exactly once per
	// callback so every view read inside fn sees one consistent generation.
	ViewSnap(fn func(View) error) error
	// PublishMutation holds the root publishMu for the whole callback.
	PublishMutation(fn func(MutationScope) error) error
	// WithPublishMu holds the root publishMu for the callback; plain
	// guarded sections only, no publishing inside.
	WithPublishMu(fn func())
	ScopePath(scope cfgmodel.Scope) (string, error)
	ApplyOAuthToken(providerID string, expected, token *oauth.Token) error
	ApplyOAuthTokenWithPendingPredecessor(providerID string, expected, token, predecessor *oauth.Token, pending bool, records ...*docread.OAuthJournalRecord) error
	TryLockDiskWrite() bool
	UnlockDiskWrite()
	ConfigWriteLockTimeout() time.Duration
	CodexRefreshLockWait() time.Duration
	WaitCodexRefresh(ctx context.Context, d time.Duration) error
	CodexRefreshBeforePersist() func(context.Context, int) error
	// OAuthJournalWorkerDone / OAuthJournalWorkerIdle forward to the root
	// store's *ConfigStore-typed test hooks.
	OAuthJournalWorkerDone(provider string)
	OAuthJournalWorkerIdle(provider string)
	OAuthRotatingProviders() []OAuthDescriptor
	ResolveConfigWriteTarget(path string) (WriteTarget, error)
	WithCodexConfigWriteLock(ctx context.Context, path string, target WriteTarget, fn func(WriteTarget) error) error
	SetOAuthTokenIfCurrentWithLock(scope cfgmodel.Scope, providerID string, expected, token *oauth.Token, withLock func(string, func(WriteTarget) error) error) (bool, error)
	// TokenProviderConfig mirrors the root's oauthTokenProviderConfig helper.
	TokenProviderConfig(providerID string, pc cfgmodel.ProviderConfig, token *oauth.Token) cfgmodel.ProviderConfig
	// ErrCredentialCAS returns the shared root sentinel so errors.Is
	// identity is preserved across the boundary.
	ErrCredentialCAS() error
	// DeletePendingOAuthIfToken deletes the root pendingOAuth entry for
	// provider only when its token matches; publishMu must be held.
	DeletePendingOAuthIfToken(provider string, token *oauth.Token) bool
}

// Store is the OAuth journal recovery component.
type Store struct {
	port Port

	oauthWorkerMu     sync.Mutex
	oauthWorkerCtx    context.Context
	oauthWorkerCancel context.CancelFunc
	oauthWorkers      map[string]*Worker
	oauthWorkerWG     sync.WaitGroup
	oauthOutbox       map[Key]*Entry
}

// NewStore builds the component over the given port.
func NewStore(p Port) *Store {
	return &Store{port: p}
}
