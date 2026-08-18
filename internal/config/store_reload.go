// Disk reload: rebuilding the full config snapshot from the config files
// (workspace merge, provider resolution, selected models), publishing it as
// a new generation, and the autoReload dedup wrapper used after writes.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/charmbracelet/crush/internal/env"
)

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
// SetSkipPermissionRequests — behind it. Only the final generation-check
// and pointer swap take publishMu now, for a duration bounded by memory
// operations, not shell subprocess I/O.
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
// Two locks are involved, never held simultaneously by the same call:
//
//   - reloadMu serialises the candidate-build phase against other
//     concurrent reload attempts (autoReload's TryLock on reloadMu skips a
//     redundant reload the same way it used to skip on publishMu). This
//     is purely about not wasting work building N candidates in parallel
//     when one would do — it is NOT required for correctness, since the
//     publish step below is itself safe against concurrent publishers.
//   - publishMu guards only the generation-check + swap at the end. Its
//     hold time is now bounded by a handful of map/pointer operations,
//     never by config-file I/O or shell subprocess execution.
//
// Base generation vs CAS semantics: the candidate is built starting from
// `prev`, the snapshot published at the time the build started. If some
// other writer (a copy-on-write mutator, e.g. SetSkipPermissionRequests)
// publishes a newer generation while the build is still in flight, this
// reload's candidate is stale only with respect to the fields it carries
// FORWARD from prev unchanged (overrides, trackedConfigPaths, snapshots)
// — cfg/resolver/knownProviders/loadedPaths/workspacePath are always
// freshly built from disk regardless of what changed concurrently in
// memory, so they can never regress. Rather than discarding a fully-built
// candidate (expensive: it already paid the shell-resolution cost) or
// silently overwriting the concurrent writer's change, the publish step
// re-reads the CURRENT snapshot under publishMu and rebases just those
// forwarded fields onto it before storing — the writer's change survives,
// and the reload's fresh disk state still wins for everything reload
// itself is authoritative over. This mirrors the reasoning already
// applied to staleness tracking (CaptureStalenessSnapshot) for the same
// class of "small piece of forwarded state, rebase onto latest" problem.
func (s *ConfigStore) reloadFromDiskUnlocked(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	return s.buildAndPublishReload(ctx)
}

// buildAndPublishReload is reloadFromDiskUnlocked's body, factored out so
// autoReload can call it directly after its own TryLock(reloadMu) without
// double-locking reloadMu (sync.Mutex is not reentrant).
func (s *ConfigStore) buildAndPublishReload(ctx context.Context) error {
	configPaths := lookupConfigs(s.workingDir)
	cfg, loadedPaths, err := loadFromConfigPaths(configPaths)
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
	if wsData, err := os.ReadFile(workspacePath); err == nil && len(wsData) > 0 && !pathAlreadyLoaded(loadedPaths, workspacePath) {
		if !json.Valid(wsData) {
			return fmt.Errorf("invalid JSON in config file %s", workspacePath)
		}
		merged, mergeErr := loadFromBytes(append([][]byte{mustMarshalConfig(cfg)}, wsData))
		if mergeErr == nil {
			dataDir := cfg.Options.DataDirectory
			*cfg = *merged
			cfg.setDefaults(s.workingDir, dataDir)
			loadedPaths = append(loadedPaths, workspacePath)
		}
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
		config:             cfg,
		resolver:           resolver,
		knownProviders:     providers,
		loadedPaths:        loadedPaths,
		trackedConfigPaths: prev.trackedConfigPaths,
		snapshots:          prev.snapshots,
		workspacePath:      workspacePath,
		overrides:          prev.overrides,
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	// Rebase: if the currently-published snapshot is no longer `prev`
	// (some copy-on-write mutator published in the meantime), carry its
	// overrides/staleness-tracking forward onto the candidate instead of
	// the ones captured before the unlocked build — otherwise this
	// publish would silently revert that concurrent change. cfg/resolver/
	// knownProviders/loadedPaths/workspacePath are NOT rebased: they are
	// reload's own authoritative fresh-from-disk output regardless of
	// what changed concurrently in memory.
	cur := s.loadSnapshot()
	if cur.generation != prev.generation {
		candidate.overrides = cur.overrides
		candidate.trackedConfigPaths = cur.trackedConfigPaths
		candidate.snapshots = cur.snapshots
	}

	s.publishLocked(candidate)

	// Caller (this function) already holds publishMu — use the Locked
	// variant to avoid a re-entrant deadlock.
	s.captureStalenessSnapshotLocked(loadedPaths)

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
	defer s.reloadMu.Unlock()
	return s.buildAndPublishReload(ctx)
}
