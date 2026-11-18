package config

// X4 oauthcred wiring: credPort adapts *ConfigStore to the oauthcred.Port
// interface. It exposes exactly the existing ConfigStore primitives
// (loadSnapshot, publishMu, configPath, diskWriteMu, diskWriteGeneration, the
// OAuth journal component, and the root refresh test seams) without adding
// any new locking.

import (
	"context"
	"net/http"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/codex"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/copilot"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/hyper"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthcred"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
)

// copilotRefreshTokenFn, codexRefreshTokenFn, and hyperExchangeTokenFn
// indirect the OAuth refresh calls used by RefreshOAuthTokenWithClient
// (now in internal/config/store/oauthcred). They default to the real
// network-calling implementations; tests may override them
// (package-private, restored via t.Cleanup) to coordinate refresh races
// without making a real network call. The HTTP client carries the
// provider's network policy (nil = default route).
var (
	copilotRefreshTokenFn = copilot.RefreshTokenWithClient
	codexRefreshTokenFn   = codex.Refresh
	hyperExchangeTokenFn  = hyper.ExchangeTokenWithClient
)

// Serial tests may replace descriptors; restore only after workers join.
var oauthProviderDescriptorsOverride func() []oauthcred.Descriptor

// Root-owned serial refresh seams (the oauthcred component reads the lock
// budget through the port; BeforePersist is the oauthjournal persistence seam).
var (
	codexRefreshLockWait      = 30 * time.Second
	codexRefreshBeforePersist func(context.Context, int) error
)

// waitCodexRefresh forwards to the oauthcred helper the refresh machinery
// moved with (root store_api/store_config_disk_sync still call it).
func waitCodexRefresh(ctx context.Context, delay time.Duration) error {
	return oauthcred.WaitCodexRefresh(ctx, delay)
}

// Staying-root test aliases: the tests keep the original unexported type
// names for the oauthcred types they use.
type (
	oauthProviderDescriptor = oauthcred.Descriptor
	oauthDiskSyncState      = oauthcred.DiskSyncState
)

// oauthCredStore returns the store's OAuth credential component,
// constructing an ephemeral one over credPort on demand for stores whose
// constructor did not install one (e.g. zero-value test stores).
func (s *ConfigStore) oauthCredStore() *oauthcred.Store {
	if s.oauthCred != nil {
		return s.oauthCred
	}
	return oauthcred.NewStore(credPort{store: s})
}

type credPort struct {
	store *ConfigStore
}

// Compile-time assertion: credPort implements oauthcred.Port.
var _ oauthcred.Port = credPort{}

// credSnapshot exposes a storeSnapshot as an oauthcred.Snapshot.
type credSnapshot struct{ sn *storeSnapshot }

func (v credSnapshot) Config() *cfgmodel.Config             { return v.sn.config }
func (v credSnapshot) KnownProviders() []catwalk.Provider   { return v.sn.knownProviders }
func (v credSnapshot) Generation() uint64                   { return v.sn.generation }
func (v credSnapshot) Clone() oauthcred.Snapshot            { return credSnapshot{sn: v.sn.clone()} }
func (v credSnapshot) SetConfig(cfg *cfgmodel.Config)       { v.sn.config = cfg }

func (p credPort) ViewSnap(fn func(oauthcred.Snapshot) error) error {
	return fn(credSnapshot{sn: p.store.loadSnapshot()})
}

// PublishMutation holds the root publishMu for the whole callback.
func (p credPort) PublishMutation(fn func() error) error {
	p.store.publishMu.Lock()
	defer p.store.publishMu.Unlock()
	return fn()
}

func (p credPort) MutateConfig(mutate func(*cfgmodel.Config)) {
	p.store.publishMu.Lock()
	defer p.store.publishMu.Unlock()
	p.store.updateConfigLocked(mutate)
}

func (p credPort) ScopePath(scope cfgmodel.Scope) (string, error) { return p.store.configPath(scope) }
func (p credPort) GlobalDataPath() string                         { return p.store.globalDataPath }
func (p credPort) WorkingDir() string                             { return p.store.workingDir }

// DiskWrite holds diskWriteMu for the whole callback.
func (p credPort) DiskWrite(fn func() error) error {
	p.store.diskWriteMu.Lock()
	defer p.store.diskWriteMu.Unlock()
	return fn()
}

func (p credPort) NoteInitialLoadWrite(path string, data []byte) {
	p.store.noteInitialLoadWriteLocked(path, data)
}

func (p credPort) AutoReload(ctx context.Context) error { return p.store.autoReloadAfterWrite(ctx) }

