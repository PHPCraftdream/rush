// Disk reload: rebuilding the full config snapshot from the config files
// (workspace merge, provider resolution, selected models), publishing it as
// a new generation, and the autoReload dedup wrapper used after writes.
package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
)

const reloadMaxAttempts = 4

// ReloadFromDisk re-runs the config load/merge flow and updates the
// in-memory config atomically.
//
// The heavy candidate-build work (disk I/O, JSON parsing, and — the
// expensive part — configureProviders' shell-substitution ResolveValue
// calls for every provider's API key/headers, each bounded by
// resolveTimeout) deliberately runs WITHOUT holding publishMu; see
// reloadFromDiskUnlocked. A single hung "$(...)" in a config value used to
// hold publishMu for the resolver's full timeout, which serialises every
// reader/writer of the store — including unrelated runtime mutators like
// SetSkipPermissionRequests — behind it. Only the final fingerprint check
// and pointer swap take publishMu now; shell subprocess and candidate-build
// work never run under it.
func (s *ConfigStore) ReloadFromDisk(ctx context.Context) error {
	if s.workingDir == "" {
		return fmt.Errorf("cannot reload: working directory not set")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ktavMigrationBudget)
		defer cancel()
	}
	return s.reloadFromDiskUnlocked(ctx)
}

// reloadFromDiskUnlocked builds a full candidate storeSnapshot from local
// variables only — no store field is touched until the very end — then
// publishes it under a short publishMu critical section.
//
// Candidate construction and final input verification run without publishMu.
// Publication briefly nests publishMu and diskWriteMu to validate the
// snapshot and disk-write generations:
//
//   - reloadMu serialises the candidate-build phase against other
//     concurrent reload attempts (autoReload's TryLock on reloadMu skips a
//     redundant reload the same way it used to skip on publishMu). This
//     is purely about not wasting work building N candidates in parallel
//     when one would do — it is NOT required for correctness, since the
//     publish step below is itself safe against concurrent publishers.
//   - publishMu guards final generation validation + swap. diskWriteMu is
//     nested only for generation validation, never during filesystem I/O.
//
// A candidate is based on the snapshot loaded before its build. If either
// that snapshot or the disk-write generation changes before publication,
// the candidate is rejected and rebuilt from the current state.
func (s *ConfigStore) reloadFromDiskUnlocked(ctx context.Context) error {
	if err := s.reloadMu.LockContext(ctx); err != nil {
		return err
	}
	return s.runReloadLocked(ctx)
}

// runReloadLocked retries a candidate if any config file changed during its
// build. The pending flag closes the small hand-off window where a disk
// writer finishes after the candidate was published but before reloadMu is
// released.
func (s *ConfigStore) runReloadLocked(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ktavMigrationBudget)
		defer cancel()
	}
	if err := ktavMigrateStartupSlots(ctx, s.workingDir); err != nil {
		s.releaseReloadLock()
		return err
	}
	prev := s.loadSnapshot()
	dataDir := ""
	if prev.config != nil && prev.config.Options != nil {
		dataDir = prev.config.Options.DataDirectory
	}
	if dataDir != "" {
		if err := ktavMigrateWorkspaceIfNeeded(ctx, s.workingDir, dataDir); err != nil {
			s.releaseReloadLock()
			return err
		}
	}
	expectedUncertainty := s.MCPUncertaintyVersions()
	for attempt := 1; attempt <= reloadMaxAttempts; attempt++ {
		err := s.buildAndPublishReload(ctx, expectedUncertainty)
		if err != nil && !errors.Is(err, errReloadDiskChanged) && !errors.Is(err, errStableReadUnstable) {
			s.releaseReloadLock()
			return err
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				s.releaseReloadLock()
				return ctxErr
			}
			if attempt == reloadMaxAttempts {
				s.releaseReloadLock()
				return fmt.Errorf("%w after %d attempts: %w", ErrConfigReloadUnstable, attempt, err)
			}
			continue
		}

		s.reloadPendingMu.Lock()
		if s.reloadPending {
			if attempt == reloadMaxAttempts {
				// Leave the pending bit set. A later stable reload must consume it;
				// clearing it here would lose the writer that exhausted our budget.
				s.reloadMu.Unlock()
				s.reloadPendingMu.Unlock()
				return fmt.Errorf("%w after %d attempts: %w", ErrConfigReloadUnstable, attempt, errReloadDiskChanged)
			}
			s.reloadPending = false
			s.reloadPendingWaiter = false
			s.reloadPendingMu.Unlock()
			continue
		}
		// Keep the pending flag and reloadMu handshake atomic. A writer that
		// arrives after this point will observe an unlocked reloadMu and can
		// run its own queued reload.
		s.reloadMu.Unlock()
		s.reloadPendingMu.Unlock()
		return nil
	}
	panic("unreachable")
}

