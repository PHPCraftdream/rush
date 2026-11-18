package config

import (
	"context"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/mcpstore"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthcred"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
	"github.com/PHPCraftdream/rush/internal/config/store/settings"
)

// corePort adapts *ConfigStore to the settings.Port interface. It exposes
// exactly the existing ConfigStore primitives (loadSnapshot, configPath,
// withConfigWriteLock, noteInitialLoadWriteLocked, autoReloadAfterWrite,
// modelsIntegrity) without adding any new locking: every method delegates to
// the same code paths the store's own write/reload API uses today.
type corePort struct {
	store *ConfigStore
}

// Compile-time assertion: corePort implements settings.Port.
var _ settings.Port = corePort{}

// ViewSnap samples the store's published snapshot exactly once per callback
// so every view read inside fn sees a single consistent generation.
func (p corePort) ViewSnap(fn func(settings.SnapView) error) error {
	sn := p.store.loadSnapshot()
	return fn(snapView{sn: sn})
}

// snapView exposes a storeSnapshot as a settings.SnapView.
type snapView struct{ sn *storeSnapshot }

func (v snapView) Config() *cfgmodel.Config { return v.sn.config }
func (v snapView) WorkspacePath() string    { return v.sn.workspacePath }
func (v snapView) Generation() uint64       { return v.sn.generation }

func (p corePort) ScopePath(scope cfgmodel.Scope) (string, error) {
	return p.store.configPath(scope)
}

func (p corePort) GlobalDataPath() string { return p.store.globalDataPath }
func (p corePort) WorkingDir() string     { return p.store.workingDir }

// NoteInitialLoadWrite requires diskWriteMu to be held and may be called
// only from inside a WithFileWriteLock callback. It must not call
// PublishMutation or re-enter WithFileWriteLock.
func (p corePort) NoteInitialLoadWrite(path string, data []byte) {
	p.store.noteInitialLoadWriteLocked(path, data)
}

func (p corePort) AutoReload() error {
	return p.store.autoReloadAfterWrite(context.Background())
}

func (p corePort) WithFileWriteLock(path string, fn func(settings.WriteTarget) error) error {
	return p.store.withConfigWriteLock(path, func(target configWriteTarget) error {
		return fn(settings.WriteTarget{
			Path:         target.path,
			SelectedPath: target.selectedPath,
			Owner:        target.owner,
			Enforce:      target.enforce,
		})
	})
}

func (p corePort) ModelsIntegrity() error       { return p.store.modelsIntegrity }
func (p corePort) SetModelsIntegrity(err error) { p.store.modelsIntegrity = err }

// X1 amendment item 3: the settings-lock store delegates moved here from the
// deleted root settings_lock.go; each forwards to the settings component
// (s.settings).
func (s *ConfigStore) LockState() (LockState, error) { return s.settings.LockState() }

func (s *ConfigStore) SettingsLocked() (bool, error) { return s.settings.SettingsLocked() }

func (s *ConfigStore) CheckProcessPassword() error { return s.settings.CheckProcessPassword() }

func (s *ConfigStore) LockSettings(scope Scope, password string) error {
	return s.settings.LockSettings(scope, password)
}

func (s *ConfigStore) UnlockSettings(scope Scope, password string) error {
	return s.settings.UnlockSettings(scope, password)
}

// settingsStore returns the store's settings facade, constructing an
// ephemeral one over corePort on demand for stores whose constructor did not
// install one (e.g. zero-value test stores).
func (s *ConfigStore) settingsStore() *settings.Store {
	if s.settings != nil {
		return s.settings
	}
	return settings.NewStore(corePort{store: s})
}

// ---------------------------------------------------------------------------
// X2 mcpstore wiring
// ---------------------------------------------------------------------------

// mcpStore returns the store's MCP persistence component, constructing an
// ephemeral one over mcpPort on demand for stores whose constructor did not
// install one (e.g. zero-value test stores).
func (s *ConfigStore) mcpStore() *mcpstore.Store {
	if s.mcp != nil {
		return s.mcp
	}
	return mcpstore.NewStore(mcpPort{store: s})
}

// mcpPort adapts *ConfigStore to the mcpstore.Port interface. It exposes
// exactly the existing ConfigStore primitives (loadSnapshot, publishMu,
// configPath, diskWriteMu, diskWriteGeneration, the workingDir owner cache,
// the systemConfigPathOverride test seam, and the settings write guard)
// without adding any new locking.
type mcpPort struct {
	store *ConfigStore
}

// Compile-time assertion: mcpPort implements mcpstore.Port.
var _ mcpstore.Port = mcpPort{}

// mcpView exposes a storeSnapshot as an mcpstore.MCPView.
type mcpView struct{ sn *storeSnapshot }

func (v mcpView) Config() *cfgmodel.Config                   { return v.sn.config }
func (v mcpView) MCPRevisions() map[string]uint64            { return v.sn.mcpRevisions }
func (v mcpView) MCPInputs() map[string][32]byte             { return v.sn.mcpInputs }
func (v mcpView) Resolver() VariableResolver                 { return v.sn.resolver }
func (v mcpView) ResolverRevision() uint64                   { return v.sn.resolverRevision }
func (v mcpView) Snapshots() map[string]docread.FileSnapshot { return v.sn.snapshots }
func (v mcpView) WorkspacePath() string                      { return v.sn.workspacePath }
func (v mcpView) Generation() uint64                         { return v.sn.generation }

