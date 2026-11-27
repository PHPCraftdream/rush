// REVERT-CHECKS (orchestrator runs the mutants); one line per test.
// TestConfigDiskSyncForcedTickCatchesSameStat -> removing the st.tick%configDiskSyncForcedEvery forced check in syncConfigFromDisk (store_config_disk_sync.go).
// TestConfigDiskSyncNoReloadOnMetadataOnlyChange -> making configDiskSyncDigestsMatch compare metadata instead of digest (store_config_disk_sync.go).
// TestConfigDiskSyncNoReloadWhenSettled -> dropping the st.settled short-circuit in syncConfigFromDisk (store_config_disk_sync.go).
// TestConfigDiskSyncIneligibleTrackedFileReloadsOnce -> dropping the st.lastAttemptKey outcome-independent memo in configDiskSyncReload (store_config_disk_sync.go).
// TestConfigDiskSyncMemoClearedWhenForeignPathAdoptsDisk -> removing the st.lastAttemptKey clear in syncConfigFromDisk's settled branch (store_config_disk_sync.go).
// TestConfigDiskSyncInvalidJSONKeepsSnapshotAndWarnsOnce -> ignoring st.failedKey in configDiskSyncReload, so every tick retries and re-warns (store_config_disk_sync.go).
// TestConfigDiskSyncStopJoinsCancelledPoller -> making StopConfigDiskSync skip configSyncWG.Wait (store_config_disk_sync.go).
// TestConfigDiskSyncStopJoinsDuringInFlightReload -> holding configSyncMu across StopConfigDiskSync's WG.Wait (store_config_disk_sync.go:118-126): Stop blocks inside its mutex hold, the released reload's configSyncOnModelsChangedFunc callback blocks on the same lock, the join never completes, and the bounded 2s join times out.
// TestConfigDiskSyncOnModelsChangedOnlyOnModelDiff -> firing OnModelsChanged on every reload instead of a model diff (store_config_disk_sync.go).
// TestConfigDiskSyncOnModelsChangedEffortOnlyFires -> dropping ReasoningEffort, Think, MaxTokens, Temperature, or ProviderOptions from configDiskModelProfile (store_config_disk_sync.go); each has its own single-field step that then does not fire.
// TestConfigDiskSyncStopBeforeStartIsTerminal -> dropping the configSyncStopped terminal-stop state (store_config_disk_sync.go, store.go).
// TestReloadRejectsUnsignedModelsEdit -> removing the reloadModelsIntegrityMismatch block in buildAndPublishReload (store_reload.go).
package cfgstore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/providers/catalogsrc"
	"github.com/stretchr/testify/require"
)

// configSyncIsolate redirects every global config path to fresh temp dirs
// and returns the workspace dir, the project config path and the global
// data config path; call it before any store is constructed.
func configSyncIsolate(t *testing.T) (dir, configPath, globalPath string) {
	t.Helper()
	dir = t.TempDir()
	// Leaf-local copy of the root isolateAllGlobalConfigPaths fixture: the
	// root helper lives in the root package's test files and cannot be
	// reached from here (import cycle).
	configDir := filepath.Join(dir, "global-config")
	dataDir := filepath.Join(dir, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)
	configPath = filepath.Join(dir, "rush.json")
	globalPath = datadir.GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
	return dir, configPath, globalPath
}

// resetProviderState is the leaf-local copy of the root helper.
func resetProviderState() { catalogsrc.ResetProviderCacheForTests() }

// writeSignedModelsFixture writes an offline, deterministic rush.json with
// a validly signed models block.
func writeSignedModelsFixture(t *testing.T, path, modelsJSON string) {
	t.Helper()
	doc := `{"options":{"disable_default_providers":true},"models":` + modelsJSON +
		`,"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}}`
	signed, err := applyModelsIntegrity(doc)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(signed), 0o600))
}