func (s *ConfigStore) releaseReloadLock() {
	s.reloadPendingMu.Lock()
	if s.reloadPendingWaiter {
		s.reloadPending = false
		s.reloadPendingWaiter = false
	}
	s.reloadMu.Unlock()
	s.reloadPendingMu.Unlock()
}

// buildAndPublishReload is reloadFromDiskUnlocked's body, factored out so
// autoReload can call it directly after its own TryLock(reloadMu) without
// double-locking reloadMu (sync.Mutex is not reentrant).
func (s *ConfigStore) buildAndPublishReload(ctx context.Context, expectedUncertainty map[string]uint64) error {
	configPaths := docread.LookupConfigCandidates(s.workingDir)
	// Mirror the initial-load fail-closed rule: an authoritative slot that
	// holds only a backup must never reload into an unlocked, empty config.
	if err := docread.CheckAuthoritativeBackupOnly(s.workingDir); err != nil {
		return err
	}
	externalPaths := docread.McpJSONCandidatePaths(s.workingDir)
	cfg, loadedPaths, fingerprints, configDocuments, err := loadConfigCandidateStableForWorkingDir(configPaths, s.workingDir)
	if err != nil {
		return fmt.Errorf("failed to reload config: %w", err)
	}
	// prev is read WITHOUT publishMu: it is only used to seed defaults
	// (dataDir) and as the starting point for the rebase-on-publish
	// below. Reading it unlocked is safe because storeSnapshot is
	// immutable once published — loadSnapshot() always returns either
	// this value or a strictly newer one, never a torn/partial view.
	prev := s.loadSnapshot()

	var dataDir string
	if prev.config != nil && prev.config.Options != nil {
		dataDir = prev.config.Options.DataDirectory
	}
	setDefaults(cfg, s.workingDir, dataDir)

	workspaceDiscoveryPath := docread.NormalizeDiscoveryPath(docread.WorkspaceConfigPath(cfg.Options.DataDirectory))
	workspacePath := docread.NormalizeReloadPath(workspaceDiscoveryPath)
	if err := docread.CheckBackupOnlySlot(filepath.Join(cfg.Options.DataDirectory, appName+".json")); err != nil {
		return err
	}
	if !docread.PathAlreadyLoaded(loadedPaths, workspacePath) && s.workingDir != "" {
		expectedOwner, enforceOwner, ownerErr := docread.ConfigOwnerForWorkingDir(s.workingDir)
		if ownerErr != nil {
			return fmt.Errorf("failed to determine workspace config owner: %w", ownerErr)
		}
		wsData, fingerprint, readErr := docread.ReadStableConfigFileOwned(workspaceDiscoveryPath, expectedOwner, enforceOwner)
		if readErr == nil {
			var decodeErr error
			wsData, decodeErr = docread.DecodeConfigDocument(workspaceDiscoveryPath, wsData)
			if decodeErr != nil {
				return fmt.Errorf("failed to decode workspace config %s: %w", workspacePath, decodeErr)
			}
		}
		if readErr == nil {
			fingerprints[workspaceDiscoveryPath] = fingerprint
			configDocuments = append(configDocuments, stableConfigDocument{
				Path: workspaceDiscoveryPath, Data: wsData,
				Fingerprint: fingerprint, Present: fingerprint.Exists,
				Aliases: []stableConfigAlias{{Path: workspaceDiscoveryPath, Fingerprint: fingerprint}},
			})
		} else if !os.IsNotExist(readErr) {
			return fmt.Errorf("failed to read workspace config for reload: %w", readErr)
		}
		if readErr != nil {
			fingerprints[workspaceDiscoveryPath] = fingerprint
			configDocuments = append(configDocuments, stableConfigDocument{
				Path:    workspaceDiscoveryPath,
				Aliases: []stableConfigAlias{{Path: workspaceDiscoveryPath, Fingerprint: fingerprint}},
			})
		}
		if readErr == nil && len(wsData) > 0 {
			if !json.Valid(wsData) {
				return fmt.Errorf("invalid JSON in config file %s", workspacePath)
			}
			merged, mergeErr := loadFromBytes([][]byte{mustMarshalConfig(cfg), wsData})
			if mergeErr != nil {
				return fmt.Errorf("failed to merge workspace config %s: %w", workspacePath, mergeErr)
			}
			dataDir := cfg.Options.DataDirectory
			*cfg = *merged
			setDefaults(cfg, s.workingDir, dataDir)
			loadedPaths = append(loadedPaths, docread.NormalizeReloadPath(workspacePath))
		}
	}
	if hook := s.reloadAfterDiskRead; hook != nil {
		hook()
	}
	// Re-verify the models-integrity rule on the candidate's stable bytes
	// (plan §3.8, P1: without this a running process systematically adopts
	// a hand-edited models block within one poll interval). On a mismatch
	// (operator decision D4) the candidate keeps the PREVIOUS role models
	// while the rest of it still publishes. This lives in
	// buildAndPublishReload, so every reload path is covered.
	if reloadModelsIntegrityMismatch(s, configDocuments) {
		cfg.Models = prevModelsSelections(prev)
	}

	// Keep .mcp.json discovery and the literal disabled-override merge
	// consistent with the initial Load path. A reload after the external file
	// appears must not silently drop those servers from the new snapshot.
	externalDocuments, _, err := docread.LoadExternalMCPDocumentsStableForWorkingDir(externalPaths, fingerprints, s.workingDir)
	if err != nil {
		return fmt.Errorf("failed to load external MCP config during reload: %w", err)
	}
	docread.MergeExternalMCPServersFromDocuments(cfg, configDocuments, externalDocuments)
	if hook := s.reloadAfterExternalRead; hook != nil {
		hook()
	}
	// The candidate is only publishable if every input still has the exact
	// bytes that were parsed. This content check, rather than mtime alone,
	// rejects an ABA edit that returns to the original metadata after the
	// candidate parsed an intermediate version.
	if candidateInputsChanged(s.workingDir, configPaths, externalPaths, fingerprints) {
		return errReloadDiskChanged
	}

	if err := cfg.ValidateHooks(); err != nil {
		return fmt.Errorf("invalid hook configuration on reload: %w", err)
	}

	// env/resolvers are built fresh from the real process environment on
	// every reload. The candidate resolver carries ctx so cancellation can
	// abort an in-flight shell substitution, while the resolver published
	// below must remain usable after this reload returns.
	baseEnv := env.New()
	candidateResolverOpts := []ShellResolverOption{WithContext(ctx)}
	if s.reloadResolverExpander != nil {
		candidateResolverOpts = append(candidateResolverOpts, WithExpander(s.reloadResolverExpander))
	}
	candidateResolver := NewShellVariableResolver(baseEnv, candidateResolverOpts...)
	runtimeResolver := NewShellVariableResolver(baseEnv)
	providers, err := Providers(cfg)
	if err != nil {
		return fmt.Errorf("failed to load providers during reload: %w", err)
	}

	// This is the expensive step this refactor is about: configureProviders
	// resolves every provider's API key/headers (and MCP discovery),
	// each ResolveValue call bounded by resolveTimeout. It runs under
	// reloadMu (serialising it against other reload attempts) but NOT
	// publishMu — a hung shell substitution here no longer blocks
	// SetSkipPermissionRequests/SetProviderRuntimeConfig/other readers.
	if err := configureProvidersWithScope(ctx, cfg, baseEnv, candidateResolver, providers, s.removeConfigFieldBestEffort, s.cliModels, s.discoveryProviders); err != nil {
		return fmt.Errorf("failed to configure providers during reload: %w", err)
	}

	// Reject a candidate that would silently drop a live custom provider
	// because its base_url is missing or empty (see
	// store_reload_providers.go): publishing such a snapshot would leave
	// role pins resolving to a provider that no longer exists. On error
	// nothing below runs, so the previous snapshot, models, provider map
	// and generation stay untouched. The initial Load keeps the
	// warn-and-skip behaviour.
	if err := rejectLiveProviderEndpointDrops(prev.config, cfg, providers); err != nil {
		return fmt.Errorf("failed to configure providers during reload: %w", err)
	}

	if !cfg.IsConfigured() {
		slog.Warn("No providers configured after reload")
	} else {
		// ConfigureSelectedModels no longer takes store/persist; this reload runs without publishMu held.
		if err := ConfigureSelectedModels(cfg, providers); err != nil {
			return fmt.Errorf("failed to configure selected models during reload: %w", err)
		}
		cfg.SetupAgents()
	}

	// Every fallible step has succeeded. Build the candidate snapshot from
	// what this build started with (prev) — see the rebase step below for
	// why this is safe even if a concurrent writer published a newer
	// generation while the above ran unlocked.
	candidate := &storeSnapshot{
		config:              cfg,
		resolver:            runtimeResolver,
		resolverFingerprint: resolverInputFingerprint(baseEnv.Env()),
		resolverDynamic:     configHasDynamicMCPResolution(cfg),
		mcpInputs:           mcpInputFingerprints(configDocuments, externalDocuments),
		knownProviders:      providers,
		loadedPaths:         loadedPaths,
		workspacePath:       workspacePath,
		overrides:           prev.overrides,
	}
	stalenessPaths := docread.ConfigAndMCPStalenessPaths(docread.LookupConfigCandidates(s.workingDir), s.workingDir)
	stalenessPaths = append(stalenessPaths, workspacePath, s.globalDataPath)
	candidate.trackedConfigPaths, candidate.snapshots = docread.ReloadStalenessState(stalenessPaths, fingerprints)

	s.diskWriteMu.Lock()
	diskWriteGeneration := s.diskWriteGeneration
	s.diskWriteMu.Unlock()

	if hook := s.reloadBeforeFinalInputCheck; hook != nil {
		hook()
	}
	if candidateInputsChanged(s.workingDir, configPaths, externalPaths, fingerprints) {
		return errReloadDiskChanged
	}
	if hook := s.reloadBeforePublish; hook != nil {
		hook()
	}

	if publishErr := func() error {
		s.publishMu.Lock()
		defer s.publishMu.Unlock()
		s.diskWriteMu.Lock()
		defer s.diskWriteMu.Unlock()

		cur := s.loadSnapshot()
		if s.diskWriteGeneration != diskWriteGeneration || cur.generation != prev.generation {
			return errReloadDiskChanged
		}
		candidate.mcpRevisions = mcpRevisionDiff(cur.mcpRevisions, cur.config, candidate.config, cur.mcpInputs, candidate.mcpInputs)
		// Command substitutions can change without environment changes.
		if candidate.resolverFingerprint != cur.resolverFingerprint || candidate.resolverDynamic || cur.resolverDynamic {
			candidate.resolverRevision = cur.resolverRevision + 1
		} else {
			candidate.resolverRevision = cur.resolverRevision
		}
		s.overlayPendingOAuthLocked(candidate)
		s.publishLocked(candidate)
		return nil
	}(); publishErr != nil {
		return publishErr
	}
	s.clearMCPUncertainty(expectedUncertainty)

	return nil
}

