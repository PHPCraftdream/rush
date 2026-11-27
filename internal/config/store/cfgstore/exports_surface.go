// Package cfgstore: the config store engine (snapshots, reload, disk sync,
// MCP/OAuth write paths, settings staleness). Formed by a pure byte-preserving
// merge of the original 16 leaf files; see docs/plans/2026-11-26-cfgstore-ledger.tsv.

package cfgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/codex"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/copilot"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/hyper"
	"github.com/PHPCraftdream/rush/internal/config/domains/peak"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"
	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	catalog "github.com/PHPCraftdream/rush/internal/config/providers/catalog"
	"github.com/PHPCraftdream/rush/internal/config/store/mcpstore"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthcred"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
)

// Seam pointer vars: root tests swap these through the pointers so the
// production call sites observe the swap (identity preserved).
var (
	CodexRefreshTokenFn           = &codexRefreshTokenFn
	CopilotRefreshTokenFn         = &copilotRefreshTokenFn
	HyperExchangeTokenFn          = &hyperExchangeTokenFn
	OauthProviderDescriptorsOverr = &oauthProviderDescriptorsOverride
	ConfigDiskSyncWait            = &configDiskSyncWait
	ConfigDiskSyncRandInt63       = &configDiskSyncRandInt63
	ConfigDiskSyncFirstDelay      = &configDiskSyncFirstDelay
	ConfigDiskSyncInterval        = &configDiskSyncInterval
	CodexRefreshLockWait          = &codexRefreshLockWait
	ConfigDiskSyncAfterTick       = &configDiskSyncAfterTick
)

// Identity-preserving error exports.
var (
	ErrKtavWriteNotSupported         = errKtavWriteNotSupported
	ErrReloadProviderEndpointDropped = errReloadProviderEndpointDropped
)

// Constants re-exported for the root tests.
const (
	ConfigWriteLockTimeout         = configWriteLockTimeout
	InternalConfigWriteLockTimeout = internalConfigWriteLockTimeout
)

// Type aliases for unexported helper types the root tests reference.
type (
	// MCPAdmissionReadSeeker aliases the admission verifier's reader type.
	MCPAdmissionReadSeeker = mcpAdmissionReadSeeker
	// MCPJSONObject aliases the MCP JSON scanner value type.
	MCPJSONObject = mcpJSONObject
	// MCPJSONMember aliases the MCP JSON member type.
	MCPJSONMember = mcpJSONMember
	// ConfigDiskSyncState aliases the disk-sync poller state.
	ConfigDiskSyncState = configDiskSyncState
	// StoreSnapshot aliases the internal snapshot type.
	StoreSnapshot = storeSnapshot
	// OAuthDiskSyncState aliases the oauth disk-sync state alias.
	OAuthDiskSyncState = oauthDiskSyncState
	// MCPLockedFilesAlias aliases mcpLockedFiles for wrapper signatures.
	MCPLockedFilesAlias = mcpLockedFiles
	// MCPJSONEditAlias aliases mcpJSONEdit for wrapper signatures.
	MCPJSONEditAlias = mcpJSONEdit
)

// Seam function wrappers (forward unchanged).
func WaitCodexRefresh(ctx context.Context, delay time.Duration) error {
	return waitCodexRefresh(ctx, delay)
}

func ConfigDiskSyncDelay() time.Duration { return configDiskSyncDelay() }

func DefaultConfigDiskSyncFirstDelay() time.Duration { return defaultConfigDiskSyncFirstDelay() }

func OAuthProviderDescriptors() []oauthcred.Descriptor { return oauthProviderDescriptors() }

func ApplyMCPJSONEdits(data []byte, edits ...MCPJSONEditAlias) []byte {
	return applyMCPJSONEdits(data, edits...)
}

func CloneOptions(o *Options) *Options          { return cloneOptions(o) }
func CloneTUIOptions(t *TUIOptions) *TUIOptions { return cloneTUIOptions(t) }

func ConfigMCPAdmissionLockGroups(targets []ConfigWriteTarget, caseSensitivity func(string) casefold.ConfigCaseSensitivity) ([]*mcpstore.LockGroup, map[string]mcpstore.LockTarget, map[string]string) {
	return configMCPAdmissionLockGroups(targets, caseSensitivity)
}