func (p credPort) HasConfigField(scope cfgmodel.Scope, key string) bool {
	return p.store.HasConfigField(scope, key)
}

func (p credPort) SetConfigFields(scope cfgmodel.Scope, kv map[string]any) error {
	return p.store.SetConfigFields(scope, kv)
}

func (p credPort) RemoveConfigField(scope cfgmodel.Scope, key string) error {
	return p.store.RemoveConfigField(scope, key)
}

func (p credPort) WithFileWriteLock(path string, fn func(oauthcred.WriteTarget) error) error {
	return p.store.withConfigWriteLock(path, func(target configWriteTarget) error {
		return fn(credWriteTargetOf(target))
	})
}

func (p credPort) LoadSnapshot() oauthcred.Snapshot { return credSnapshot{sn: p.store.loadSnapshot()} }

func (p credPort) LockPublish()   { p.store.publishMu.Lock() }
func (p credPort) UnlockPublish() { p.store.publishMu.Unlock() }

// PublishSnapshot publishes a Clone()-derived snapshot; the caller holds
// publishMu (same publishLocked contract as before the move).
func (p credPort) PublishSnapshot(next oauthcred.Snapshot) {
	p.store.publishLocked(next.(credSnapshot).sn)
}

func (p credPort) Generation() uint64 { return p.store.Generation() }

func (p credPort) OAuthJournal() *oauthjournal.Store { return p.store.oauthJournal() }

func (p credPort) SetConfigFieldWithAuthority(scope cfgmodel.Scope, key string, value any, authority cfgmodel.SettingsWriteAuthority) error {
	return p.store.SetConfigFieldWithAuthority(scope, key, value, authority)
}

func (p credPort) SetConfigFieldsWithAuthority(scope cfgmodel.Scope, kv map[string]any, authority cfgmodel.SettingsWriteAuthority) error {
	return p.store.SetConfigFieldsWithAuthority(scope, kv, authority)
}

func (p credPort) RemoveConfigFieldWithAuthority(scope cfgmodel.Scope, key string, authority cfgmodel.SettingsWriteAuthority) error {
	return p.store.RemoveConfigFieldWithAuthority(scope, key, authority)
}

func (p credPort) TryLockDiskWrite() bool   { return p.store.diskWriteMu.TryLock() }
func (p credPort) UnlockDiskWrite()         { p.store.diskWriteMu.Unlock() }
func (p credPort) CommitDiskWrite()         { p.store.diskWriteGeneration++ }
func (p credPort) ConfigWriteLockTimeout() time.Duration { return configWriteLockTimeout }

func (p credPort) ResolveConfigWriteTarget(path string) (oauthcred.WriteTarget, error) {
	target, err := p.store.resolveConfigWriteTarget(path)
	if err != nil {
		return oauthcred.WriteTarget{}, err
	}
	return credWriteTargetOf(target), nil
}

func (p credPort) VerifyWriteTarget(target oauthcred.WriteTarget) error {
	return verifyConfigWriteTarget(credWriteTargetOfJournal(target))
}

func (p credPort) WaitCodexRefresh(ctx context.Context, delay time.Duration) error {
	return waitCodexRefresh(ctx, delay)
}

func (p credPort) CodexRefreshLockWait() time.Duration { return codexRefreshLockWait }

func (p credPort) CopilotRefreshTokenFn() oauthcred.RefreshFn { return copilotRefreshTokenFn }
func (p credPort) CodexRefreshTokenFn() oauthcred.RefreshFn   { return codexRefreshTokenFn }
func (p credPort) HyperExchangeTokenFn() oauthcred.RefreshFn  { return hyperExchangeTokenFn }

func (p credPort) OAuthProviderDescriptorsOverride() func() []oauthcred.Descriptor {
	return oauthProviderDescriptorsOverride
}

// credWriteTargetOf converts the root configWriteTarget to the oauthcred
// WriteTarget seam type (the journalWriteTargetOf pattern).
func credWriteTargetOf(t configWriteTarget) oauthcred.WriteTarget {
	return oauthcred.WriteTarget{Path: t.path, SelectedPath: t.selectedPath, LockPath: t.lockPath, Expected: t.expected, Owner: t.owner, Enforce: t.enforce}
}

func credWriteTargetOfJournal(t oauthcred.WriteTarget) configWriteTarget {
	return configWriteTarget{path: t.Path, selectedPath: t.SelectedPath, lockPath: t.LockPath, expected: t.Expected, owner: t.Owner, enforce: t.Enforce}
}