// prevModelsSelections snapshots the previous snapshot's raw role-model
// selections for the integrity-mismatch carry-forward.
func prevModelsSelections(prev *storeSnapshot) map[SelectedModelType]SelectedModel {
	if prev == nil || prev.config == nil {
		return nil
	}
	return maps.Clone(prev.config.Models)
}

// reloadModelsIntegrityMismatch re-verifies the models-integrity rule on
// the candidate's stable bytes for the global-data and workspace paths.
// A warning is logged once per distinct offending fingerprint.
func reloadModelsIntegrityMismatch(s *ConfigStore, docs []stableConfigDocument) bool {
	lockExists := s.anySettingsLockExists()
	mismatch := false
	for _, path := range []string{s.globalDataPath, s.loadSnapshot().workspacePath} {
		if path == "" {
			continue
		}
		canonical := docread.NormalizeReloadPath(path)
		var doc *stableConfigDocument
		for i := range docs {
			if docread.NormalizeReloadPath(docs[i].Path) == canonical || docread.MatchesLogicalSlot(docs[i].Path, canonical) {
				doc = &docs[i]
				break
			}
		}
		if doc == nil || !doc.Present {
			continue
		}
		if modelsIntegrityOf(doc.Data, lockExists) != nil {
			mismatch = true
			warnModelsIntegrityFingerprintOnce(path, doc.Fingerprint.Digest)
		}
	}
	return mismatch
}

