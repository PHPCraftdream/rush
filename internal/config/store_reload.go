// Disk reload: rebuilding the full config snapshot from the config files
// (workspace merge, provider resolution, selected models), publishing it as
// a new generation, and the autoReload dedup wrapper used after writes.
package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/env"
)

const reloadMaxAttempts = 4

const stableReadMaxAttempts = 4

var (
	errReloadDiskChanged = errors.New("config files changed while reloading")
	// ErrConfigReloadUnstable indicates that no stable file snapshot could be
	// built within the bounded reload retry budget.
	ErrConfigReloadUnstable = errors.New("config files remained unstable during reload")
)

type reloadFileFingerprint struct {
	exists  bool
	size    int64
	modTime int64
	digest  [sha256.Size]byte
}

var errStableReadUnstable = errors.New("config file remained unstable while reading")

// readStableConfigFile returns bytes that were observed identically by two
// consecutive reads. A fingerprint is made from those exact bytes; callers
// must not fingerprint a path before reading it because that admits a stale
// candidate when the file changes between the two operations.
func readStableConfigFile(path string) ([]byte, reloadFileFingerprint, error) {
	for attempt := 0; attempt < stableReadMaxAttempts; attempt++ {
		info, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return nil, reloadFileFingerprint{}, statErr
			}
			return nil, reloadFileFingerprint{}, statErr
		}
		if info.IsDir() {
			return nil, reloadFileFingerprint{}, nil
		}
		first, err := os.ReadFile(path)
		if err != nil {
			return nil, reloadFileFingerprint{}, err
		}
		second, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) && attempt+1 < stableReadMaxAttempts {
				continue
			}
			return nil, reloadFileFingerprint{}, err
		}
		if !bytes.Equal(first, second) {
			continue
		}
		info, err = os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) && attempt+1 < stableReadMaxAttempts {
				continue
			}
			return nil, reloadFileFingerprint{}, err
		}
		if info.IsDir() {
			return second, reloadFileFingerprint{}, nil
		}
		return second, reloadFileFingerprint{
			exists:  true,
			size:    int64(len(second)),
			modTime: info.ModTime().UnixNano(),
			digest:  sha256.Sum256(second),
		}, nil
	}
	return nil, reloadFileFingerprint{}, errStableReadUnstable
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
	return s.reloadFromDiskUnlocked(ctx)
}

// reloadFromDiskUnlocked builds a full candidate storeSnapshot from local
// variables only — no store field is touched until the very end — then
// publishes it under a short publishMu critical section.
//
// Candidate construction uses reloadMu alone. Publication briefly nests
// publishMu and diskWriteMu while it verifies that the files read for the
// candidate have not changed:
//
//   - reloadMu serialises the candidate-build phase against other
//     concurrent reload attempts (autoReload's TryLock on reloadMu skips a
//     redundant reload the same way it used to skip on publishMu). This
//     is purely about not wasting work building N candidates in parallel
//     when one would do — it is NOT required for correctness, since the
//     publish step below is itself safe against concurrent publishers.
//   - publishMu guards the final verification + swap. diskWriteMu is nested
//     only for that short verification and snapshot capture, never during
//     config parsing or shell subprocess execution.
//
// Base generation vs CAS semantics: the candidate is built starting from
// `prev`, the snapshot published at the time the build started. If some
// other writer (a copy-on-write mutator, e.g. SetSkipPermissionRequests)
// publishes a newer generation while the build is still in flight, this
// reload's candidate is stale only with respect to runtime overrides carried
// FORWARD from prev. Config, resolver, providers, loaded paths, workspace
// path, and staleness fingerprints are freshly built from disk regardless of
// what changed concurrently in memory, so they can never regress. Rather
// than discarding a fully-built candidate (expensive: it already paid the
// shell-resolution cost) or
// silently overwriting the concurrent writer's change, the publish step
// re-reads the CURRENT snapshot under publishMu and rebases just those
// forwarded fields onto it before storing — the writer's change survives,
// and the reload's fresh disk state still wins for everything reload
// itself is authoritative over. This mirrors the reasoning already
// applied to runtime overrides for the same class of "small piece of
// forwarded state, rebase onto latest" problem.
func (s *ConfigStore) reloadFromDiskUnlocked(ctx context.Context) error {
	s.reloadMu.Lock()
	return s.runReloadLocked(ctx)
}