func DataFingerprint(path string, data []byte) docread.Fingerprint {
	return dataFingerprint(path, data)
}

func EditMCPDocument(data []byte, path, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	return editMCPDocument(data, path, operation, oldName, newName, value, disabled, mutate)
}

func EntryOrigins(origins map[string]MCPOrigin, path, workspacePath, globalPath, systemPath string, data []byte) {
	entryOrigins(origins, path, workspacePath, globalPath, systemPath, data)
}

func LastMCPMember(members []MCPJSONMember, key string) *MCPJSONMember {
	return lastMCPMember(members, key)
}

func MCPCommitWasReconciled(err error) bool                { return mcpCommitWasReconciled(err) }
func MCPContainerName(path string) string                  { return mcpContainerName(path) }
func RejectUnconvertedKtavWrite(path string) error         { return rejectUnconvertedKtavWrite(path) }
func ScanMCPJSONObject(data []byte) (MCPJSONObject, error) { return scanMCPJSONObject(data) }
func ScanMCPJSONValue(data []byte, offset int) (int, *MCPJSONObject, error) {
	return scanMCPJSONValue(data, offset)
}

func UpdateMCPFile(files *MCPLockedFilesAlias, path, name string, mutate func(map[string]any) error) error {
	return updateMCPFile(files, path, name, mutate)
}

func ValidateFinalMCPAdmissionFile(ctx context.Context, path string, file *os.File, expected docread.Fingerprint, expectedOwner int, enforceOwner bool) error {
	return validateFinalMCPAdmissionFile(ctx, path, file, expected, expectedOwner, enforceOwner)
}

func ValidatePreparedAdmissionFile(ctx context.Context, path string, file *os.File, expected docread.Fingerprint, expectedOwner int, enforceOwner bool) error {
	return validatePreparedAdmissionFile(ctx, path, file, expected, expectedOwner, enforceOwner)
}

func VerifyMCPAdmissionBytes(ctx context.Context, file MCPAdmissionReadSeeker, expectedSize int64, expectedDigest [32]byte) error {
	return verifyMCPAdmissionBytes(ctx, file, expectedSize, expectedDigest)
}

func VerifyConfigTarget(target ConfigWriteTarget) error { return verifyConfigTarget(target) }

func VerifyConfigTargetBinding(target ConfigWriteTarget, actual reloadFileFingerprint, err error) error {
	return verifyConfigTargetBinding(target, actual, err)
}

// ConfigDiskSyncState white-box helpers for the root boundary tests.
func NewConfigDiskSyncStateForTest(force bool) ConfigDiskSyncState {
	st := configDiskSyncState{}
	st.force = force
	return st
}

func ConfigDiskSyncStateForce(st *ConfigDiskSyncState) *bool { return &st.force }

func ConfigDiskSyncStateReloadAttempts(st *ConfigDiskSyncState) *uint64 {
	return &st.reloadAttempts
}

// TestStoreOpts configures NewTestConfigStore (root shim target).
type TestStoreOpts struct {
	Config         *Config
	GlobalDataPath string
	WorkspacePath  string
	Resolver       VariableResolver
	LoadedPaths    []string
}

// NewTestConfigStore replicates the root newTestConfigStore helper.
func NewTestConfigStore(opts TestStoreOpts) *ConfigStore {
	return newTestConfigStore(testStoreOpts{
		config:         opts.Config,
		globalDataPath: opts.GlobalDataPath,
		workspacePath:  opts.WorkspacePath,
		resolver:       opts.Resolver,
		loadedPaths:    opts.LoadedPaths,
	})
}