var (
	modelsIntegrityWarnMu sync.Mutex
	modelsIntegrityWarned = map[[sha256.Size]byte]struct{}{}
)

// warnModelsIntegrityFingerprintOnce logs the unsigned-models warning at
// most once per distinct file fingerprint.
func warnModelsIntegrityFingerprintOnce(path string, digest [sha256.Size]byte) {
	modelsIntegrityWarnMu.Lock()
	defer modelsIntegrityWarnMu.Unlock()
	if _, done := modelsIntegrityWarned[digest]; done {
		return
	}
	modelsIntegrityWarned[digest] = struct{}{}
	slog.Warn("Model settings were changed outside rush; keeping the previous role models", "path", path)
}

func mcpConfig(cfg *Config, name string) (MCPConfig, bool) {
	if cfg == nil {
		return MCPConfig{}, false
	}
	value, ok := cfg.MCP[name]
	return value, ok
}

func (s *ConfigStore) autoReload(ctx context.Context) error {
	if s.workingDir == "" {
		return nil // Expected skip: working directory not set.
	}
	// Skip if a reload is already in progress. This covers both concurrent
	// auto-reloads after parallel writes and the re-entrant call that
	// configureProviders could in principle trigger mid-build. reloadMu
	// (not publishMu) is the guard now: the candidate-build phase this
	// dedups against no longer holds publishMu at all, so TryLock on
	// publishMu would no longer detect "a reload is already in progress"
	// for most of that phase.
	//
	// Note: a write that completes after the in-progress reload has already
	// read the config file won't be reflected in memory until the next
	// reload. That's acceptable — writes are rare and the next user action
	// or file-watch tick picks it up. Callers needing guaranteed freshness
	// after a write should call ReloadFromDisk explicitly.
	if !s.reloadMu.TryLock() {
		return nil
	}
	return s.runReloadLocked(ctx)
}

