package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/env"
)

const defaultCatwalkURL = "https://catwalk.charm.land"

// Load loads the configuration from the default paths and returns a
// ConfigStore that owns both the pure-data Config and all runtime state.
//
// Everything that belongs to one generation (config, workspacePath,
// resolver, knownProviders, loadedPaths) is prepared entirely on local
// variables and only published to the store's snapshot once, at the very
// end, after every step that can mutate the config (setDefaults, workspace
// merge, provider/model configuration, SetupAgents) has already run to
// completion. This mirrors reloadFromDiskLocked's "build fully, then swap
// once" shape so the store never has a window where a reader could observe
// a half-configured config (e.g. providers set but SetupAgents not yet
// run).
func Load(workingDir, dataDir string, debug bool) (*ConfigStore, error) {
	configPaths := lookupConfigs(workingDir)

	cfg, loadedPaths, err := loadFromConfigPaths(configPaths)
	if err != nil {
		return nil, fmt.Errorf("failed to load config from paths %v: %w", configPaths, err)
	}

	cfg.setDefaults(workingDir, dataDir)

	globalDataPath := GlobalConfigData()
	workspacePath := filepath.Join(cfg.Options.DataDirectory, fmt.Sprintf("%s.json", appName))

	if debug {
		cfg.Options.Debug = true
	}

	// store is constructed early (with an empty/placeholder snapshot) only
	// because configureProviders and mergeExternalMCPServers need a
	// *ConfigStore to call scope-aware helpers (RemoveConfigField,
	// HasConfigField) that read/write disk paths, not the in-memory
	// config. Its real, fully-prepared snapshot is published in one shot
	// at the end of this function via publish().
	store := &ConfigStore{
		workingDir:     workingDir,
		globalDataPath: globalDataPath,
	}
	store.snap.Store(&storeSnapshot{
		config:        cfg,
		workspacePath: workspacePath,
		loadedPaths:   loadedPaths,
	})

	// Load workspace config last so it has highest priority.
	if wsData, err := os.ReadFile(workspacePath); err == nil && len(wsData) > 0 && !pathAlreadyLoaded(loadedPaths, workspacePath) {
		if !json.Valid(wsData) {
			return nil, fmt.Errorf("invalid JSON in config file %s", workspacePath)
		}
		merged, mergeErr := loadFromBytes(append([][]byte{mustMarshalConfig(cfg)}, wsData))
		if mergeErr == nil {
			// Preserve defaults that setDefaults already applied.
			dataDir := cfg.Options.DataDirectory
			*cfg = *merged
			cfg.setDefaults(workingDir, dataDir)
			loadedPaths = append(loadedPaths, workspacePath)
		}
	}

	// Load MCP servers from .mcp.json files (Claude Code format) and merge
	// them into the config. Servers defined in crush.json take precedence;
	// the disabled state for external servers is read from crush's own config.
	if external := loadExternalMCPServers(workingDir); len(external) > 0 {
		mergeExternalMCPServers(cfg, store, external)
	}

	// Validate hooks after all config merging is complete so workspace
	// hooks also get their matcher regexes compiled.
	if err := cfg.ValidateHooks(); err != nil {
		return nil, fmt.Errorf("invalid hook configuration: %w", err)
	}

	if !isInsideWorktree() {
		const depth = 2
		const items = 100
		// Fork patch (orchestrator UX): gate on cfg.Options.Debug (which
		// already folds in the --debug flag AND options.debug from the
		// config file) rather than the raw flag, so config-file debug users
		// still see the notice. See logStartupNotice.
		logStartupNotice(cfg.Options.Debug, "No git repository detected in working directory, will limit file walk operations", "depth", depth, "items", items)
		assignIfNil(&cfg.Tools.Ls.MaxDepth, depth)
		assignIfNil(&cfg.Tools.Ls.MaxItems, items)
		assignIfNil(&cfg.Options.TUI.Completions.MaxDepth, depth)
		assignIfNil(&cfg.Options.TUI.Completions.MaxItems, items)
	}

	if isAppleTerminal() {
		// Fork patch (orchestrator UX): see the git-repo notice above.
		logStartupNotice(cfg.Options.Debug, "Detected Apple Terminal, enabling transparent mode")
		assignIfNil(&cfg.Options.TUI.Transparent, true)
	}

	// Load known providers, this loads the config from catwalk
	knownProviders, err := Providers(cfg)
	if err != nil {
		return nil, err
	}

	env := env.New()
	// Configure providers
	valueResolver := NewShellVariableResolver(env)

	// Hold reloadMu AND publishMu during the initial load (same acquisition
	// order as buildAndPublishReload: reloadMu -> publishMu), so that
	// auto-reload triggered by config-modifying operations inside
	// configureProviders (e.g. RemoveConfigField) or configureSelectedModels
	// (persisting a newly-selected default model via
	// updatePreferredModelsLocked -> SetConfigFields, when a role has no
	// model configured yet) is skipped instead of recursing.
	//
	// autoReload's redundant-reload dedup is reloadMu.TryLock(), not
	// publishMu (see reloadMu's field doc) — holding only publishMu here,
	// as this function did before, no longer makes a re-entrant autoReload
	// call a safe no-op: it would find reloadMu free, succeed the TryLock,
	// and hang forever trying to re-acquire publishMu on this same
	// goroutine (sync.Mutex is not reentrant). This was a real,
	// 100%-reproducible deadlock on any Load() call where a model role has
	// no configured selection yet (confirmed via
	// TestModelsBump_NonAtomModel_ReportsCleanly hanging past both a 600s
	// and an isolated 30s test timeout with an identical stuck stack).
	//
	// Acquiring reloadMu here is safe with no other goroutine to race
	// against: the *ConfigStore value doesn't exist (and so cannot be
	// referenced by anything else) until this function constructs and
	// returns it below, so nothing outside this call can possibly be
	// reloading THIS store concurrently while both locks are held.
	store.reloadMu.Lock()
	defer store.reloadMu.Unlock()
	store.publishMu.Lock()
	defer store.publishMu.Unlock()

	publish := func() {
		store.snap.Store(&storeSnapshot{
			config:         cfg,
			resolver:       valueResolver,
			knownProviders: knownProviders,
			loadedPaths:    loadedPaths,
			workspacePath:  workspacePath,
		})
	}

	if err := cfg.configureProviders(context.Background(), store, env, valueResolver, knownProviders); err != nil {
		return nil, fmt.Errorf("failed to configure providers: %w", err)
	}

	if !cfg.IsConfigured() {
		slog.Warn("No providers configured")
		publish()
		return store, nil
	}

	if err := configureSelectedModels(store, cfg, knownProviders, true); err != nil {
		return nil, fmt.Errorf("failed to configure selected models: %w", err)
	}
	cfg.SetupAgents()

	// Publish the fully-prepared generation in one shot, then capture the
	// initial staleness snapshot against it. We already hold publishMu, so
	// call the Locked variant directly to avoid a re-entrant deadlock.
	publish()
	store.captureStalenessSnapshotLocked(loadedPaths)

	return store, nil
}