// NewDiskSyncWhiteBoxStore replicates the leaf test helper
// configSyncWhiteBoxStore for the root boundary tests (the helper itself
// lives in this package's test files and cannot be reached from the root).
func NewDiskSyncWhiteBoxStore(t *testing.T, dir string, cfg *Config, paths []string) (*ConfigStore, *atomic.Int32) {
	t.Helper()
	fingerprints := make(map[string]reloadFileFingerprint, len(paths))
	for _, path := range paths {
		fingerprint, err := docread.ReadReloadFingerprint(path)
		if err != nil {
			t.Fatalf("read fingerprint %s: %v", path, err)
		}
		fingerprints[docread.NormalizeDiscoveryPath(path)] = fingerprint
	}
	s := &ConfigStore{workingDir: dir, globalDataPath: paths[len(paths)-1]}
	tracked, snapshots := docread.ReloadStalenessState(paths, fingerprints)
	s.snap.Store(&storeSnapshot{
		config:             cfg,
		trackedConfigPaths: tracked,
		snapshots:          snapshots,
		workspacePath:      "",
		mcpRevisions:       initialMCPRevisions(cfg),
		resolverRevision:   1,
	})
	return s, &atomic.Int32{}
}

// syncStartedCounter mirrors the leaf test helper's started counter.
func syncStartedCounter() *atomic.Int32 { return &atomic.Int32{} }

// ---------------------------------------------------------------------------
// ConfigStore method wrappers forwarding to the unexported originals.
// ---------------------------------------------------------------------------

func (s *ConfigStore) ApplyOAuthToken(providerID string, expected, token *oauth.Token) error {
	return s.applyOAuthToken(providerID, expected, token)
}

func (s *ConfigStore) ApplyOAuthTokenWithPendingPredecessor(providerID string, expected, token, predecessor *oauth.Token, pending bool, records ...*docread.OAuthJournalRecord) error {
	return s.applyOAuthTokenWithPendingPredecessor(providerID, expected, token, predecessor, pending, records...)
}

func (s *ConfigStore) AutoReloadAfterWrite(ctx context.Context) error {
	return s.autoReloadAfterWrite(ctx)
}

func (s *ConfigStore) ConfigPathOrEmpty(scope Scope) string { return s.configPathOrEmpty(scope) }

func (s *ConfigStore) FlushOAuthOutbox(ctx context.Context, path, provider string) error {
	return s.flushOAuthOutbox(ctx, path, provider)
}

func (s *ConfigStore) InstallOAuthOutbox(path string, scope Scope, provider string, record *docread.OAuthJournalRecord) *oauthjournal.Entry {
	return s.installOAuthOutbox(path, scope, provider, record)
}

func (s *ConfigStore) LoadSnapshot() *StoreSnapshot { return s.loadSnapshot() }

func (s *ConfigStore) MCPPathData(files *mcpLockedFiles, path string) ([]byte, bool, error) {
	return s.mcpPathData(files, path)
}

func (s *ConfigStore) NoteInitialLoadWriteLocked(path string, data []byte) {
	s.noteInitialLoadWriteLocked(path, data)
}

func (s *ConfigStore) OAuthCredStore() *oauthcred.Store { return s.oauthCredStore() }

func (s *ConfigStore) OAuthJournal() *oauthjournal.Store { return s.oauthJournal() }

func (s *ConfigStore) OAuthJournalWorkerAttempt(ctx context.Context, scope Scope, provider string, attempt int) (bool, error) {
	return s.oauthJournalWorkerAttempt(ctx, scope, provider, attempt)
}

func (s *ConfigStore) OAuthOutboxCurrent(path, provider string) *oauthjournal.Entry {
	return s.oauthOutboxCurrent(path, provider)
}

func (s *ConfigStore) OverlayPendingOAuthLocked(candidate *StoreSnapshot) {
	s.overlayPendingOAuthLocked(candidate)
}

func (s *ConfigStore) PrepareMCPFileMutation(files *mcpLockedFiles, path, operation, oldName, newName string, value MCPConfig, disabled *bool) error {
	return s.prepareMCPFileMutation(files, path, operation, oldName, newName, value, disabled)
}

func (s *ConfigStore) RecoverCodexOAuthJournal(ctx context.Context, scope Scope, provider string) (*oauth.Token, *docread.OAuthJournalRecord, error) {
	return s.recoverCodexOAuthJournal(ctx, scope, provider)
}

func (s *ConfigStore) ResolveConfigWriteTarget(path string) (ConfigWriteTarget, error) {
	return s.resolveConfigWriteTarget(path)
}

