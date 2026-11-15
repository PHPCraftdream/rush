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
	"hash"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
)

const reloadMaxAttempts = 4

const stableReadMaxAttempts = 4

// configFingerprintPath folds the leaf spelling when the containing directory
// is case-insensitive. Case-only aliases are one filesystem object there and
// must not produce different staleness fingerprints.
func configFingerprintPath(path string) string {
	abs := normalizeDiscoveryPath(path)
	dir, leaf := filepath.Dir(abs), filepath.Base(abs)
	if folded, ok := configPlatformCaseFoldLeaf(dir, leaf); ok {
		return filepath.Join(dir, folded)
	}
	return abs
}

func writeConfigDiscoveryInfo(h hash.Hash, info os.FileInfo) {
	_, _ = io.WriteString(h, info.Mode().String())
	// Directory mtimes change for unrelated files created beside a candidate;
	// their physical identity is the part relevant to parent replacement.
	// Regular-file metadata remains part of the exact file fingerprint below.
	if info.IsDir() {
		_, _ = io.WriteString(h, fmt.Sprintf("|dir|%d|", info.Mode()))
	} else {
		_, _ = io.WriteString(h, fmt.Sprintf("|%d|%d|%d|", info.Size(), info.ModTime().UnixNano(), info.Mode()))
	}
	identity := configFileIdentityOf(info)
	_, _ = io.WriteString(h, fmt.Sprintf("%t|%d|%d", identity.valid, identity.device, identity.inode))
}

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
	configPaths := lookupConfigCandidates(s.workingDir)
	// Mirror the initial-load fail-closed rule: an authoritative slot that
	// holds only a backup must never reload into an unlocked, empty config.
	if err := checkAuthoritativeBackupOnly(s.workingDir); err != nil {
		return err
	}
	externalPaths := mcpJSONCandidatePaths(s.workingDir)
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
	cfg.setDefaults(s.workingDir, dataDir)

	workspaceDiscoveryPath := normalizeDiscoveryPath(workspaceConfigPath(cfg.Options.DataDirectory))
	workspacePath := normalizeReloadPath(workspaceDiscoveryPath)
	if err := checkBackupOnlySlot(filepath.Join(cfg.Options.DataDirectory, appName+".json")); err != nil {
		return err
	}
	if !pathAlreadyLoaded(loadedPaths, workspacePath) && s.workingDir != "" {
		expectedOwner, enforceOwner, ownerErr := configOwnerForWorkingDir(s.workingDir)
		if ownerErr != nil {
			return fmt.Errorf("failed to determine workspace config owner: %w", ownerErr)
		}
		wsData, fingerprint, readErr := readStableConfigFileOwned(workspaceDiscoveryPath, expectedOwner, enforceOwner)
		if readErr == nil {
			var decodeErr error
			wsData, decodeErr = decodeConfigDocument(workspaceDiscoveryPath, wsData)
			if decodeErr != nil {
				return fmt.Errorf("failed to decode workspace config %s: %w", workspacePath, decodeErr)
			}
		}
		if readErr == nil {
			fingerprints[workspaceDiscoveryPath] = fingerprint
			configDocuments = append(configDocuments, stableConfigDocument{
				path: workspaceDiscoveryPath, data: wsData,
				fingerprint: fingerprint, present: fingerprint.exists,
				aliases: []stableConfigAlias{{path: workspaceDiscoveryPath, fingerprint: fingerprint}},
			})
		} else if !os.IsNotExist(readErr) {
			return fmt.Errorf("failed to read workspace config for reload: %w", readErr)
		}
		if readErr != nil {
			fingerprints[workspaceDiscoveryPath] = fingerprint
			configDocuments = append(configDocuments, stableConfigDocument{
				path:    workspaceDiscoveryPath,
				aliases: []stableConfigAlias{{path: workspaceDiscoveryPath, fingerprint: fingerprint}},
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
			cfg.setDefaults(s.workingDir, dataDir)
			loadedPaths = append(loadedPaths, normalizeReloadPath(workspacePath))
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
	externalDocuments, _, err := loadExternalMCPDocumentsStableForWorkingDir(externalPaths, fingerprints, s.workingDir)
	if err != nil {
		return fmt.Errorf("failed to load external MCP config during reload: %w", err)
	}
	mergeExternalMCPServersFromDocuments(cfg, configDocuments, externalDocuments)
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
	stalenessPaths := configAndMCPStalenessPaths(lookupConfigCandidates(s.workingDir), s.workingDir)
	stalenessPaths = append(stalenessPaths, workspacePath, s.globalDataPath)
	candidate.trackedConfigPaths, candidate.snapshots = reloadStalenessState(stalenessPaths, fingerprints)

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
		doc := stableConfigDocumentForPath(docs, path)
		if doc == nil || !doc.present {
			continue
		}
		if modelsIntegrityOf(doc.data, lockExists) != nil {
			mismatch = true
			warnModelsIntegrityFingerprintOnce(path, doc.fingerprint.digest)
		}
	}
	return mismatch
}

// stableConfigDocumentForPath finds the candidate document for path.
func stableConfigDocumentForPath(docs []stableConfigDocument, path string) *stableConfigDocument {
	canonical := normalizeReloadPath(path)
	for i := range docs {
		if normalizeReloadPath(docs[i].path) == canonical || matchesLogicalSlot(docs[i].path, canonical) {
			return &docs[i]
		}
	}
	return nil
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

func captureReloadFingerprints(paths []string) (map[string]reloadFileFingerprint, error) {
	fingerprints := make(map[string]reloadFileFingerprint, len(paths))
	for _, path := range paths {
		if err := addReloadFingerprint(fingerprints, path); err != nil {
			return nil, err
		}
	}
	return fingerprints, nil
}

func addReloadFingerprint(fingerprints map[string]reloadFileFingerprint, path string) error {
	if path == "" {
		return nil
	}
	path = normalizeReloadPath(path)
	if _, exists := fingerprints[path]; exists {
		return nil
	}
	fingerprint, err := readReloadFingerprint(path)
	if err != nil {
		return err
	}
	fingerprints[path] = fingerprint
	return nil
}

func readReloadFingerprint(path string) (reloadFileFingerprint, error) {
	_, fingerprint, err := readStableConfigFile(path)
	if err != nil && !os.IsNotExist(err) {
		return reloadFileFingerprint{}, err
	}
	return fingerprint, nil
}

func reloadFingerprintsChanged(before map[string]reloadFileFingerprint) bool {
	return reloadFingerprintsChangedWithOwner(before, nil)
}

func reloadFingerprintsChangedWithOwner(before map[string]reloadFileFingerprint, owner func(string) (int, bool, error)) bool {
	for path, expected := range before {
		expectedOwner, enforceOwner := 0, false
		var ownerErr error
		if owner != nil {
			expectedOwner, enforceOwner, ownerErr = owner(path)
			if ownerErr != nil {
				return true
			}
		}
		_, actual, err := readStableConfigFileOwned(path, expectedOwner, enforceOwner)
		if err != nil || actual != expected {
			if os.IsNotExist(err) {
				// readStableConfigFileOwned returns the discovery fingerprint along
				// with a not-exist error, so this remains an exact comparison.
				if actual == expected {
					continue
				}
			}
			return true
		}
	}
	return false
}

func sameReloadPathSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	for _, path := range left {
		leftSet[normalizeReloadPath(path)] = struct{}{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, path := range right {
		rightSet[normalizeReloadPath(path)] = struct{}{}
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for path := range rightSet {
		if _, ok := leftSet[path]; !ok {
			return false
		}
	}
	return true
}

// trackedSlotFingerprint is the single disk-fingerprint accessor for
// tracked slot paths: it returns the captured reload fingerprint when one
// exists for the path (selected documents, plus canonical/normalized
// aliases via reloadFingerprintForPath), and falls back to a REAL on-disk
// fingerprint (raw bytes: existence plus identity/digest, no decoding,
// never applied to the config) for tracked paths the selected set does not
// cover — an unselected slot sibling that exists must not look absent.
func trackedSlotFingerprint(path string, fingerprints map[string]reloadFileFingerprint) (reloadFileFingerprint, error) {
	if fingerprint := reloadFingerprintForPath(fingerprints, path); fingerprint != (reloadFileFingerprint{}) {
		return fingerprint, nil
	}
	return readReloadFingerprint(path)
}

func reloadFingerprintForPath(fingerprints map[string]reloadFileFingerprint, path string) reloadFileFingerprint {
	if fingerprint, ok := fingerprints[path]; ok {
		return rebindReloadFingerprintPath(path, fingerprint)
	}
	canonical := normalizeReloadPath(path)
	if fingerprint, ok := fingerprints[canonical]; ok {
		return rebindReloadFingerprintPath(path, fingerprint)
	}
	for candidate, fingerprint := range fingerprints {
		if normalizeReloadPath(candidate) == canonical {
			return rebindReloadFingerprintPath(path, fingerprint)
		}
	}
	return reloadFileFingerprint{}
}

func rebindReloadFingerprintPath(path string, fingerprint reloadFileFingerprint) reloadFileFingerprint {
	fingerprint.discovery = configDiscoveryFingerprint(path)
	fingerprint.parentDiscovery = configDiscoveryFingerprint(filepath.Dir(path))
	fingerprint.parentIdentity = configParentIdentity(path)
	fingerprint.aliasChain = configAliasChainFingerprint(path)
	return fingerprint
}