func (p mcpPort) ViewSnap(fn func(mcpstore.MCPView) error) error {
	return fn(mcpView{sn: p.store.loadSnapshot()})
}

// PublishMutation holds publishMu for the whole callback; Publish clones the
// current snapshot, replaces the given fields, and publishes the next
// generation (the same publishLocked CAS-free path the moved methods used).
func (p mcpPort) PublishMutation(fn func(mcpstore.MutationScope) error) error {
	p.store.publishMu.Lock()
	defer p.store.publishMu.Unlock()
	return fn(mcpMutationScope{store: p.store})
}

type mcpMutationScope struct{ store *ConfigStore }

func (m mcpMutationScope) Current() mcpstore.MCPView { return mcpView{sn: m.store.loadSnapshot()} }

func (m mcpMutationScope) Publish(next mcpstore.MCPNext) {
	sn := m.store.loadSnapshot().clone()
	if next.Config != nil {
		sn.config = next.Config
	}
	if next.MCPRevisions != nil {
		sn.mcpRevisions = next.MCPRevisions
	}
	if next.MCPInputs != nil {
		sn.mcpInputs = next.MCPInputs
	}
	if next.Snapshots != nil {
		sn.snapshots = next.Snapshots
	}
	sn.resolverDynamic = next.ResolverDynamic
	m.store.publishLocked(sn)
}

func (p mcpPort) MutateConfig(mutate func(*cfgmodel.Config)) {
	p.store.publishMu.Lock()
	defer p.store.publishMu.Unlock()
	p.store.updateConfigLocked(mutate)
}

func (p mcpPort) ScopePath(scope cfgmodel.Scope) (string, error) { return p.store.configPath(scope) }
func (p mcpPort) GlobalDataPath() string                         { return p.store.globalDataPath }
func (p mcpPort) WorkingDir() string                             { return p.store.workingDir }

func (p mcpPort) SystemConfigPath() string {
	if p.store.systemConfigPathOverride != "" {
		return p.store.systemConfigPathOverride
	}
	return SystemConfig()
}

// WorkingDirOwner computes (once) the working-directory config owner — the
// exact workingDirOwnerOnce/owner/err block that moved out of the old
// mcpOwnerPolicy.
func (p mcpPort) WorkingDirOwner() (owner int, hasWorkingDir bool, err error) {
	s := p.store
	s.workingDirOwnerOnce.Do(func() {
		var enforce bool
		s.workingDirOwner, enforce, s.workingDirOwnerErr = docread.ConfigOwnerForWorkingDir(s.workingDir)
		_ = enforce
	})
	if s.workingDirOwnerErr != nil {
		return 0, false, s.workingDirOwnerErr
	}
	return s.workingDirOwner, s.workingDir != "", nil
}

// DiskWrite holds diskWriteMu for the whole callback.
func (p mcpPort) DiskWrite(fn func(mcpstore.DiskWriter) error) error {
	p.store.diskWriteMu.Lock()
	defer p.store.diskWriteMu.Unlock()
	return fn(mcpDiskWriter{store: p.store})
}

// mcpDiskWriter converts between the root configWriteTarget and the
// mcpstore.LockTarget seam type around the existing resolve/verify helpers,
// preserving the hook timing of the original withMCPLocksUsingCaseSensitivity.
type mcpDiskWriter struct{ store *ConfigStore }

func lockTargetOf(t configWriteTarget) mcpstore.LockTarget {
	return mcpstore.LockTarget{Path: t.path, SelectedPath: t.selectedPath, LockPath: t.lockPath, Expected: t.expected, Owner: t.owner, Enforce: t.enforce}
}

func configWriteTargetOf(l mcpstore.LockTarget) configWriteTarget {
	return configWriteTarget{path: l.Path, selectedPath: l.SelectedPath, lockPath: l.LockPath, expected: l.Expected, owner: l.Owner, enforce: l.Enforce}
}

func (w mcpDiskWriter) ResolveWriteTargets(paths []string) ([]mcpstore.LockTarget, error) {
	targets := make([]mcpstore.LockTarget, 0, len(paths))
	for _, path := range paths {
		target, err := w.store.resolveConfigWriteTarget(path)
		if err != nil {
			return nil, err
		}
		runConfigAfterMCPResolveTargetHook(&target)
		targets = append(targets, lockTargetOf(target))
	}
	return targets, nil
}

func (w mcpDiskWriter) VerifyTargetBindingAfterLock(target mcpstore.LockTarget) error {
	return verifyConfigTargetBindingAfterLock(configWriteTargetOf(target))
}

func (w mcpDiskWriter) VerifyTargetBinding(target mcpstore.LockTarget, actual docread.Fingerprint, readErr error) error {
	return verifyConfigTargetBinding(configWriteTargetOf(target), actual, readErr)
}

func (p mcpPort) CommitDiskWrite() { p.store.diskWriteGeneration++ }

// NoteInitialLoadWrite requires diskWriteMu to be held and may be called
// only from inside a WithFileWriteLock callback. It must not call
// PublishMutation or re-enter DiskWrite.
func (p mcpPort) NoteInitialLoadWrite(path string, data []byte) {
	p.store.noteInitialLoadWriteLocked(path, data)
}

