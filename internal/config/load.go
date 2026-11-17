package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/catalog"
	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/slotmigrate"
	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
)

const defaultCatwalkURL = "https://catwalk.charm.land"

var loadAfterProviderConfigTestHook struct {
	sync.RWMutex
	fn func(string)
}

func runLoadAfterProviderConfigTestHook(workingDir string) {
	loadAfterProviderConfigTestHook.RLock()
	hook := loadAfterProviderConfigTestHook.fn
	loadAfterProviderConfigTestHook.RUnlock()
	if hook != nil {
		hook(workingDir)
	}
}

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
// loadOptions carries the optional construction-time inputs of Load.
type loadOptions struct {
	cliModels func() []catalog.LocalCLIModel

	// discoveryProviders restricts network model-catalog discovery (Codex
	// account catalog, stepfun/zai live catalogs, Z.AI effort docs) to the
	// named provider ids. Nil means every provider, exactly as before.
	discoveryProviders map[string]bool
}

// LoadOption customizes Load. Options are applied once, before any disk
// or network work starts.
type LoadOption func(*loadOptions)

// WithCLIModels supplies the locally-available CLI model descriptors used
// to synthesize the built-in local-cli provider during configureProviders.
// Composition roots pass the agent-side detection function here (today
// cliprovider.AvailableCLIModels); passing nil, or not passing the option
// at all, behaves exactly like a machine with no CLI installed. The value
// is stored on the returned ConfigStore and reused by every later reload.
func WithCLIModels(fn func() []catalog.LocalCLIModel) LoadOption {
	return func(o *loadOptions) { o.cliModels = fn }
}

// WithDiscoveryProviders scopes network model-catalog discovery to the
// listed provider ids; a reload of the returned store reuses the same
// scope. Without this option every enabled provider is discovered, as
// before. Calling it with no ids disables network discovery entirely.
func WithDiscoveryProviders(ids ...string) LoadOption {
	return func(o *loadOptions) {
		if o.discoveryProviders == nil {
			o.discoveryProviders = make(map[string]bool, len(ids))
		}
		for _, id := range ids {
			o.discoveryProviders[id] = true
		}
	}
}