// rewriteWithNewMetadata rewrites identical bytes so size is preserved but
// mtime (and on Unix the inode) changes.
func rewriteWithNewMetadata(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// configSyncRefreshPublishedSnapshot rewrites the published snapshot's
// per-path fingerprints from disk, the way a real reload records them, so
// the poller settles after a successful (stubbed) reload.
func configSyncRefreshPublishedSnapshot(s *ConfigStore) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	next := s.loadSnapshot().clone()
	if next.snapshots == nil {
		next.snapshots = map[string]fileSnapshot{}
	} else {
		next.snapshots = make(map[string]fileSnapshot, len(next.snapshots))
	}
	for _, path := range next.trackedConfigPaths {
		fingerprint, err := docread.ReadReloadFingerprint(path)
		if err != nil {
			continue
		}
		next.snapshots[path] = docread.NewFileSnapshot(path, fingerprint)
	}
	s.publishLocked(next)
}

// configSyncWhiteBoxStoreFingerprints builds a store over dir with the
// given paths as the tracked set and the given fingerprints as the
// published per-path state, keyed the same way reloadStalenessState keys
// its snapshots, and injects a counting reload stub that refreshes the
// published fingerprints like a real reload would. The caller owns cfg.
func configSyncWhiteBoxStoreFingerprints(t *testing.T, dir string, cfg *Config, paths []string, fingerprints map[string]reloadFileFingerprint) (*ConfigStore, *atomic.Int32) {
	t.Helper()
	s := &ConfigStore{workingDir: dir, globalDataPath: paths[len(paths)-1]}
	tracked, snapshots := docread.ReloadStalenessState(paths, fingerprints)
	s.snap.Store(&storeSnapshot{
		config:             cfg,
		trackedConfigPaths: tracked,
		snapshots:          snapshots,
		workspacePath:      "",
		generation:         1,
	})
	var calls atomic.Int32
	s.configSyncMu.Lock()
	s.configSyncOpts = ConfigDiskSyncOptions{
		Reload: func(context.Context) error {
			calls.Add(1)
			configSyncRefreshPublishedSnapshot(s)
			return nil
		},
	}
	s.configSyncMu.Unlock()
	return s, &calls
}

// configSyncWhiteBoxStore builds a store over dir with the given paths as
// the tracked set and fingerprints read from disk; see
// configSyncWhiteBoxStoreFingerprints.
func configSyncWhiteBoxStore(t *testing.T, dir string, cfg *Config, paths []string) (*ConfigStore, *atomic.Int32) {
	t.Helper()
	fingerprints := make(map[string]reloadFileFingerprint, len(paths))
	for _, path := range paths {
		fingerprint, err := docread.ReadReloadFingerprint(path)
		require.NoError(t, err)
		// Key the same way reloadStalenessState keys its snapshots.
		fingerprints[docread.NormalizeDiscoveryPath(path)] = fingerprint
	}
	return configSyncWhiteBoxStoreFingerprints(t, dir, cfg, paths, fingerprints)
}

// TestConfigDiskSyncForcedTickCatchesSameStat pins the forced exact check:
// a content change that keeps size and mtime (same bytes length, frozen
// stat) is invisible to the stat gate and must be caught by the every-12th-
// tick full check, not before.
func TestConfigDiskSyncForcedTickCatchesSameStat(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	cfg := &Config{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}}}
	s, calls := configSyncWhiteBoxStore(t, dir, cfg, []string{configPath, globalPath})

	// Tick 1 settles on the unchanged, unedited files.
	var st configDiskSyncState
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded)
	require.True(t, st.settled)

	// Freeze stat at the pre-edit metadata: same size, same mtime.
	before, err := os.Stat(configPath)
	require.NoError(t, err)
	oldStat := configDiskSyncStat
	configDiskSyncStat = func(path string) (os.FileInfo, error) {
		if path == configPath {
			return before, nil
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { configDiskSyncStat = oldStat })

	// Same-length content change: swap "gpt-4" for "gpt-5" in the raw
	// bytes without touching cache_rev (a digest change is all we need).
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	edited := bytes.Replace(data, []byte("gpt-4"), []byte("gpt-5"), 1)
	require.Len(t, edited, len(data))
	require.NoError(t, os.WriteFile(configPath, edited, 0o600))

	// Ticks 2..11: the frozen stat gate hides the change.
	for range 10 {
		result, err = s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, err)
		require.False(t, result.Reloaded)
	}
	require.EqualValues(t, 11, st.tick)

	// Tick 12 is forced and catches the same-stat rewrite.
	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded, "forced tick 12 must catch a same-size same-mtime rewrite")
	require.EqualValues(t, 1, calls.Load())
}