func (p mcpPort) SettingsGuard() *settings.Store { return p.store.settingsStore() }

// ---------------------------------------------------------------------------
// X2 delegators: each forwards to the MCP persistence component (s.mcp).
// ---------------------------------------------------------------------------

func (s *ConfigStore) ResolveMCPWritableScope(name string) (Scope, error) {
	return s.mcpStore().ResolveMCPWritableScope(name)
}

func (s *ConfigStore) PersistMCPConfig(scope Scope, name string, value MCPConfig) error {
	return s.mcpStore().PersistMCPConfig(scope, name, value)
}

func (s *ConfigStore) PersistMCPConfigResultWithAuthority(scope Scope, name string, value MCPConfig, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mcpStore().PersistMCPConfigResultWithAuthority(scope, name, value, authority)
}

func (s *ConfigStore) PersistMCPConfigExact(scope Scope, name string, value MCPConfig) error {
	return s.mcpStore().PersistMCPConfigExact(scope, name, value)
}

func (s *ConfigStore) PersistMCPConfigResult(scope Scope, name string, value MCPConfig) (MCPMutationResult, error) {
	return s.mcpStore().PersistMCPConfigResult(scope, name, value)
}

func (s *ConfigStore) mutateMCPExact(operation string, scope Scope, oldName, newName string, value MCPConfig, disabled *bool) (MCPMutationResult, error) {
	return s.mcpStore().MutateMCPExact(operation, scope, oldName, newName, value, disabled)
}

func (s *ConfigStore) PersistRemoveMCPConfig(scope Scope, name string) error {
	return s.mcpStore().PersistRemoveMCPConfig(scope, name)
}

func (s *ConfigStore) PersistRemoveMCPConfigExact(scope Scope, name string) error {
	return s.mcpStore().PersistRemoveMCPConfigExact(scope, name)
}

func (s *ConfigStore) PersistRemoveMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.mcpStore().PersistRemoveMCPConfigResult(scope, name)
}

func (s *ConfigStore) PersistRemoveMCPConfigResultWithAuthority(scope Scope, name string, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mcpStore().PersistRemoveMCPConfigResultWithAuthority(scope, name, authority)
}

func (s *ConfigStore) PersistRemovePendingMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.mcpStore().PersistRemovePendingMCPConfigResult(scope, name)
}

func (s *ConfigStore) PersistRemovePendingMCPConfigResultWithAuthority(scope Scope, name string, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mcpStore().PersistRemovePendingMCPConfigResultWithAuthority(scope, name, authority)
}

func (s *ConfigStore) PersistRemovePendingMCPConfig(scope Scope, name string) error {
	return s.mcpStore().PersistRemovePendingMCPConfig(scope, name)
}

func (s *ConfigStore) PersistMCPDisabledOverride(scope Scope, name string, disabled bool) error {
	return s.mcpStore().PersistMCPDisabledOverride(scope, name, disabled)
}

func (s *ConfigStore) PersistMCPDisabledOverrideResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.mcpStore().PersistMCPDisabledOverrideResult(scope, name, disabled)
}

func (s *ConfigStore) PersistMCPDisabledOverrideResultWithAuthority(scope Scope, name string, disabled bool, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mcpStore().PersistMCPDisabledOverrideResultWithAuthority(scope, name, disabled, authority)
}

func (s *ConfigStore) PersistMCPEnableRollbackResult(scope Scope, name string, token MCPMutationResult) (MCPMutationResult, error) {
	return s.mcpStore().PersistMCPEnableRollbackResult(scope, name, token)
}

func (s *ConfigStore) PersistMCPEnableRollbackResultWithAuthority(scope Scope, name string, token MCPMutationResult, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mcpStore().PersistMCPEnableRollbackResultWithAuthority(scope, name, token, authority)
}

func (s *ConfigStore) PersistMCPDisabledOverrideExact(scope Scope, name string, disabled bool) error {
	return s.mcpStore().PersistMCPDisabledOverrideExact(scope, name, disabled)
}

func (s *ConfigStore) PersistMCPDisabledOverrideExactResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.mcpStore().PersistMCPDisabledOverrideExactResult(scope, name, disabled)
}

func (s *ConfigStore) PersistMCPDisabledOverrideAtScope(scope Scope, name string, disabled bool) error {
	return s.mcpStore().PersistMCPDisabledOverrideAtScope(scope, name, disabled)
}

func (s *ConfigStore) PersistMCPDisabledOverrideInScope(scope Scope, name string, disabled bool) error {
	return s.mcpStore().PersistMCPDisabledOverrideInScope(scope, name, disabled)
}

func (s *ConfigStore) PersistReplaceMCP(oldName, newName string, value MCPConfig) error {
	return s.mcpStore().PersistReplaceMCP(oldName, newName, value)
}

func (s *ConfigStore) PersistReplaceMCPResult(scope Scope, oldName, newName string, value MCPConfig) (MCPMutationResult, error) {
	return s.mcpStore().PersistReplaceMCPResult(scope, oldName, newName, value)
}