func (s *ConfigStore) SettleOAuthOutbox(ctx context.Context, path, provider string, entry *oauthjournal.Entry) (bool, error) {
	return s.settleOAuthOutbox(ctx, path, provider, entry)
}

func (s *ConfigStore) StartOAuthJournalWorker(scope Scope, provider string) {
	s.startOAuthJournalWorker(scope, provider)
}

func (s *ConfigStore) StopOAuthJournalWorkers() { s.stopOAuthJournalWorkers() }

func (s *ConfigStore) SyncConfigFromDisk(ctx context.Context, st *ConfigDiskSyncState) (ConfigDiskSyncResult, error) {
	return s.syncConfigFromDisk(ctx, st)
}

func (s *ConfigStore) SyncOAuthFromDisk(ctx context.Context, st *OAuthDiskSyncState) {
	s.syncOAuthFromDisk(ctx, st)
}

func (s *ConfigStore) UpdateConfig(mutate func(cfgCopy *Config)) { s.updateConfig(mutate) }

func (s *ConfigStore) WithConfigWriteLock(path string, fn func(ConfigWriteTarget) error) error {
	return s.withConfigWriteLock(path, fn)
}

// ---------------------------------------------------------------------------
// White-box field accessors (pointer-returning) for the root tests.
// ---------------------------------------------------------------------------

// ReloadMutex exposes the reload lock type for pointer accessors.
type ReloadMutex = reloadMutex

func (s *ConfigStore) PublishMuForTest() *sync.Mutex        { return &s.publishMu }
func (s *ConfigStore) ReloadMuForTest() *ReloadMutex        { return &s.reloadMu }
func (s *ConfigStore) ReloadPendingMuForTest() *sync.Mutex  { return &s.reloadPendingMu }
func (s *ConfigStore) DiskWriteMuForTest() *sync.Mutex      { return &s.diskWriteMu }
func (s *ConfigStore) WorkingDirForTest() *string           { return &s.workingDir }
func (s *ConfigStore) GlobalDataPathForTest() *string       { return &s.globalDataPath }
func (s *ConfigStore) DataDirSourceForTest() *DataDirSource { return &s.dataDirSource }
func (s *ConfigStore) SystemConfigPathOverrideForTest() *string {
	return &s.systemConfigPathOverride
}
func (s *ConfigStore) CliModelsForTest() *func() []catalog.LocalCLIModel { return &s.cliModels }
func (s *ConfigStore) ReloadPendingForTest() *bool                       { return &s.reloadPending }
func (s *ConfigStore) ReloadPendingWaiterForTest() *bool                 { return &s.reloadPendingWaiter }

func (s *ConfigStore) InitialLoadFingerprintsForTest() *map[string]docread.Fingerprint {
	return &s.initialLoadFingerprints
}
func (s *ConfigStore) ReloadAfterDiskReadForTest() *func() { return &s.reloadAfterDiskRead }
func (s *ConfigStore) ReloadAfterExternalReadForTest() *func() {
	return &s.reloadAfterExternalRead
}
func (s *ConfigStore) ReloadBeforePublishForTest() *func() { return &s.reloadBeforePublish }
func (s *ConfigStore) ReloadBeforeFinalInputCheckForTest() *func() {
	return &s.reloadBeforeFinalInputCheck
}
func (s *ConfigStore) ReloadAfterWriteQueuedForTest() *func() { return &s.reloadAfterWriteQueued }
func (s *ConfigStore) ReloadResolverExpanderForTest() *Expander {
	return &s.reloadResolverExpander
}

func (sn *StoreSnapshot) SnapConfig() *Config             { return sn.config }
func (sn *StoreSnapshot) SnapResolver() *VariableResolver { return &sn.resolver }
func (sn *StoreSnapshot) SnapResolverDynamic() *bool      { return &sn.resolverDynamic }

func (sn *StoreSnapshot) SnapKnownProviders() *[]catwalk.Provider { return &sn.knownProviders }

func (sn *StoreSnapshot) SnapLoadedPaths() *[]string { return &sn.loadedPaths }

func (sn *StoreSnapshot) SnapTrackedConfigPaths() *[]string { return &sn.trackedConfigPaths }