// TestConfigDiskSyncNoReloadOnMetadataOnlyChange pins that a metadata-only
// change (fresh mtime, identical bytes) does not reload and does not churn
// the generation, which would flush every model-pair cache.
func TestConfigDiskSyncNoReloadOnMetadataOnlyChange(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	cfg := &Config{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}}}
	s, calls := configSyncWhiteBoxStore(t, dir, cfg, []string{configPath, globalPath})

	var st configDiskSyncState
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded)
	require.True(t, st.settled)
	gen := s.Generation()

	// Identical bytes, new metadata.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	rewriteWithNewMetadata(t, configPath, data)

	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "identical bytes must not trigger a reload")
	require.Zero(t, calls.Load())
	require.Equal(t, gen, s.Generation())
}

// TestConfigDiskSyncNoReloadWhenSettled pins the level-triggered dedup:
// once the published snapshot matches disk, later ticks make zero reload
// calls (an in-process write publishes fresh fingerprints for free).
func TestConfigDiskSyncNoReloadWhenSettled(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	cfg := &Config{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}}}
	s, calls := configSyncWhiteBoxStore(t, dir, cfg, []string{configPath, globalPath})

	var st configDiskSyncState
	_, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, st.settled)

	for range 3 {
		result, err := s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, err)
		require.False(t, result.Reloaded)
	}
	require.Zero(t, calls.Load(), "settled ticks must not reload")

	// A real content change reloads exactly once and settles again. The
	// model id differs in length from gpt-4: same-length rewrites rely on
	// mtime changing, while a size change is deterministic on every
	// filesystem.
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-5x"}}`)
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded)
	require.EqualValues(t, 1, calls.Load())
	_, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load(), "no duplicate reload after the write was adopted")
}

// TestConfigDiskSyncInvalidJSONKeepsSnapshotAndWarnsOnce pins failure
// handling: an invalid-JSON workspace file keeps the old snapshot, makes
// exactly one reload attempt and logs one warning per distinct digest set
// across retries, and recovers once the file is fixed.
func TestConfigDiskSyncIneligibleTrackedFileReloadsOnce(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	// A directory at the global data path is present but ineligible:
	// eligibleConfigCandidate rejects directories, so Load tracks it with
	// the zero fingerprint while the poller still reads it on every tick.
	require.NoError(t, os.Mkdir(globalPath, 0o755))
	cfg := &Config{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}}}
	configFingerprint, err := docread.ReadReloadFingerprint(configPath)
	require.NoError(t, err)
	s, calls := configSyncWhiteBoxStoreFingerprints(t, dir, cfg,
		[]string{configPath, globalPath},
		map[string]reloadFileFingerprint{
			docread.NormalizeDiscoveryPath(configPath): configFingerprint,
			docread.NormalizeDiscoveryPath(globalPath): {},
		})

	// Tick 1 attempts the reload once.
	var st configDiskSyncState
	result, tickErr := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, tickErr)
	require.True(t, result.Reloaded)
	require.EqualValues(t, 1, calls.Load())
	require.EqualValues(t, 1, st.reloadAttempts)

	// Ticks 2..5: the digests can never match the published snapshot, so
	// each tick would reload without the memo; it must stay quiescent.
	for range 4 {
		result, tickErr = s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, tickErr, "the memo skip must not fabricate a stale error")
		require.False(t, result.Reloaded)
		require.EqualValues(t, 1, calls.Load(), "an unchanged candidate set must not repeat the attempt")
		require.EqualValues(t, 1, st.reloadAttempts)
	}

	// Replacing the directory with a real eligible file changes the
	// candidate digest set, so exactly one more reload succeeds.
	require.NoError(t, os.Remove(globalPath))
	writeSignedModelsFixture(t, globalPath, `{"smart":{"provider":"openai","model":"gpt-global"}}`)
	result, tickErr = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, tickErr)
	require.True(t, result.Reloaded)
	require.EqualValues(t, 2, calls.Load())
	require.EqualValues(t, 2, st.reloadAttempts)
}