func (s *ConfigStore) PersistReplaceMCPResultWithAuthority(scope Scope, oldName, newName string, value MCPConfig, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mcpStore().PersistReplaceMCPResultWithAuthority(scope, oldName, newName, value, authority)
}

func (s *ConfigStore) PersistReplaceMCPInScope(scope Scope, oldName, newName string, value MCPConfig) error {
	return s.mcpStore().PersistReplaceMCPInScope(scope, oldName, newName, value)
}

func (s *ConfigStore) PersistReplaceMCPAtScope(scope Scope, oldName, newName string, value MCPConfig) error {
	return s.mcpStore().PersistReplaceMCPAtScope(scope, oldName, newName, value)
}

func (s *ConfigStore) PersistMCPFields(scope Scope, name string, fields map[string]any) error {
	return s.mcpStore().PersistMCPFields(scope, name, fields)
}

func (s *ConfigStore) PersistMCPFieldsExact(scope Scope, name string, fields map[string]any) error {
	return s.mcpStore().PersistMCPFieldsExact(scope, name, fields)
}

func (s *ConfigStore) PersistMCPConfigAtScope(scope Scope, name string, value MCPConfig) error {
	return s.mcpStore().PersistMCPConfigAtScope(scope, name, value)
}

func (s *ConfigStore) PersistRemoveMCPConfigAtScope(scope Scope, name string) error {
	return s.mcpStore().PersistRemoveMCPConfigAtScope(scope, name)
}

func (s *ConfigStore) PersistMCPFieldsAtScope(scope Scope, name string, fields map[string]any) error {
	return s.mcpStore().PersistMCPFieldsAtScope(scope, name, fields)
}

func (s *ConfigStore) PersistMCPConfigInScope(scope Scope, name string, value MCPConfig) error {
	return s.mcpStore().PersistMCPConfigInScope(scope, name, value)
}

func (s *ConfigStore) PersistRemoveMCPConfigInScope(scope Scope, name string) error {
	return s.mcpStore().PersistRemoveMCPConfigInScope(scope, name)
}

func (s *ConfigStore) PersistMCPFieldsInScope(scope Scope, name string, fields map[string]any) error {
	return s.mcpStore().PersistMCPFieldsInScope(scope, name, fields)
}

func (s *ConfigStore) WithCurrentMCPMutation(result MCPMutationResult, fn func() error) error {
	return s.mcpStore().WithCurrentMCPMutation(result, fn)
}

func (s *ConfigStore) WithCurrentMCPAdmission(snapshot MCPAdmissionSnapshot, name string, fn func(MCPAdmissionGuard) error) error {
	return s.mcpStore().WithCurrentMCPAdmission(snapshot, name, fn)
}

func (s *ConfigStore) WithCurrentMCPAdmissionContext(ctx context.Context, snapshot MCPAdmissionSnapshot, name string, fn func(MCPAdmissionGuard) error) error {
	return s.mcpStore().WithCurrentMCPAdmissionContext(ctx, snapshot, name, fn)
}

func (s *ConfigStore) MarkMCPUncertain(name string) uint64 {
	return s.mcpStore().MarkMCPUncertain(name)
}

func (s *ConfigStore) MCPUncertaintyVersion(name string) (uint64, bool) {
	return s.mcpStore().MCPUncertaintyVersion(name)
}

func (s *ConfigStore) MCPUncertaintyVersions(names ...string) map[string]uint64 {
	return s.mcpStore().MCPUncertaintyVersions(names...)
}

func (s *ConfigStore) SnapshotMCPAdmission(name string) MCPAdmissionSnapshot {
	return s.mcpStore().SnapshotMCPAdmission(name)
}

func (s *ConfigStore) AddMCP(name string, mcpConfig MCPConfig) bool {
	return s.mcpStore().AddMCP(name, mcpConfig)
}

func (s *ConfigStore) UpdateMCP(name string, mutate func(*MCPConfig)) (MCPConfig, bool) {
	return s.mcpStore().UpdateMCP(name, mutate)
}

func (s *ConfigStore) RemoveMCP(name string) (MCPConfig, bool) {
	return s.mcpStore().RemoveMCP(name)
}

func (s *ConfigStore) RemoveMCPIfCurrent(name string, expected MCPConfig, revision uint64) (MCPConfig, bool) {
	return s.mcpStore().RemoveMCPIfCurrent(name, expected, revision)
}

func (s *ConfigStore) SetMCPDisabled(name string, disabled bool) (MCPConfig, bool) {
	return s.mcpStore().SetMCPDisabled(name, disabled)
}

func (s *ConfigStore) PrepareDockerMCPConfig() (MCPConfig, error) {
	return s.mcpStore().PrepareDockerMCPConfig()
}

func (s *ConfigStore) PersistDockerMCPConfig(mcpConfig MCPConfig) error {
	return s.mcpStore().PersistDockerMCPConfig(mcpConfig)
}

func (s *ConfigStore) EnableDockerMCP() error { return s.mcpStore().EnableDockerMCP() }

func (s *ConfigStore) DisableDockerMCP() error { return s.mcpStore().DisableDockerMCP() }