// runReloadLocked retries a candidate if any config file changed during its
// build. The pending flag closes the small hand-off window where a disk
// writer finishes after the candidate was published but before reloadMu is
// released.
func (s *ConfigStore) runReloadLocked(ctx context.Context) error {
	for attempt := 1; attempt <= reloadMaxAttempts; attempt++ {
		err := s.buildAndPublishReload(ctx)
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
func (s *ConfigStore) buildAndPublishReload(ctx context.Context) error {
	configPaths := lookupConfigCandidates(s.workingDir)
	externalPaths := mcpJSONCandidatePaths(s.workingDir)
	cfg, loadedPaths, fingerprints, configDocuments, err := loadConfigCandidateStable(configPaths)
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

	workspacePath := filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName))
	if !pathAlreadyLoaded(loadedPaths, workspacePath) {
		wsData, fingerprint, readErr := readStableConfigFile(workspacePath)
		if readErr == nil {
			fingerprints[normalizeReloadPath(workspacePath)] = fingerprint
			configDocuments = append(configDocuments, stableConfigDocument{
				path: normalizeReloadPath(workspacePath), data: wsData,
				fingerprint: fingerprint, present: fingerprint.exists,
			})
		} else if !os.IsNotExist(readErr) {
			return fmt.Errorf("failed to read workspace config for reload: %w", readErr)
		}
		if readErr != nil {
			fingerprints[normalizeReloadPath(workspacePath)] = reloadFileFingerprint{}
			configDocuments = append(configDocuments, stableConfigDocument{path: normalizeReloadPath(workspacePath)})
		}
		if readErr == nil && len(wsData) > 0 {
			if !json.Valid(wsData) {
				return fmt.Errorf("invalid JSON in config file %s", workspacePath)
			}
			merged, mergeErr := loadFromBytes(append([][]byte{mustMarshalConfig(cfg), wsData}))
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

	// Keep .mcp.json discovery and the literal disabled-override merge
	// consistent with the initial Load path. A reload after the external file
	// appears must not silently drop those servers from the new snapshot.
	externalDocuments, _, err := loadExternalMCPDocumentsStable(externalPaths, fingerprints)
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

	// env/resolver are built fresh from the real process environment on
	// every reload. ctx is now threaded all the way into ResolveValue
	// (via configureProviders → resolver.ResolveValue), so a caller that
	// cancels ctx (e.g. app shutdown) can abort an in-flight shell
	// substitution instead of waiting out the full resolveTimeout.
	baseEnv := env.New()
	resolver := NewShellVariableResolver(baseEnv, WithContext(ctx))
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
	if err := cfg.configureProviders(ctx, s, baseEnv, resolver, providers); err != nil {
		return fmt.Errorf("failed to configure providers during reload: %w", err)
	}

	if !cfg.IsConfigured() {
		slog.Warn("No providers configured after reload")
	} else {
		// persist=false: reloadFromDiskUnlocked runs without publishMu
		// held, so configureSelectedModels must NOT take the Locked
		// (reentrant-only) path here — there is no reentrant lock to
		// avoid re-acquiring. Only Load (which does hold publishMu for
		// its whole body) passes persist=true.
		if err := configureSelectedModels(s, cfg, providers, false); err != nil {
			return fmt.Errorf("failed to configure selected models during reload: %w", err)
		}
		cfg.SetupAgents()
	}

	// Every fallible step has succeeded. Build the candidate snapshot from
	// what this build started with (prev) — see the rebase step below for
	// why this is safe even if a concurrent writer published a newer
	// generation while the above ran unlocked.
	candidate := &storeSnapshot{
		config:         cfg,
		resolver:       resolver,
		knownProviders: providers,
		loadedPaths:    loadedPaths,
		workspacePath:  workspacePath,
		overrides:      prev.overrides,
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.diskWriteMu.Lock()
	defer s.diskWriteMu.Unlock()

	if candidateInputsChanged(s.workingDir, configPaths, externalPaths, fingerprints) {
		return errReloadDiskChanged
	}

	// Rebase: if the currently-published snapshot is no longer `prev`
	// (some copy-on-write mutator published in the meantime), carry its
	// runtime overrides onto the candidate instead of the ones captured
	// before the unlocked build — otherwise this publish would silently
	// revert that concurrent change. cfg/resolver/
	// knownProviders/loadedPaths/workspacePath are NOT rebased: they are
	// reload's own authoritative fresh-from-disk output regardless of
	// what changed concurrently in memory.
	cur := s.loadSnapshot()
	if cur.generation != prev.generation {
		candidate.overrides = cur.overrides
	}
	stalenessPaths := configAndMCPStalenessPaths(lookupConfigCandidates(s.workingDir), s.workingDir)
	stalenessPaths = append(stalenessPaths, workspacePath, s.globalDataPath)
	candidate.trackedConfigPaths, candidate.snapshots = reloadStalenessState(stalenessPaths, fingerprints)

	s.publishLocked(candidate)

	return nil
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
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reloadFileFingerprint{}, nil
		}
		return reloadFileFingerprint{}, err
	}
	if info.IsDir() {
		return reloadFileFingerprint{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return reloadFileFingerprint{}, nil
		}
		return reloadFileFingerprint{}, err
	}
	return reloadFileFingerprint{
		exists:  !info.IsDir(),
		size:    int64(len(data)),
		modTime: info.ModTime().UnixNano(),
		digest:  sha256.Sum256(data),
	}, nil
}

func reloadFingerprintsChanged(before map[string]reloadFileFingerprint) bool {
	for path, expected := range before {
		actual, err := readReloadFingerprint(path)
		if err != nil || actual != expected {
			return true
		}
	}
	return false
}

func candidateInputsChanged(workingDir string, configPaths, externalPaths []string, fingerprints map[string]reloadFileFingerprint) bool {
	return !sameReloadPathSet(configPaths, lookupConfigCandidates(workingDir)) ||
		!sameReloadPathSet(externalPaths, mcpJSONCandidatePaths(workingDir)) ||
		reloadFingerprintsChanged(fingerprints)
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

func normalizeReloadPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return abs
}

func reloadStalenessState(paths []string, fingerprints map[string]reloadFileFingerprint) ([]string, map[string]fileSnapshot) {
	trackedSet := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			trackedSet[normalizeReloadPath(path)] = struct{}{}
		}
	}
	tracked := make([]string, 0, len(trackedSet))
	for path := range trackedSet {
		tracked = append(tracked, path)
	}
	slices.Sort(tracked)

	snapshots := make(map[string]fileSnapshot, len(tracked))
	for _, path := range tracked {
		fingerprint := fingerprints[path]
		snapshots[path] = fileSnapshot{
			Path:        path,
			Exists:      fingerprint.exists,
			Size:        fingerprint.size,
			ModTime:     fingerprint.modTime,
			ContentHash: fingerprint.digest,
		}
	}
	return tracked, snapshots
}