// TestConfigDiskSyncMemoClearedWhenForeignPathAdoptsDisk is P2-2: the
// outcome-independent attempt memo must not outlive a foreign publication.
// A→B→A: the poller attempts the ineligible-dir state (memo=dir-key);
// another reload path adopts a real file written behind its back (the
// settled tick clears the memo); the dir comes back — the flip back to the
// attempted content MUST reload again.
// Revert-check: removing the st.lastAttemptKey clear in the settled branch
// leaves the memo at the dir-key, so the final tick skips and the attempt
// count stays 1 instead of 2.
func TestConfigDiskSyncMemoClearedWhenForeignPathAdoptsDisk(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	require.NoError(t, os.Mkdir(globalPath, 0o755))
	cfg := &Config{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}}}
	configFingerprint, err := docread.ReadReloadFingerprint(configPath)
	require.NoError(t, err)
	s, calls := configSyncWhiteBoxStoreFingerprints(t, dir, cfg,
		[]string{configPath, globalPath},
		map[string]reloadFileFingerprint{
			docread.NormalizeDiscoveryPath(configPath): configFingerprint,
			docread.NormalizeDiscoveryPath(globalPath): {},
		})

	// A: the ineligible directory is attempted once and memoized.
	var st configDiskSyncState
	result, tickErr := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, tickErr)
	require.True(t, result.Reloaded)
	require.EqualValues(t, 1, st.reloadAttempts)

	// B: a real file appears and ANOTHER reload path adopts it — the
	// published fingerprints now match disk, so the tick settles AND
	// clears the memo.
	require.NoError(t, os.Remove(globalPath))
	writeSignedModelsFixture(t, globalPath, `{"smart":{"provider":"openai","model":"gpt-foreign"}}`)
	configSyncRefreshPublishedSnapshot(s)
	result, tickErr = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, tickErr)
	require.False(t, result.Reloaded, "the foreign adoption leaves nothing to reload")
	require.True(t, st.settled, "the agreement tick must settle")
	require.EqualValues(t, 1, st.reloadAttempts, "the settled tick must not attempt")

	// A again: the directory returns — the flip back to the attempted
	// content must reload, which only happens when the memo was cleared.
	require.NoError(t, os.Remove(globalPath))
	require.NoError(t, os.Mkdir(globalPath, 0o755))
	result, tickErr = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, tickErr)
	require.True(t, result.Reloaded, "the flip back to the attempted content must reload")
	require.EqualValues(t, 2, st.reloadAttempts)
	require.EqualValues(t, 2, calls.Load())
}

// TestConfigDiskSyncLifecycle pins the lifecycle contract: Start twice
// spawns one goroutine, Stop joins it, nothing fires after Stop, Start
// after Stop is refused, and a store without a working directory never
// starts anything.
// TestConfigDiskSyncStopJoinsDuringInFlightReload pins the Stop-during-
// reload join. Catches: StopConfigDiskSync deadlocking on configSyncMu
// held across WG.Wait while a successful in-flight reload takes the same
// lock in configSyncOnModelsChangedFunc.
func TestConfigDiskSyncOnModelsChangedOnlyOnModelDiff(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	cfg := &Config{
		Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}},
	}
	s, _ := configSyncWhiteBoxStore(t, dir, cfg, []string{configPath, globalPath})

	var fired atomic.Int32
	s.configSyncMu.Lock()
	// The counting stub only refreshes fingerprints; also adopt the disk
	// models into the published config so the diff sees the change.
	base := s.configSyncOpts.Reload
	s.configSyncOpts.Reload = func(ctx context.Context) error {
		if err := base(ctx); err != nil {
			return err
		}
		s.updateConfig(func(cfgCopy *Config) {
			data, err := os.ReadFile(configPath)
			if err != nil {
				return
			}
			var doc struct {
				Models map[SelectedModelType]SelectedModel `json:"models"`
			}
			if json.Unmarshal(data, &doc) == nil && doc.Models != nil {
				cfgCopy.Models = doc.Models
			}
		})
		return nil
	}
	s.configSyncOpts.OnModelsChanged = func(ModelsChange) { fired.Add(1) }
	s.configSyncMu.Unlock()

	var st configDiskSyncState
	// Settle.
	_, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)

	// A recent-models write (unrelated content) reloads but must not fire.
	require.NoError(t, os.WriteFile(configPath, []byte(
		`{"options":{"disable_default_providers":true},"models":{"smart":{"provider":"openai","model":"gpt-4"}},`+
			`"recent_models":{"smart":[{"provider":"openai","model":"gpt-4"}]},`+
			`"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}}`), 0o600))
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded)
	require.False(t, result.ModelsChanged)
	require.Zero(t, fired.Load())

	// A models.smart write fires both.
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-5"}}`)
	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded)
	require.True(t, result.ModelsChanged)
	require.EqualValues(t, 1, fired.Load())
}