// Moved from store_mcp_forwarders.go (file deleted: the mcpstore package costs one net file; ratchet recorded 222).
type (
	MCPAdmissionGuard    = mcpstore.MCPAdmissionGuard
	MCPAdmissionSnapshot = mcpstore.MCPAdmissionSnapshot
	MCPMutationResult    = mcpstore.MCPMutationResult

	// White-box test aliases: the staying-root tests keep the original
	// unexported type names for the mcpstore types they construct.
	mcpAdmissionToken      = mcpstore.AdmissionToken
	mcpEvaluation          = mcpstore.Evaluation
	mcpFileRecord          = mcpstore.FileRecord
	mcpAdmissionReadSeeker = mcpstore.AdmissionReadSeeker
	mcpJSONEdit            = mcpstore.JSONEdit
	mcpJSONMember          = mcpstore.JSONMember
	mcpJSONObject          = mcpstore.JSONObject
	mcpLockedFiles         = mcpstore.LockedFiles
)

var (
	ErrMCPNotFound         = mcpstore.ErrMCPNotFound
	ErrMCPExternal         = mcpstore.ErrMCPExternal
	ErrMCPAmbiguous        = mcpstore.ErrMCPAmbiguous
	ErrMCPUnwritableOrigin = mcpstore.ErrMCPUnwritableOrigin
	ErrMCPTargetExists     = mcpstore.ErrMCPTargetExists
	ErrMCPStale            = mcpstore.ErrMCPStale
	ErrMCPCommitUncertain  = mcpstore.ErrMCPCommitUncertain
	ErrMCPMutationStale    = mcpstore.ErrMCPMutationStale

	// ErrMCPHardLinkTopology moved with the locked-file set (mcpstore).
	ErrMCPHardLinkTopology = mcpstore.ErrMCPHardLinkTopology
)

// Store method forwarders (original unexported names, mcpstore bodies).
func (s *ConfigStore) evaluateMCPFiles(files *mcpLockedFiles) (mcpEvaluation, error) {
	return s.mcpStore().EvaluateMCPFiles(files)
}

func (s *ConfigStore) withMCPAdmissionLocks(fn func(*mcpLockedFiles) error) error {
	return s.mcpStore().WithMCPAdmissionLocks(fn)
}

func (s *ConfigStore) withMCPLocksUsingCaseSensitivity(
	ctx context.Context,
	fn func(*mcpLockedFiles) error,
	caseSensitivity func(string) casefold.ConfigCaseSensitivity,
) error {
	return s.mcpStore().WithMCPLocksUsingCaseSensitivity(ctx, fn, caseSensitivity)
}

func (s *ConfigStore) applyCommittedMCPFingerprint(files *mcpLockedFiles, record *mcpFileRecord, committed docread.Fingerprint) error {
	return s.mcpStore().ApplyCommittedMCPFingerprint(files, record, committed)
}

func (s *ConfigStore) prepareMCPFileMutation(files *mcpLockedFiles, path, operation, oldName, newName string, value MCPConfig, disabled *bool) error {
	return s.mcpStore().PrepareMCPFileMutation(files, path, operation, oldName, newName, value, disabled)
}

func (s *ConfigStore) mcpPathData(files *mcpLockedFiles, path string) ([]byte, bool, error) {
	return s.mcpStore().MCPPathData(files, path)
}

func (s *ConfigStore) orderedMCPPaths() []string { return s.mcpStore().OrderedMCPPaths() }

// Package-level forwarders.
func mcpCommitWasReconciled(err error) bool { return mcpstore.MCPCommitWasReconciled(err) }

func editMCPDocument(data []byte, path, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	return mcpstore.EditMCPDocument(data, path, operation, oldName, newName, value, disabled, mutate)
}

func updateMCPFile(files *mcpLockedFiles, path, name string, mutate func(map[string]any) error) error {
	return mcpstore.UpdateMCPFile(files, path, name, mutate)
}

func applyMCPJSONEdits(data []byte, edits ...mcpJSONEdit) []byte {
	return mcpstore.ApplyMCPJSONEdits(data, edits...)
}

func scanMCPJSONObject(data []byte) (mcpJSONObject, error) {
	return mcpstore.ScanMCPJSONObject(data)
}

func scanMCPJSONValue(data []byte, offset int) (int, *mcpJSONObject, error) {
	return mcpstore.ScanMCPJSONValue(data, offset)
}

func lastMCPMember(members []mcpJSONMember, key string) *mcpJSONMember {
	return mcpstore.LastMCPMember(members, key)
}

func mcpContainerName(path string) string { return mcpstore.MCPContainerName(path) }

func entryOrigins(origins map[string]MCPOrigin, path, workspacePath, globalPath, systemPath string, data []byte) {
	mcpstore.EntryOrigins(origins, path, workspacePath, globalPath, systemPath, data)
}

func (s *ConfigStore) configPathOrEmpty(scope Scope) string {
	path, _ := s.configPath(scope)
	return path
}

func configMCPAdmissionLockGroups(targets []configWriteTarget, caseSensitivity func(string) casefold.ConfigCaseSensitivity) ([]*mcpstore.LockGroup, map[string]mcpstore.LockTarget, map[string]string) {
	lockTargets := make([]mcpstore.LockTarget, 0, len(targets))
	for _, target := range targets {
		lockTargets = append(lockTargets, mcpstore.LockTarget{Path: target.path, SelectedPath: target.selectedPath, LockPath: target.lockPath, Expected: target.expected, Owner: target.owner, Enforce: target.enforce})
	}
	return mcpstore.ConfigMCPAdmissionLockGroups(lockTargets, caseSensitivity)
}