func (sn *StoreSnapshot) SnapSnapshots() *map[string]docread.FileSnapshot { return &sn.snapshots }

func (sn *StoreSnapshot) SnapWorkspacePath() *string { return &sn.workspacePath }

func (sn *StoreSnapshot) SnapGeneration() *uint64 { return &sn.generation }

func (sn *StoreSnapshot) SnapMCPRevisions() *map[string]uint64 { return &sn.mcpRevisions }

func (sn *StoreSnapshot) SnapMCPInputs() *map[string][32]byte { return &sn.mcpInputs }

func (sn *StoreSnapshot) SnapResolverRevision() *uint64 { return &sn.resolverRevision }

func (sn *StoreSnapshot) SnapResolverFingerprint() *[32]byte { return &sn.resolverFingerprint }

func (sn *StoreSnapshot) SnapOverrides() *RuntimeOverrides { return &sn.overrides }
func (sn *StoreSnapshot) Clone() *StoreSnapshot            { return sn.clone() }

// Additional method wrappers for remaining root test call sites.
func (s *ConfigStore) RecordRecentModelForTest(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	return s.recordRecentModel(scope, modelType, model)
}

func (s *ConfigStore) WithMCPAdmissionLocks(fn func(*MCPLockedFilesAlias) error) error {
	return s.withMCPAdmissionLocks(fn)
}

func (s *ConfigStore) WithMCPLocksUsingCaseSensitivity(ctx context.Context, fn func(*MCPLockedFilesAlias) error, caseSensitivity func(string) casefold.ConfigCaseSensitivity) error {
	return s.withMCPLocksUsingCaseSensitivity(ctx, fn, caseSensitivity)
}

func (s *ConfigStore) EvaluateMCPFiles(files *MCPLockedFilesAlias) (mcpstore.Evaluation, error) {
	return s.evaluateMCPFiles(files)
}

func (s *ConfigStore) SetOAuthTokenIfCurrent(scope Scope, providerID string, expected, token *oauth.Token) (bool, error) {
	return s.setOAuthTokenIfCurrent(scope, providerID, expected, token)
}
func (s *ConfigStore) AnySettingsLockExists() bool          { return s.anySettingsLockExists() }
func (s *ConfigStore) PublishLocked(next *StoreSnapshot)    { s.publishLocked(next) }
func (s *ConfigStore) OrderedMCPPaths() []string            { return s.orderedMCPPaths() }
func (s *ConfigStore) AutoReload(ctx context.Context) error { return s.autoReload(ctx) }
func (s *ConfigStore) ApplyCommittedMCPFingerprint(files *MCPLockedFilesAlias, record *mcpstore.FileRecord, committed docread.Fingerprint) error {
	return s.applyCommittedMCPFingerprint(files, record, committed)
}

// Seam pointer vars (second batch).
var (
	OauthJournalWorkerDone    = &oauthJournalWorkerDone
	OauthJournalWorkerIdle    = &oauthJournalWorkerIdle
	CodexRefreshBeforePersist = &codexRefreshBeforePersist
)

// configWriteTarget white-box accessors and constructor for the root tests.
func NewConfigWriteTargetForTest(path, selectedPath, lockPath string, expected reloadFileFingerprint, owner int, enforce bool) ConfigWriteTarget {
	return configWriteTarget{path: path, selectedPath: selectedPath, lockPath: lockPath, expected: expected, owner: owner, enforce: enforce}
}
func (t configWriteTarget) WTPath() string                    { return t.path }
func (t configWriteTarget) WTSelectedPath() string            { return t.selectedPath }
func (t configWriteTarget) WTLockPath() string                { return t.lockPath }
func (t configWriteTarget) WTExpected() reloadFileFingerprint { return t.expected }
func (t configWriteTarget) WTOwner() int                      { return t.owner }
func (t configWriteTarget) WTEnforce() bool                   { return t.enforce }

func (t *configWriteTarget) WTPathForTest() *string                    { return &t.path }
func (t *configWriteTarget) WTSelectedPathForTest() *string            { return &t.selectedPath }
func (t *configWriteTarget) WTLockPathForTest() *string                { return &t.lockPath }
func (t *configWriteTarget) WTExpectedForTest() *reloadFileFingerprint { return &t.expected }
func (t *configWriteTarget) WTOwnerForTest() *int                      { return &t.owner }
func (t *configWriteTarget) WTEnforceForTest() *bool                   { return &t.enforce }