func Load(workingDir, dataDir string, debug bool, opts ...LoadOption) (*ConfigStore, error) {
	var options loadOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	var lastErr error
	// ONE shared migration deadline for every slot and every load attempt.
	ktavCtx, ktavCancel := context.WithTimeout(context.Background(), slotmigrate.KtavMigrationBudget())
	defer ktavCancel()
	for attempt := 0; attempt < reloadMaxAttempts; attempt++ {
		store, err := loadOnce(ktavCtx, workingDir, dataDir, debug, options.cliModels, options.discoveryProviders)
		if !errors.Is(err, errReloadDiskChanged) && !errors.Is(err, errStableReadUnstable) {
			if err == nil {
				store.recoverOAuthJournalsAtLoad()
			}
			return store, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%w after %d attempts: %v", docread.ErrConfigReloadUnstable, reloadMaxAttempts, lastErr)
}

func loadOnce(ktavCtx context.Context, workingDir, dataDir string, debug bool, cliModels func() []catalog.LocalCLIModel, discoveryProviders map[string]bool) (*ConfigStore, error) {
	if err := docread.CheckAuthoritativeBackupOnly(workingDir); err != nil {
		return nil, err
	}
	// Migrate initial discovery sources before constructing the candidate
	// snapshot. Gated by ktavAutoMigrate (false in this change).
	if err := slotmigrate.KtavMigrateStartupSlots(ktavCtx, workingDir); err != nil {
		return nil, err
	}
	// Read every deterministic candidate, including absent paths. This closes
	// the create-after-discovery gap for project and workspace configuration.
	groups := docread.LookupConfigCandidateGroups(workingDir)
	configPaths := append(groups.Global, groups.Project...)

	cfg, loadedPaths, fingerprints, configDocuments, err := loadConfigCandidateStableForWorkingDir(configPaths, workingDir)
	if err != nil {
		return nil, fmt.Errorf("failed to load config from paths %v: %w", configPaths, err)
	}

	// Data-directory selection (SD-D #1143, WS-3): the ordered resolution
	// below runs exactly once here, never in setDefaults and never again
	// on reload (reloadFromDiskLocked seeds the previous DataDirectory, so
	// setDefaults keeps it). dataDirFromConfig is only meaningful when the
	// merged config FILES named a directory -- the pre-merge cfg value is
	// exactly that (setDefaults has not run yet).
	configuredDataDir := ""
	if cfg.Options != nil {
		configuredDataDir = cfg.Options.DataDirectory
	}
	dataDirResolved, dataDirSource := resolveDataDirectory(workingDir, dataDir, configuredDataDir)
	setDefaults(cfg, workingDir, dataDirResolved)
	sharedDataDir := dataDirIsSharedOfLinkedWorktree(workingDir, dataDirResolved, dataDirSource)
	datadir.SetWorkspaceHome(!sharedDataDir)

	globalDataPath := docread.NormalizeReloadPath(docread.SelectConfigSlot(GlobalConfigData()))
	workspaceDiscoveryPath := docread.NormalizeDiscoveryPath(docread.WorkspaceConfigPath(cfg.Options.DataDirectory))
	workspacePath := docread.NormalizeReloadPath(workspaceDiscoveryPath)
	if err := docread.CheckBackupOnlySlot(filepath.Join(cfg.Options.DataDirectory, appName+".json")); err != nil {
		return nil, err
	}
	if err := slotmigrate.KtavMigrateWorkspaceIfNeeded(ktavCtx, workingDir, cfg.Options.DataDirectory); err != nil {
		return nil, err
	}
	if docread.FileExists(docread.KtavSiblingOf(filepath.Join(cfg.Options.DataDirectory, appName+".json"))) {
		workspaceDiscoveryPath = docread.NormalizeDiscoveryPath(docread.WorkspaceConfigPath(cfg.Options.DataDirectory))
		workspacePath = docread.NormalizeReloadPath(workspaceDiscoveryPath)
	}
	// KTAV-4b (plan 4.2): convert legacy OAuth recovery journals for the
	// slots the recovery workers are about to use, strictly BEFORE any
	// worker starts (recoverOAuthJournalsAtLoad runs after loadOnce
	// succeeds), under the same shared startup deadline. Gated on
	// ktavAutoMigrate; failures defer with a warning and the JSON journal
	// stays active. Library mode (sdk.ModeLibrary) never reaches Load, so
	// it never runs any of this.
	slotmigrate.KtavMigrateOAuthJournalsAtStartup(ktavCtx, []string{
		GlobalConfigData(),
		filepath.Join(cfg.Options.DataDirectory, appName+".json"),
	}, workingDir)

	if debug {
		cfg.Options.Debug = true
	}

	// Load workspace config last so it has highest priority.
	if !docread.PathAlreadyLoaded(loadedPaths, workspacePath) && workingDir != "" {
		expectedOwner, enforceOwner, ownerErr := docread.ConfigOwnerForWorkingDir(workingDir)
		if ownerErr != nil {
			return nil, fmt.Errorf("failed to determine workspace config owner: %w", ownerErr)
		}
		wsData, fingerprint, readErr := docread.ReadStableConfigFileOwned(workspaceDiscoveryPath, expectedOwner, enforceOwner)
		if readErr == nil {
			var decodeErr error
			wsData, decodeErr = docread.DecodeConfigDocument(workspaceDiscoveryPath, wsData)
			if decodeErr != nil {
				return nil, fmt.Errorf("failed to decode workspace config %s: %w", workspacePath, decodeErr)
			}
		}
		if readErr == nil {
			fingerprints[workspaceDiscoveryPath] = fingerprint
			configDocuments = append(configDocuments, docread.StableConfigDocument{
				Path: workspaceDiscoveryPath, Data: wsData,
				Fingerprint: fingerprint, Present: fingerprint.Exists,
				Aliases: []docread.StableConfigAlias{{Path: workspaceDiscoveryPath, Fingerprint: fingerprint}},
			})
		} else if !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("failed to read workspace config %s: %w", workspacePath, readErr)
		}
		if readErr != nil {
			fingerprints[workspaceDiscoveryPath] = fingerprint
			configDocuments = append(configDocuments, docread.StableConfigDocument{
				Path:    workspaceDiscoveryPath,
				Aliases: []docread.StableConfigAlias{{Path: workspaceDiscoveryPath, Fingerprint: fingerprint}},
			})
		}
		if readErr == nil && len(wsData) > 0 {
			if !json.Valid(wsData) {
				return nil, fmt.Errorf("invalid JSON in config file %s", workspacePath)
			}
			merged, mergeErr := loadFromBytes(append([][]byte{mustMarshalConfig(cfg)}, wsData))
			if mergeErr != nil {
				return nil, fmt.Errorf("failed to merge workspace config %s: %w", workspacePath, mergeErr)
			}
			// Preserve defaults that setDefaults already applied.
			dataDir := cfg.Options.DataDirectory
			*cfg = *merged
			setDefaults(cfg, workingDir, dataDir)
			loadedPaths = append(loadedPaths, docread.NormalizeReloadPath(workspacePath))
		}
	}
	// store is constructed with a placeholder snapshot only because
	// configureProviders and mergeExternalMCPServers need a *ConfigStore to
	// call scope-aware helpers (RemoveConfigField, HasConfigField) that
	// read/write disk paths, not the in-memory config. Its real,
	// fully-prepared snapshot is published in one shot inside InitialLoad's
	// build callback below, once every step that can mutate the config has
	// run to completion.
	store := NewStore(workingDir, globalDataPath, dataDirSource, sharedDataDir, cliModels, discoveryProviders, &InitialSnapshot{
		Config:        cfg,
		MCPRevisions:  initialMCPRevisions(cfg),
		WorkspacePath: workspacePath,
		LoadedPaths:   loadedPaths,
	})

	// Load MCP servers from .mcp.json files (Claude Code format) and merge
	// them into the config. Servers defined in rush.json take precedence;
	// the disabled state for external servers is read from rush's own config.
	externalPaths := docread.McpJSONCandidatePaths(workingDir)
	externalDocuments, _, err := docread.LoadExternalMCPDocumentsStableForWorkingDir(externalPaths, fingerprints, workingDir)
	if err != nil {
		if errors.Is(err, errStableReadUnstable) {
			return nil, err
		}
		return nil, fmt.Errorf("failed to load external MCP config: %w", err)
	}
	docread.MergeExternalMCPServersFromDocuments(cfg, configDocuments, externalDocuments)
	if candidateInputsChanged(workingDir, configPaths, externalPaths, fingerprints) {
		return nil, errReloadDiskChanged
	}

	// Validate hooks after all config merging is complete so workspace
	// hooks also get their matcher regexes compiled.
	if err := cfg.ValidateHooks(); err != nil {
		return nil, fmt.Errorf("invalid hook configuration: %w", err)
	}

	// The no-repo walk limits must key on the working directory Load was
	// given, not the process CWD: an SDK host (sdk.Open +
	// Options.WorkingDir) runs with its process CWD somewhere else
	// entirely, and the historical process-wide
	// `git rev-parse --is-inside-work-tree` probe answered that wrong
	// question once and froze the answer for the whole process lifetime.
	// worktreeRoot targets workingDir via cmd.Dir and memoizes per
	// directory (worktreeRootCache), so this keeps the per-directory
	// caching that motivated the old probe and adds no uncached git
	// spawn per call.
	if worktreeRoot(workingDir) == "" {
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

	if err := store.InitialLoad(fingerprints, func(publish func(*InitialSnapshot) error) error {
		publishSnapshot := func() *InitialSnapshot {
			return &InitialSnapshot{
				Config:              cfg,
				Resolver:            valueResolver,
				MCPRevisions:        initialMCPRevisions(cfg),
				MCPInputs:           mcpInputFingerprints(configDocuments, externalDocuments),
				ResolverFingerprint: resolverInputFingerprint(env.Env()),
				ResolverDynamic:     configHasDynamicMCPResolution(cfg),
				KnownProviders:      knownProviders,
				LoadedPaths:         loadedPaths,
				WorkspacePath:       workspacePath,
				ConfigPaths:         configPaths,
				ExternalPaths:       externalPaths,
			}
		}

		if err := configureProvidersWithScope(context.Background(), cfg, env, valueResolver, knownProviders, store.removeConfigFieldBestEffort, cliModels, discoveryProviders); err != nil {
			return fmt.Errorf("failed to configure providers: %w", err)
		}

		if !cfg.IsConfigured() {
			slog.Warn("No providers configured")
			runLoadAfterProviderConfigTestHook(workingDir)
			if err := publish(publishSnapshot()); err != nil {
				return err
			}
			store.verifyModelsIntegrityAtLoad()
			return nil
		}

		if err := ConfigureSelectedModels(cfg, knownProviders); err != nil {
			return fmt.Errorf("failed to configure selected models: %w", err)
		}
		cfg.SetupAgents()

		// Verify and publish the fully-prepared generation together with the
		// exact-byte staleness snapshot. publish holds diskWriteMu across the
		// final verification and swap.
		runLoadAfterProviderConfigTestHook(workingDir)
		if err := publish(publishSnapshot()); err != nil {
			return err
		}
		store.verifyModelsIntegrityAtLoad()
		return nil
	}); err != nil {
		return nil, err
	}
	return store, nil
}

// ResolveDataDirectory resolves the effective data directory (honoring
// --data-dir / the project's configured data_directory, defaulting to
// <workingDir>/.rush) WITHOUT the network/provider-fetch/persist side
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
	groups := docread.LookupConfigCandidateGroups(workingDir)
	configPaths := append(groups.Global, groups.Project...)

	cfg, _, err := loadFromConfigPaths(configPaths)
	if err != nil {
		return "", fmt.Errorf("failed to load config from paths %v: %w", configPaths, err)
	}

	// Same ordered resolution Load performs (SD-D #1143 WS-3 parity), so
	// rescue commands always land on the data directory Load would pick.
	configuredDataDir := ""
	if cfg.Options != nil {
		configuredDataDir = cfg.Options.DataDirectory
	}
	dataDirResolved, _ := resolveDataDirectory(workingDir, dataDir, configuredDataDir)
	return dataDirResolved, nil
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
// They fired unconditionally on every invocation, including `rush run
// --json` and the `logs`/`sessions`/`mcp` scripting commands,
// polluting stderr that orchestrators capture.
//
// Fork patch (orchestrator UX): callers pass cfg.Options.Debug (which
// folds in both the --debug flag and options.debug from the config
// file), so the verbose path — `rush --debug`, `rush run --debug`,
// OR `"options": {"debug": true}` — still surfaces them while default
// and scripted paths stay quiet. Real provider/config/auth warnings
// elsewhere in Load are unaffected.
func logStartupNotice(debug bool, msg string, args ...any) {
	if !debug {
		return
	}
	slog.Warn(msg, args...)
}

func configureProviders(ctx context.Context, cfg *Config, baseEnv env.Env, resolver VariableResolver, knownProviders []catwalk.Provider, dropField func(Scope, string), cliModels func() []catalog.LocalCLIModel) error {
	return configureProvidersWithScope(ctx, cfg, baseEnv, resolver, knownProviders, dropField, cliModels, nil)
}

// discoveryEnabled reports whether provider id is inside the discovery
// scope. A nil scope means every provider — the behavior is unchanged
// when no scope was configured at Load time.
func discoveryEnabled(scope map[string]bool, id string) bool {
	return scope == nil || scope[id]
}