const mcpAdmissionVerifyBufferSize = mcpstore.MCPAdmissionVerifyBufferSize

func validateFinalMCPAdmissionFile(ctx context.Context, path string, file *os.File, expected docread.Fingerprint, expectedOwner int, enforceOwner bool) error {
	return mcpstore.ValidateFinalMCPAdmissionFile(ctx, path, file, expected, expectedOwner, enforceOwner)
}

func verifyMCPAdmissionBytes(ctx context.Context, file mcpAdmissionReadSeeker, expectedSize int64, expectedDigest [32]byte) error {
	return mcpstore.VerifyMCPAdmissionBytes(ctx, file, expectedSize, expectedDigest)
}

// Docker MCP package-level functions moved to mcpstore; the root re-exports
// them (no remaining external callers outside docker_mcp_test.go today).
func IsDockerMCPAvailable() bool { return mcpstore.IsDockerMCPAvailable() }

func DockerMCPAvailabilityCached() (bool, bool) { return mcpstore.DockerMCPAvailabilityCached() }

func RefreshDockerMCPAvailability() bool { return mcpstore.RefreshDockerMCPAvailability() }

func DockerMCPConfig() MCPConfig { return mcpstore.DockerMCPConfig() }

// dataFingerprint moved to mcpstore (DataFingerprint); the root write path
// (store_write.go noteInitialLoadWriteLocked) keeps the original name.
func dataFingerprint(path string, data []byte) docread.Fingerprint {
	return mcpstore.DataFingerprint(path, data)
}

func validatePreparedAdmissionFile(ctx context.Context, path string, file *os.File, expected docread.Fingerprint, expectedOwner int, enforceOwner bool) error {
	return mcpstore.ValidatePreparedAdmissionFile(ctx, path, file, expected, expectedOwner, enforceOwner)
}

// ---------------------------------------------------------------------------
// X3 oauthjournal wiring
// ---------------------------------------------------------------------------

// oauthJournal returns the store's OAuth journal recovery component,
// constructing an ephemeral one over journalPort on demand for stores whose
// constructor did not install one (e.g. zero-value test stores).
func (s *ConfigStore) oauthJournal() *oauthjournal.Store {
	if s.oauthjournal == nil {
		return oauthjournal.NewStore(journalPort{store: s})
	}
	return s.oauthjournal
}

// Root-owned *ConfigStore-typed worker test hooks (moved verbatim from the
// deleted root store_oauth_journal_worker.go).
var (
	oauthJournalWorkerDone func(*ConfigStore, string)
	oauthJournalWorkerIdle func(*ConfigStore, string)
)

// The component owns the serial wait seams; seed them with the root
// interruptible sleep so production behavior matches the pre-move defaults.
func init() {
	oauthjournal.OAuthJournalReadWait = waitCodexRefresh
	oauthjournal.OAuthJournalRetryWait = waitCodexRefresh
	oauthjournal.OAuthJournalWorkerWait = waitCodexRefresh
}

// journalPort adapts *ConfigStore to the oauthjournal.Port interface. It
// exposes exactly the existing ConfigStore primitives (loadSnapshot,
// publishMu, configPath, diskWriteMu, the codex refresh constants and write
// locks, applyOAuthToken*, and the root worker test hooks) without adding
// any new locking.
type journalPort struct {
	store *ConfigStore
}

// Compile-time assertion: journalPort implements oauthjournal.Port.
var _ oauthjournal.Port = journalPort{}

// journalWriteTargetOf converts the root configWriteTarget to the
// oauthjournal.WriteTarget seam type (the lockTargetOf pattern).
func journalWriteTargetOf(t configWriteTarget) oauthjournal.WriteTarget {
	return oauthjournal.WriteTarget{Path: t.path, SelectedPath: t.selectedPath, LockPath: t.lockPath, Expected: t.expected, Owner: t.owner, Enforce: t.enforce}
}

func configWriteTargetOfJournal(t oauthjournal.WriteTarget) configWriteTarget {
	return configWriteTarget{path: t.Path, selectedPath: t.SelectedPath, lockPath: t.LockPath, expected: t.Expected, owner: t.Owner, enforce: t.Enforce}
}

// credToJournal/journalToCred convert between the oauthcred and oauthjournal
// WriteTarget seam types (same field shape).
func credToJournal(t oauthcred.WriteTarget) oauthjournal.WriteTarget {
	return oauthjournal.WriteTarget{Path: t.Path, SelectedPath: t.SelectedPath, LockPath: t.LockPath, Expected: t.Expected, Owner: t.Owner, Enforce: t.Enforce}
}

func journalToCred(t oauthjournal.WriteTarget) oauthcred.WriteTarget {
	return oauthcred.WriteTarget{Path: t.Path, SelectedPath: t.SelectedPath, LockPath: t.LockPath, Expected: t.Expected, Owner: t.Owner, Enforce: t.Enforce}
}

func (p journalPort) ViewSnap(fn func(oauthjournal.View) error) error {
	return fn(snapView{sn: p.store.loadSnapshot()})
}