func (sn *StoreSnapshot) SnapConfigForTest() **Config { return &sn.config }

// Config disk-sync white-box accessors for the root lifecycle tests.
func (s *ConfigStore) ConfigSyncMuForTest() *sync.Mutex              { return &s.configSyncMu }
func (s *ConfigStore) ConfigSyncOptsForTest() *ConfigDiskSyncOptions { return &s.configSyncOpts }
func (s *ConfigStore) ConfigSyncCtxForTest() *context.Context        { return &s.configSyncCtx }
func (s *ConfigStore) ConfigSyncStartedForTest() *bool               { return &s.configSyncStarted }

func (s *ConfigStore) ConfigSyncStoppedForTest() *bool { return &s.configSyncStopped }

// ErrReloadDiskChanged preserves the leaf reload-instability sentinel identity
// for the root retry loop (root fingerprints_policy.go aliases this var).
var ErrReloadDiskChanged = errReloadDiskChanged

func (s *ConfigStore) MCPStoreForTest() *mcpstore.Store { return s.mcpStore() }

// X4 oauthcred wiring: credPort adapts *ConfigStore to the oauthcred.Port
// interface. It exposes exactly the existing ConfigStore primitives
// (loadSnapshot, publishMu, configPath, diskWriteMu, diskWriteGeneration, the
// OAuth journal component, and the root refresh test seams) without adding
// any new locking.

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

func (v credSnapshot) Config() *cfgmodel.Config           { return v.sn.config }
func (v credSnapshot) KnownProviders() []catwalk.Provider { return v.sn.knownProviders }
func (v credSnapshot) Generation() uint64                 { return v.sn.generation }
func (v credSnapshot) Clone() oauthcred.Snapshot          { return credSnapshot{sn: v.sn.clone()} }
func (v credSnapshot) SetConfig(cfg *cfgmodel.Config)     { v.sn.config = cfg }

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

func (p credPort) TryLockDiskWrite() bool                { return p.store.diskWriteMu.TryLock() }
func (p credPort) UnlockDiskWrite()                      { p.store.diskWriteMu.Unlock() }
func (p credPort) CommitDiskWrite()                      { p.store.diskWriteGeneration++ }
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

func (s *ConfigStore) StartOAuthDiskSync() { s.oauthCredStore().StartOAuthDiskSync() }
func (s *ConfigStore) StopOAuthDiskSync()  { s.oauthCredStore().StopOAuthDiskSync() }
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

// ReloadMaxAttempts re-exports the reload retry budget (root
// store_reload.go reloadMaxAttempts); the root load loop aliases it.
const ReloadMaxAttempts = reloadMaxAttempts

// MCPConfigFor re-exports mcpConfig for the root mcpdocs_policy.go reader
// (named MCPConfigFor to avoid colliding with the MCPConfig type alias).
func MCPConfigFor(cfg *Config, name string) (MCPConfig, bool) { return mcpConfig(cfg, name) }

// ConfigPath re-exports configPath for the root mcpdocs_policy.go reader.
func (s *ConfigStore) ConfigPath(scope Scope) (string, error) { return s.configPath(scope) }

// RecoverOAuthJournalsAtLoad re-exports recoverOAuthJournalsAtLoad for the
// root load loop.
func (s *ConfigStore) RecoverOAuthJournalsAtLoad() { s.recoverOAuthJournalsAtLoad() }

// RemoveConfigFieldBestEffort re-exports removeConfigFieldBestEffort; the
// root load path passes it as the provider configuration write callback.
func (s *ConfigStore) RemoveConfigFieldBestEffort(scope Scope, key string) {
	s.removeConfigFieldBestEffort(scope, key)
}

// VerifyModelsIntegrityAtLoad re-exports verifyModelsIntegrityAtLoad for the
// root load path.
func (s *ConfigStore) VerifyModelsIntegrityAtLoad() { s.verifyModelsIntegrityAtLoad() }