// ResolveDataDirectory resolves the effective data directory (honoring
// --data-dir / the project's configured data_directory, defaulting to
// <workingDir>/.crush) WITHOUT the network/provider-fetch/persist side
// effects Load performs — safe for rescue commands (sessions kill/reset
// --force) that must keep working even when the network or DB is
// unreachable.
//
// This intentionally stops after the same pure, local, filesystem-only
// steps Load itself performs before its first network call (Providers):
// load the config files, then setDefaults, which fully resolves
// cfg.Options.DataDirectory. Everything after that in Load — Providers
// (network fetch under a 45s timeout), configureProviders,
// configureSelectedModels (which can persist a corrected model selection
// to the operator's real global config) — never runs here.
//
// Does NOT apply Load's workspace-config-merge step (Load's "Load
// workspace config last" block) — but this is not actually a gap:
// Load's own merge snapshots cfg.Options.DataDirectory BEFORE merging in
// the workspace file, then re-runs setDefaults with that pre-merge value
// afterward (see the "Preserve defaults that setDefaults already
// applied" comment above), which unconditionally restores the pre-merge
// DataDirectory. Load itself can never honor a workspace-scoped
// data_directory override either — the two resolutions are equivalent
// for this field, not an approximation.
func ResolveDataDirectory(workingDir, dataDir string) (string, error) {
	configPaths := lookupConfigs(workingDir)

	cfg, _, err := loadFromConfigPaths(configPaths)
	if err != nil {
		return "", fmt.Errorf("failed to load config from paths %v: %w", configPaths, err)
	}

	cfg.setDefaults(workingDir, dataDir)
	return cfg.Options.DataDirectory, nil
}

// mustMarshalConfig marshals the config to JSON bytes, returning empty JSON on
// error.
func mustMarshalConfig(cfg *Config) []byte {
	data, err := json.Marshal(cfg)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func assignIfNil[T any](ptr **T, val T) {
	if *ptr == nil {
		*ptr = &val
	}
}

func isAppleTerminal() bool { return os.Getenv("TERM_PROGRAM") == "Apple_Terminal" }

// logStartupNotice emits a non-actionable startup diagnostic at Warn
// level only when debug mode is enabled. These notices describe an
// environment-derived default the user cannot act on from a scripted
// invocation ("no git repo means limited walk depth", "Apple Terminal
// means transparent mode"); the config adjustment itself still applies.
// They fired unconditionally on every invocation, including `crush run
// --json` and the `logs`/`sessions`/`mcp` scripting commands,
// polluting stderr that orchestrators capture.
//
// Fork patch (orchestrator UX): callers pass cfg.Options.Debug (which
// folds in both the --debug flag and options.debug from the config
// file), so the verbose path — `crush --debug`, `crush run --debug`,
// OR `"options": {"debug": true}` — still surfaces them while default
// and scripted paths stay quiet. Real provider/config/auth warnings
// elsewhere in Load are unaffected.
func logStartupNotice(debug bool, msg string, args ...any) {
	if !debug {
		return
	}
	slog.Warn(msg, args...)
}