// PublishMutation holds the root publishMu for the whole callback.
func (p journalPort) PublishMutation(fn func(oauthjournal.MutationScope) error) error {
	p.store.publishMu.Lock()
	defer p.store.publishMu.Unlock()
	return fn(oauthMutationScope{store: p.store})
}

type oauthMutationScope struct{ store *ConfigStore }

func (m oauthMutationScope) Current() oauthjournal.View { return snapView{sn: m.store.loadSnapshot()} }

func (m oauthMutationScope) Publish(cfg *cfgmodel.Config) {
	sn := m.store.loadSnapshot().clone()
	sn.config = cfg
	m.store.publishLocked(sn)
}

func (m oauthMutationScope) PendingOAuthDigests(provider string) []string {
	return m.store.oauthCredStore().PendingDigestsLocked(provider)
}

func (m oauthMutationScope) DeletePendingOAuthIfToken(provider string, token *oauth.Token) bool {
	return m.store.oauthCredStore().DeletePendingOAuthIfTokenLocked(provider, token)
}

func (p journalPort) WithPublishMu(fn func()) {
	p.store.publishMu.Lock()
	defer p.store.publishMu.Unlock()
	fn()
}

func (p journalPort) ScopePath(scope cfgmodel.Scope) (string, error) {
	return p.store.configPath(scope)
}

func (p journalPort) ApplyOAuthToken(providerID string, expected, token *oauth.Token) error {
	return p.store.applyOAuthToken(providerID, expected, token)
}

func (p journalPort) ApplyOAuthTokenWithPendingPredecessor(providerID string, expected, token, predecessor *oauth.Token, pending bool, records ...*docread.OAuthJournalRecord) error {
	return p.store.applyOAuthTokenWithPendingPredecessor(providerID, expected, token, predecessor, pending, records...)
}

func (p journalPort) TryLockDiskWrite() bool                { return p.store.diskWriteMu.TryLock() }
func (p journalPort) UnlockDiskWrite()                      { p.store.diskWriteMu.Unlock() }
func (p journalPort) ConfigWriteLockTimeout() time.Duration { return configWriteLockTimeout }
func (p journalPort) CodexRefreshLockWait() time.Duration   { return codexRefreshLockWait }
func (p journalPort) WaitCodexRefresh(ctx context.Context, d time.Duration) error {
	return waitCodexRefresh(ctx, d)
}

func (p journalPort) CodexRefreshBeforePersist() func(context.Context, int) error {
	return codexRefreshBeforePersist
}

func (p journalPort) OAuthJournalWorkerDone(provider string) {
	if oauthJournalWorkerDone != nil {
		oauthJournalWorkerDone(p.store, provider)
	}
}

func (p journalPort) OAuthJournalWorkerIdle(provider string) {
	if oauthJournalWorkerIdle != nil {
		oauthJournalWorkerIdle(p.store, provider)
	}
}

func (p journalPort) OAuthRotatingProviders() []oauthjournal.OAuthDescriptor {
	descriptors := p.store.oauthCredStore().Descriptors()
	out := make([]oauthjournal.OAuthDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		out = append(out, oauthjournal.OAuthDescriptor{ID: descriptor.ID(), Rotating: descriptor.Rotating()})
	}
	return out
}

func (p journalPort) ResolveConfigWriteTarget(path string) (oauthjournal.WriteTarget, error) {
	target, err := p.store.resolveConfigWriteTarget(path)
	if err != nil {
		return oauthjournal.WriteTarget{}, err
	}
	return journalWriteTargetOf(target), nil
}

func (p journalPort) WithCodexConfigWriteLock(ctx context.Context, path string, target oauthjournal.WriteTarget, fn func(oauthjournal.WriteTarget) error) error {
	return p.store.oauthCredStore().WithCodexConfigWriteLock(ctx, path, journalToCred(target), func(t oauthcred.WriteTarget) error {
		return fn(credWriteTargetToJournal(t))
	})
}

func (p journalPort) SetOAuthTokenIfCurrentWithLock(scope cfgmodel.Scope, providerID string, expected, token *oauth.Token, withLock func(string, func(oauthjournal.WriteTarget) error) error) (bool, error) {
	return p.store.oauthCredStore().SetOAuthTokenIfCurrentWithLock(scope, providerID, expected, token, func(_ string, fn func(oauthcred.WriteTarget) error) error {
		return withLock(providerID, func(target oauthjournal.WriteTarget) error {
			return fn(journalToCred(target))
		})
	})
}

func (p journalPort) TokenProviderConfig(providerID string, pc cfgmodel.ProviderConfig, token *oauth.Token) cfgmodel.ProviderConfig {
	return p.store.oauthCredStore().TokenProviderConfig(providerID, pc, token)
}

func (p journalPort) ErrCredentialCAS() error { return p.store.oauthCredStore().ErrCredentialCAS() }

// DeletePendingOAuthIfToken requires the root publishMu to be held.
func (p journalPort) DeletePendingOAuthIfToken(provider string, token *oauth.Token) bool {
	return oauthMutationScope{store: p.store}.DeletePendingOAuthIfToken(provider, token)
}

// ---------------------------------------------------------------------------
// X3 delegators: each forwards to the OAuth journal component (s.oauthJournal).
// ---------------------------------------------------------------------------