// PeakHoursEntryForScope returns the peak_hours entry a SINGLE scope's
// config file currently stores for providerID — not the merged
// effective view. Returns (nil, nil) when that scope has no provider
// entry or no peak_hours on it. Read-only: no locks, no reload; a
// missing file is (nil, nil); genuine read/decode errors are surfaced.
// Moved verbatim from the root package's provider_peak_hours_read.go.
func (s *ConfigStore) PeakHoursEntryForScope(scope Scope, providerID string) (*peak.ProviderPeakHours, error) {
	if providerID == "" {
		return nil, fmt.Errorf("peak_hours read: empty provider id")
	}
	path, err := s.configPath(scope)
	if err != nil {
		return nil, err
	}
	// Mirror the write path's target resolution (slots.WriteTargetSlot then
	// resolveConfigWriteTarget) WITHOUT acquiring the inter-process lock:
	// this helper is read-only and tolerates a torn concurrent write
	// exactly like any other unlocked reader.
	selected := slots.WriteTargetSlot(path)
	target, err := s.resolveConfigWriteTarget(selected)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve config file %q: %w", path, err)
	}
	raw, _, err := docread.ReadStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	doc, err := docread.DecodeSettingsDocument(target.selectedPath, raw, target.expected)
	if err != nil {
		return nil, err
	}
	var probe struct {
		Providers map[string]struct {
			PeakHours json.RawMessage `json:"peak_hours"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(doc.JSONBytes, &probe); err != nil {
		return nil, fmt.Errorf("failed to decode config file: %w", err)
	}
	entry, ok := probe.Providers[providerID]
	if !ok || len(bytes.TrimSpace(entry.PeakHours)) == 0 {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(entry.PeakHours), []byte("null")) {
		return nil, nil
	}
	var ph peak.ProviderPeakHours
	if err := json.Unmarshal(entry.PeakHours, &ph); err != nil {
		return nil, err
	}
	return &ph, nil
}

// ConfigWriteTargetFields deconstructs the write target for the root's
// cfgcommit converters (fields are unexported in the leaf).
func (t configWriteTarget) ConfigWriteTargetFields() (path, selectedPath, lockPath string, expected reloadFileFingerprint, owner int, enforce bool) {
	return t.path, t.selectedPath, t.lockPath, t.expected, t.owner, t.enforce
}

// ---------------------------------------------------------------------------
// Parity-test wrappers (s0-w85 duplicate audit, resolution 2): the root keeps
// historical copies of these store helpers; the root parity test
// (internal/config/cfgstore_parity_test.go) runs both implementations over
// the same input table and fails on divergence. Each wrapper forwards to the
// unexported leaf original unchanged.
// ---------------------------------------------------------------------------

func SetDefaultsForTest(c *Config, workingDir, dataDir string) { setDefaults(c, workingDir, dataDir) }

func CandidateInputsChangedForTest(workingDir string, configPaths, externalPaths []string, fingerprints map[string]reloadFileFingerprint) bool {
	return candidateInputsChanged(workingDir, configPaths, externalPaths, fingerprints)
}

func InitialMCPRevisionsForTest(cfg *Config) map[string]uint64 { return initialMCPRevisions(cfg) }

func MCPInputFingerprintsForTest(rush, external []stableConfigDocument) map[string][32]byte {
	return mcpInputFingerprints(rush, external)
}

func MCPRevisionDiffForTest(current map[string]uint64, previous, next *Config, previousInputs, nextInputs map[string][32]byte, force ...string) map[string]uint64 {
	return mcpRevisionDiff(current, previous, next, previousInputs, nextInputs, force...)
}

func MCPNamesForTest(cfg *Config) map[string]MCPConfig { return mcpNames(cfg) }

func LoadConfigCandidateStableForWorkingDirForTest(configPaths []string, workingDir string) (*Config, []string, map[string]reloadFileFingerprint, []stableConfigDocument, error) {
	return loadConfigCandidateStableForWorkingDir(configPaths, workingDir)
}

func MustMarshalConfigForTest(cfg *Config) []byte { return mustMarshalConfig(cfg) }

func ResolverInputFingerprintForTest(values []string) [32]byte {
	return resolverInputFingerprint(values)
}