// TestReloadRejectsUnsignedModelsEdit pins the settings-lock bypass fix: a
// hand-edited models block with a stale cache_rev keeps the PREVIOUS role
// models on reload while the rest of the candidate (options) is adopted.
func TestConfigDiskSyncOnModelsChangedEffortOnlyFires(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	cfg := &Config{
		Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}},
	}
	s, _ := configSyncWhiteBoxStore(t, dir, cfg, []string{configPath, globalPath})

	var fired atomic.Int32
	s.configSyncMu.Lock()
	// The counting stub only refreshes fingerprints; also adopt the disk
	// models into the published config so the diff sees the change (same
	// shape as TestConfigDiskSyncOnModelsChangedOnlyOnModelDiff).
	base := s.configSyncOpts.Reload
	s.configSyncOpts.Reload = func(ctx context.Context) error {
		if err := base(ctx); err != nil {
			return err
		}
		s.updateConfig(func(cfgCopy *Config) {
			data, err := os.ReadFile(configPath)
			if err != nil {
				return
			}
			var doc struct {
				Models map[SelectedModelType]SelectedModel `json:"models"`
			}
			if json.Unmarshal(data, &doc) == nil && doc.Models != nil {
				cfgCopy.Models = doc.Models
			}
		})
		return nil
	}
	s.configSyncOpts.OnModelsChanged = func(ModelsChange) { fired.Add(1) }
	s.configSyncMu.Unlock()

	var st configDiskSyncState
	// Settle.
	_, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)

	// fire asserts the tick reloaded, reports ModelsChanged, and fired the
	// hook exactly once more.
	fire := func(n int32, what string) {
		t.Helper()
		result, err := s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, err)
		require.True(t, result.Reloaded, "%s: different bytes must reload", what)
		require.True(t, result.ModelsChanged, "%s: a single-field selection change must diff", what)
		require.EqualValues(t, n, fired.Load(), "%s: must fire exactly once", what)
	}

	// Effort-only change, same provider/model id.
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high"}}`)
	fire(1, "effort-only")

	// MaxTokens-only change on top (catches dropping MaxTokens).
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high","max_tokens":4096}}`)
	fire(2, "max_tokens-only")

	// Think-only change on top (catches dropping Think).
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high","max_tokens":4096,"think":true}}`)
	fire(3, "think-only")

	// Temperature-only change on top (P2-4; catches dropping Temperature).
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high","max_tokens":4096,"think":true,"temperature":0.7}}`)
	fire(4, "temperature-only")

	// ProviderOptions-only change on top (P2-4; catches dropping
	// ProviderOptions from the profile).
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high","max_tokens":4096,"think":true,"temperature":0.7,"provider_options":{"wire":"beta"}}}`)
	fire(5, "provider_options-only")

	// TopP-only change on top (catches dropping TopP from the profile).
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high","max_tokens":4096,"think":true,"temperature":0.7,"provider_options":{"wire":"beta"},"top_p":0.9}}`)
	fire(6, "top_p-only")

	// TopK-only change on top (catches dropping TopK from the profile).
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high","max_tokens":4096,"think":true,"temperature":0.7,"provider_options":{"wire":"beta"},"top_p":0.9,"top_k":40}}`)
	fire(7, "top_k-only")

	// FrequencyPenalty-only change on top (catches dropping
	// FrequencyPenalty from the profile).
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high","max_tokens":4096,"think":true,"temperature":0.7,"provider_options":{"wire":"beta"},"top_p":0.9,"top_k":40,"frequency_penalty":0.1}}`)
	fire(8, "frequency_penalty-only")

	// PresencePenalty-only change on top (catches dropping
	// PresencePenalty from the profile).
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"provider":"openai","model":"gpt-4","reasoning_effort":"high","max_tokens":4096,"think":true,"temperature":0.7,"provider_options":{"wire":"beta"},"top_p":0.9,"top_k":40,"frequency_penalty":0.1,"presence_penalty":0.2}}`)
	fire(9, "presence_penalty-only")

	// Bytes change with NO effective change: every profile field —
	// Provider/Model/ReasoningEffort/Think/MaxTokens/Temperature/TopP/TopK/
	// FrequencyPenalty/PresencePenalty, the ProviderOptions map, and the
	// provider identity — stays IDENTICAL (temperature 0.7, provider options
	// {"wire":"beta"}, top_p 0.9, top_k 40, frequency_penalty 0.1 and
	// presence_penalty 0.2 all kept, not dropped); only the raw bytes differ
	// (the smart object's keys are reordered, still written through
	// writeSignedModelsFixture so the models block keeps a valid
	// signature). Reloads, must not fire.
	writeSignedModelsFixture(t, configPath,
		`{"smart":{"presence_penalty":0.2,"frequency_penalty":0.1,"top_k":40,"top_p":0.9,"provider_options":{"wire":"beta"},"temperature":0.7,"think":true,"max_tokens":4096,"reasoning_effort":"high","model":"gpt-4","provider":"openai"}}`)
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded, "different bytes must reload")
	require.False(t, result.ModelsChanged)
	require.EqualValues(t, 9, fired.Load(), "no effective change must not fire")
}

// ---------------------------------------------------------------------------
// Moved from internal/config/store_config_disk_sync_boundary_test.go (P2-b):
// these tests exercise only leaf-owned logic (leaf seam vars and helpers).
// Assertions unchanged; leaf-internal seam spellings are direct vars.

// TestConfigDiskSyncJitterBounds pins the delay contract: the first delay
// is uniform in [0, interval) and every later delay is in
// [0.8, 1.2] * interval. (Moved here from store_config_disk_sync_test.go in
// round 6 for the file-length limit; no assertion changed except the
// P2-8 default-formula loop added below.)
func TestConfigDiskSyncJitterBounds(t *testing.T) {
	require.Equal(t, 5*time.Second, configDiskSyncInterval)
	for range 1000 {
		delay := configDiskSyncDelay()
		require.GreaterOrEqual(t, delay, 4*time.Second)
		require.LessOrEqual(t, delay, 6*time.Second)
	}
	// P2-8: the DEFAULT first-delay formula's own bounds. A per-draw
	// exact-boundary assertion is impossible on a random draw, so the
	// coverage is per-draw bounds here, exact boundary-draw responses of the
	// default formula in TestConfigDiskSyncFirstDelayFormulaBoundaries, and
	// the injectable seam's passthrough boundary draws in
	// TestConfigDiskSyncFirstDelayBoundaryDraws.
	for range 1000 {
		delay := defaultConfigDiskSyncFirstDelay()
		require.GreaterOrEqual(t, delay, time.Duration(0),
			"the default first-delay draw must never be negative")
		require.Less(t, delay, configDiskSyncInterval,
			"the default first-delay draw must be uniform in [0, interval)")
	}

	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	cfg := &Config{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}}}
	s, _ := configSyncWhiteBoxStore(t, dir, cfg, []string{configPath, globalPath})
	t.Cleanup(s.StopConfigDiskSync)

	first := make(chan time.Duration, 1)
	oldWait := configDiskSyncWait
	configDiskSyncWait = func(ctx context.Context, delay time.Duration) error {
		select {
		case first <- delay:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { configDiskSyncWait = oldWait })
	s.StartConfigDiskSync(ConfigDiskSyncOptions{})
	select {
	case delay := <-first:
		require.GreaterOrEqual(t, delay, time.Duration(0))
		require.Less(t, delay, configDiskSyncInterval, "first delay must be uniform in [0, interval)")
	case <-time.After(2 * time.Second):
		t.Fatal("poller never ticked")
	}
}

// TestConfigDiskSyncFirstDelayFormulaBoundaries pins the REAL default
// first-delay formula's exact response to boundary draws, via the draw-source
// seam BELOW the formula (configDiskSyncRandInt63): unlike
// TestConfigDiskSyncFirstDelayBoundaryDraws (which replaces the whole seam
// function ABOVE the formula and only proves passthrough) this exercises the
// formula body itself. rand.Int64N's contract: returns values in [0, n), so
// the extreme draws are 0 and interval-1ns (as a Duration), never interval.
// Catches: replacing defaultConfigDiskSyncFirstDelay's body with the
// steady-state configDiskSyncDelay (interval-spread + draw over
// 2*spread+1, spread = interval/5 = 1s): a draw of 0 there yields 4s, not 0
// — the exact-zero assertion below fails. A comment-level argument suffices
// for the 4s branch, but the exact equality below already excludes it:
// 0 != 4s.
func TestConfigDiskSyncFirstDelayFormulaBoundaries(t *testing.T) {
	orig := configDiskSyncRandInt63
	t.Cleanup(func() { configDiskSyncRandInt63 = orig })

	for _, tc := range []struct {
		name string
		draw int64
		want time.Duration
	}{
		// Lowest value Int64N can return: the first delay is 0.
		{"lowest", 0, 0},
		// Highest value Int64N can return (n-1): interval - 1ns, never the
		// interval itself — the delay must stay strictly below it.
		{"highest", int64(configDiskSyncInterval) - 1, configDiskSyncInterval - time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDiskSyncRandInt63 = func(int64) int64 { return tc.draw }
			require.Equal(t, tc.want, defaultConfigDiskSyncFirstDelay(),
				"the default first-delay formula must map the boundary draw exactly; a steady-state configDiskSyncDelay body would map draw 0 to 4s, not 0")
		})
	}
}

// TestConfigDiskSyncFirstDelayBoundaryDraws pins the injectable first-delay
// seam: whatever configDiskSyncFirstDelay draws is passed to the wait
// UNCHANGED by runConfigDiskSync — including the boundary draws 0 and
// just-under-interval. Table-driven, no real sleeps (the wait stub parks on
// ctx.Done and the store is stopped in cleanup). With the round-6 refactor
// the default is defaultConfigDiskSyncFirstDelay; this test now pins that
// the seam is a pure passthrough of whatever is installed.
func TestConfigDiskSyncFirstDelayBoundaryDraws(t *testing.T) {
	for _, tc := range []struct {
		name string
		draw time.Duration
	}{
		{"zero", 0},
		{"just-under-interval", configDiskSyncInterval - time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, configPath, globalPath := configSyncIsolate(t)
			writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
			cfg := &Config{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}}}
			s, _ := configSyncWhiteBoxStore(t, dir, cfg, []string{configPath, globalPath})
			t.Cleanup(s.StopConfigDiskSync)

			oldDraw := configDiskSyncFirstDelay
			configDiskSyncFirstDelay = func() time.Duration { return tc.draw }
			t.Cleanup(func() { configDiskSyncFirstDelay = oldDraw })
			first := make(chan time.Duration, 1)
			oldWait := configDiskSyncWait
			configDiskSyncWait = func(ctx context.Context, delay time.Duration) error {
				select {
				case first <- delay:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			}
			t.Cleanup(func() { configDiskSyncWait = oldWait })
			s.StartConfigDiskSync(ConfigDiskSyncOptions{})
			select {
			case delay := <-first:
				require.Equal(t, tc.draw, delay, "runConfigDiskSync must pass the first-delay draw through unchanged")
			case <-time.After(2 * time.Second):
				t.Fatal("poller never ticked")
			}
		})
	}
}