// autoReloadAfterWrite queues one reload when a disk writer overlaps an
// existing reload. Unlike autoReload, this path must not allow a write to be
// stranded behind reloadMu after the current candidate has been published.
func (s *ConfigStore) autoReloadAfterWrite(ctx context.Context) error {
	if s.workingDir == "" {
		return nil
	}
	if s.initializing.Load() {
		return nil
	}
	if s.reloadMu.TryLock() {
		return s.runReloadLocked(ctx)
	}

	s.reloadPendingMu.Lock()
	s.reloadPending = true
	s.reloadPendingWaiter = true
	s.reloadPendingMu.Unlock()
	if hook := s.reloadAfterWriteQueued; hook != nil {
		hook()
	}
	// Take ownership of a successor instead of returning while the pending bit
	// is merely a promise. This is the writer handoff: if the active reload
	// fails before it reaches its pending check, this writer still performs the
	// queued reload and receives its error.
	for {
		if s.reloadMu.TryLock() {
			s.reloadPendingMu.Lock()
			s.reloadPending = false
			s.reloadPendingMu.Unlock()
			return s.runReloadLocked(ctx)
		}
		select {
		case <-ctx.Done():
			// The active reload may have released between the failed TryLock
			// above and cancellation. Clear our handoff marker if we can take
			// ownership; otherwise releaseReloadLock will clear it on behalf
			// of this canceled waiter.
			if s.reloadMu.TryLock() {
				s.reloadPendingMu.Lock()
				s.reloadPending = false
				s.reloadPendingWaiter = false
				s.reloadPendingMu.Unlock()
				s.reloadMu.Unlock()
			}
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