func (s *ConfigStore) installOAuthOutbox(path string, scope Scope, provider string, record *docread.OAuthJournalRecord) *oauthOutboxEntry {
	return s.oauthJournal().InstallOAuthOutbox(path, scope, provider, record)
}

func (s *ConfigStore) oauthOutboxCurrent(path, provider string) *oauthOutboxEntry {
	return s.oauthJournal().OutboxCurrent(path, provider)
}

func (s *ConfigStore) clearOAuthOutbox(path, provider string, entry *oauthOutboxEntry) {
	s.oauthJournal().ClearOutbox(path, provider, entry)
}

func (s *ConfigStore) settleOAuthOutbox(ctx context.Context, path, provider string, entry *oauthOutboxEntry) (bool, error) {
	return s.oauthJournal().SettleOutbox(ctx, path, provider, entry)
}

func (s *ConfigStore) flushOAuthOutbox(ctx context.Context, path, provider string) error {
	return s.oauthJournal().FlushOutbox(ctx, path, provider)
}

func (s *ConfigStore) stopOAuthJournalWorkers() { s.oauthJournal().StopWorkers() }

func (s *ConfigStore) startOAuthJournalWorker(scope Scope, provider string) {
	s.oauthJournal().StartWorker(scope, provider)
}

func (s *ConfigStore) recoverCodexOAuthJournal(ctx context.Context, scope Scope, provider string) (*oauth.Token, *docread.OAuthJournalRecord, error) {
	return s.oauthJournal().RecoverCodexJournal(ctx, scope, provider)
}

func (s *ConfigStore) withOAuthJournalRecoveryWriteLock(parent context.Context, path string, fn func() error) error {
	return s.oauthJournal().WithRecoveryWriteLock(parent, path, fn)
}

func (s *ConfigStore) recoverOAuthJournalsAtLoad() { s.oauthJournal().RecoverAtLoad() }

func (s *ConfigStore) runOAuthJournalWorker(ctx context.Context, provider string, worker *oauthJournalWorker) {
	s.oauthJournal().RunWorker(ctx, provider, worker)
}

func (s *ConfigStore) oauthJournalWorkerAttempt(ctx context.Context, scope Scope, provider string, attempt int) (bool, error) {
	return s.oauthJournal().WorkerAttempt(ctx, scope, provider, attempt)
}

// White-box test aliases: the staying-root tests keep the original
// unexported type names for the oauthjournal types they construct.
type (
	oauthOutboxKey       = oauthjournal.Key
	oauthOutboxEntry     = oauthjournal.Entry
	oauthJournalWorker   = oauthjournal.Worker
	oauthJournalDecision = oauthjournal.Decision
	oauthJournalRecord   = docread.OAuthJournalRecord
	oauthJournalLogKey   = oauthjournal.LogKey
)

var (
	errOAuthJournalRead    = oauthjournal.ErrOAuthJournalRead
	errOAuthJournalDisk    = oauthjournal.ErrOAuthJournalDisk
	errOAuthJournalInvalid = oauthjournal.ErrOAuthJournalInvalid

	arbitrateOAuthJournal           = oauthjournal.Arbitrate
	readOAuthJournal                = oauthjournal.ReadJournal
	readOAuthJournalDiskToken       = oauthjournal.ReadJournalDiskToken
	readOAuthJournalWithRetry       = oauthjournal.ReadJournalWithRetry
	writeOAuthJournalOnce           = oauthjournal.WriteJournalOnce
	writeOAuthJournalUntilDurable   = oauthjournal.WriteJournalUntilDurable
	verifyOAuthJournalConfigDurable = oauthjournal.VerifyConfigDurable
	removeOAuthJournal              = oauthjournal.RemoveJournal
	verifyOAuthOutboxJournal        = oauthjournal.VerifyOutboxJournal
	quarantineOAuthJournal          = oauthjournal.Quarantine
	quarantineOwnedOAuthJournal     = oauthjournal.QuarantineOwned
	pruneOAuthJournalQuarantines    = oauthjournal.PruneQuarantines
	oauthJournalPath                = oauthjournal.JournalPath
	oauthRefreshLockPath            = oauthjournal.RefreshLockPath
	oauthJournalLivePredecessor     = oauthjournal.LivePredecessor
	oauthJournalWithBurned          = oauthjournal.WithBurned
	oauthRefreshTokenSHA256         = oauthjournal.RefreshTokenSHA256
	oauthRotationDigest             = oauthjournal.RotationDigest
	decodeOAuthJournalDiskToken     = oauthjournal.DecodeOAuthJournalDiskToken
	logOAuthJournalFailure          = oauthjournal.LogOAuthJournalFailure
	logOAuthJournalFailureAt        = oauthjournal.LogOAuthJournalFailureAt
	oauthJournalLogs                = &oauthjournal.OAuthJournalLogs
)

const (
	oauthJournalError          = oauthjournal.OAuthJournalError
	oauthJournalEqual          = oauthjournal.OAuthJournalEqual
	oauthJournalPredecessor    = oauthjournal.OAuthJournalPredecessor
	oauthJournalDiskLaterOrTie = oauthjournal.OAuthJournalDiskLaterOrTie
	oauthJournalLater          = oauthjournal.OAuthJournalLater
	oauthJournalLogout         = oauthjournal.OAuthJournalLogout
)