// oauthProviderDescriptors enumerates the OAuth provider descriptors through
// the component (nil port → default descriptors).
func oauthProviderDescriptors() []oauthcred.Descriptor {
	return oauthcred.NewStore(nil).Descriptors()
}

// ---------------------------------------------------------------------------
// X4 delegators: each forwards to the OAuth credential component
// (s.oauthCredStore), preserving the pre-move signatures.
// ---------------------------------------------------------------------------

func (s *ConfigStore) SetProviderAPIKey(scope Scope, providerID string, apiKey any) error {
	return s.oauthCredStore().SetProviderAPIKey(scope, providerID, apiKey)
}

func (s *ConfigStore) SetProviderAPIKeyWithAuthority(scope Scope, providerID string, apiKey any, authority SettingsWriteAuthority) error {
	return s.oauthCredStore().SetProviderAPIKeyWithAuthority(scope, providerID, apiKey, authority)
}

func (s *ConfigStore) SetProviderRuntimeConfig(providerID string, pc ProviderConfig) {
	s.oauthCredStore().SetProviderRuntimeConfig(providerID, pc)
}

func (s *ConfigStore) SetProviderRuntimeAPIKeyIfTemplate(providerID, template, expectedAPIKey, apiKey string) bool {
	return s.oauthCredStore().SetProviderRuntimeAPIKeyIfTemplate(providerID, template, expectedAPIKey, apiKey)
}

func (s *ConfigStore) RefreshOAuthToken(ctx context.Context, scope Scope, providerID string) error {
	return s.oauthCredStore().RefreshOAuthToken(ctx, scope, providerID)
}

func (s *ConfigStore) RefreshOAuthTokenWithClient(ctx context.Context, scope Scope, providerConfig ProviderConfig, client *http.Client) error {
	return s.oauthCredStore().RefreshOAuthTokenWithClient(ctx, scope, providerConfig, client)
}

func (s *ConfigStore) ImportCopilot() (*oauth.Token, bool) {
	return s.oauthCredStore().ImportCopilot()
}

func (s *ConfigStore) RemoveProviderAPIKey(scope Scope, providerID string) error {
	return s.oauthCredStore().RemoveProviderAPIKey(scope, providerID)
}

func (s *ConfigStore) RemoveProviderAPIKeyWithAuthority(scope Scope, providerID string, authority SettingsWriteAuthority) error {
	return s.oauthCredStore().RemoveProviderAPIKeyWithAuthority(scope, providerID, authority)
}

func (s *ConfigStore) StartOAuthDiskSync()  { s.oauthCredStore().StartOAuthDiskSync() }
func (s *ConfigStore) StopOAuthDiskSync()   { s.oauthCredStore().StopOAuthDiskSync() }
func (s *ConfigStore) OAuthDiskSyncRunning() bool {
	return s.oauthCredStore().OAuthDiskSyncRunning()
}

// Unexported forwarders for staying-root tests and root callers.

func (s *ConfigStore) setOAuthTokenIfCurrent(scope Scope, providerID string, expected, token *oauth.Token) (bool, error) {
	return s.oauthCredStore().SetOAuthTokenIfCurrent(scope, providerID, expected, token)
}

func (s *ConfigStore) applyOAuthToken(providerID string, expected, token *oauth.Token) error {
	return s.oauthCredStore().ApplyOAuthToken(providerID, expected, token)
}

func (s *ConfigStore) applyOAuthTokenWithPendingPredecessor(providerID string, expected, token, predecessor *oauth.Token, pending bool, records ...*oauthJournalRecord) error {
	return s.oauthCredStore().ApplyOAuthTokenWithPendingPredecessor(providerID, expected, token, predecessor, pending, records...)
}

func (s *ConfigStore) syncOAuthFromDisk(ctx context.Context, st *oauthDiskSyncState) {
	s.oauthCredStore().SyncOAuthFromDisk(ctx, st)
}

func (s *ConfigStore) adoptCodexDiskAuthority(ctx context.Context, path, provider string, used, expected, disk *oauth.Token, readErr error, logoutAllowed bool) (bool, error) {
	return s.oauthCredStore().AdoptCodexDiskAuthority(ctx, path, provider, used, expected, disk, readErr, logoutAllowed)
}

// overlayPendingOAuthLocked applies the pending rotations to the candidate
// snapshot (the X4 back-call from the root reload publish path).
func (s *ConfigStore) overlayPendingOAuthLocked(candidate *storeSnapshot) {
	candidate.config = s.oauthCredStore().OverlayPendingRotation(candidate.config)
}
