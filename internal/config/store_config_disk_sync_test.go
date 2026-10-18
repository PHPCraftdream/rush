// REVERT-CHECKS (orchestrator runs the mutants); one line per test.
// TestConfigDiskSyncForcedTickCatchesSameStat -> removing the st.tick%configDiskSyncForcedEvery forced check in syncConfigFromDisk (store_config_disk_sync.go).
// TestConfigDiskSyncNoReloadOnMetadataOnlyChange -> making configDiskSyncDigestsMatch compare metadata instead of digest (store_config_disk_sync.go).
// TestConfigDiskSyncNoReloadWhenSettled -> dropping the st.settled short-circuit in syncConfigFromDisk (store_config_disk_sync.go).
// TestConfigDiskSyncIneligibleTrackedFileReloadsOnce -> dropping the st.lastAttemptKey outcome-independent memo in configDiskSyncReload (store_config_disk_sync.go).
// TestConfigDiskSyncMemoClearedWhenForeignPathAdoptsDisk -> removing the st.lastAttemptKey clear in syncConfigFromDisk's settled branch (store_config_disk_sync.go).
// TestConfigDiskSyncInvalidJSONKeepsSnapshotAndWarnsOnce -> ignoring st.failedKey in configDiskSyncReload, so every tick retries and re-warns (store_config_disk_sync.go).
// TestConfigDiskSyncStopJoinsCancelledPoller -> making StopConfigDiskSync skip configSyncWG.Wait (store_config_disk_sync.go).
// TestConfigDiskSyncJitterBounds -> replacing the first-delay uniform draw in runConfigDiskSync with configDiskSyncDelay (store_config_disk_sync.go).
// TestConfigDiskSyncOnModelsChangedOnlyOnModelDiff -> firing OnModelsChanged on every reload instead of a model diff (store_config_disk_sync.go).
// TestReloadRejectsUnsignedModelsEdit -> removing the reloadModelsIntegrityMismatch block in buildAndPublishReload (store_reload.go).
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
	"go.uber.org/goleak"
)

// configSyncIsolate redirects every global config path to fresh temp dirs
// and returns the workspace dir, the project config path and the global
// data config path; call it before any store is constructed.
func configSyncIsolate(t *testing.T) (dir, configPath, globalPath string) {
	t.Helper()
	dir = t.TempDir()
	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)
	configPath = filepath.Join(dir, "rush.json")
	globalPath = GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
	return dir, configPath, globalPath
}

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
		fingerprint, err := readReloadFingerprint(path)
		if err != nil {
			continue
		}
		next.snapshots[path] = fileSnapshot{
			Path: path, Exists: fingerprint.exists, Size: fingerprint.size,
			ModTime: fingerprint.modTime, ContentHash: fingerprint.digest,
			fingerprint: fingerprint,
		}
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
	tracked, snapshots := reloadStalenessState(paths, fingerprints)
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
		fingerprint, err := readReloadFingerprint(path)
		require.NoError(t, err)
		// Key the same way reloadStalenessState keys its snapshots.
		fingerprints[normalizeDiscoveryPath(path)] = fingerprint
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
func TestConfigDiskSyncInvalidJSONKeepsSnapshotAndWarnsOnce(t *testing.T) {
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })

	var buf syncBuffer
	oldLevel := slog.SetLogLoggerLevel(slog.LevelDebug)
	defer slog.SetLogLoggerLevel(oldLevel)
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	s.configSyncMu.Lock()
	s.configSyncOpts = ConfigDiskSyncOptions{Reload: s.ReloadFromDisk}
	s.configSyncMu.Unlock()

	gen := s.Generation()
	// Corrupt the workspace file (.rush/rush.json).
	workspace := s.loadSnapshot().workspacePath
	require.NotEmpty(t, workspace)
	require.NoError(t, os.MkdirAll(filepath.Dir(workspace), 0o755))
	require.NoError(t, os.WriteFile(workspace, []byte("{not json"), 0o600))

	var st configDiskSyncState
	for i := range 3 {
		result, tickErr := s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, tickErr)
		if i == 0 {
			require.Error(t, result.ReloadErr, "the first tick attempts the reload and reports its error")
		} else {
			require.NoError(t, result.ReloadErr, "identical digests are not retried")
			require.EqualValues(t, 1, st.reloadAttempts, "no retry until the digest set changes")
		}
		require.False(t, result.Reloaded)
		require.Equal(t, gen, s.Generation(), "a failed reload must keep the published snapshot")
	}
	require.EqualValues(t, 1, st.attempts(), "exactly one reload attempt per distinct digest set")
	warns := strings.Count(buf.String(), "Config disk sync reload failed")
	require.Equal(t, 1, warns, "one warning per distinct failed digest set")

	// Fix the file; the digest set changed, so exactly one more reload
	// succeeds and recovery is logged once.
	writeSignedModelsFixture(t, workspace, `{}`)
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded)
	require.EqualValues(t, 2, st.attempts())
	require.Equal(t, 1, strings.Count(buf.String(), "Config disk sync recovered"))
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// attempts counts the reload attempts recorded on the state.
func (st *configDiskSyncState) attempts() uint64 { return st.reloadAttempts }

// TestConfigDiskSyncIneligibleTrackedFileReloadsOnce pins the
// outcome-independent attempt memo: a tracked file the loader ignores (a
// directory at the path) keeps its zero fingerprint in the published
// snapshot while the digests never match, so without the memo every tick
// would run a full reload forever.
func TestConfigDiskSyncIneligibleTrackedFileReloadsOnce(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	// A directory at the global data path is present but ineligible:
	// eligibleConfigCandidate rejects directories, so Load tracks it with
	// the zero fingerprint while the poller still reads it on every tick.
	require.NoError(t, os.Mkdir(globalPath, 0o755))
	cfg := &Config{Models: map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-4"}}}
	configFingerprint, err := readReloadFingerprint(configPath)
	require.NoError(t, err)
	s, calls := configSyncWhiteBoxStoreFingerprints(t, dir, cfg,
		[]string{configPath, globalPath},
		map[string]reloadFileFingerprint{
			normalizeDiscoveryPath(configPath): configFingerprint,
			normalizeDiscoveryPath(globalPath): {},
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
	configFingerprint, err := readReloadFingerprint(configPath)
	require.NoError(t, err)
	s, calls := configSyncWhiteBoxStoreFingerprints(t, dir, cfg,
		[]string{configPath, globalPath},
		map[string]reloadFileFingerprint{
			normalizeDiscoveryPath(configPath): configFingerprint,
			normalizeDiscoveryPath(globalPath): {},
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
func TestConfigDiskSyncStopJoinsDuringInFlightReload(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	s, err := Load(dir, "", false)
	require.NoError(t, err)

	started := make(chan struct{})
	release := make(chan struct{})
	// Fast deterministic ticks: the real first delay is random in
	// [0, interval), which a unit test must not wait on.
	oldWait := configDiskSyncWait
	configDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-time.After(5 * time.Millisecond):
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() { configDiskSyncWait = oldWait })
	s.StartConfigDiskSync(ConfigDiskSyncOptions{
		Reload: func(context.Context) error {
			select {
			case <-started:
			default:
				close(started)
			}
			// Deliberately ignoring ctx here: Stop's cancel must not
			// short-circuit the reload, or the success path (and the
			// configSyncMu take) is never reached.
			<-release
			// Change the published models so the models diff succeeds and
			// the tick really calls configSyncOnModelsChangedFunc (the
			// configSyncMu take that deadlocked against Stop's Wait).
			s.updateConfig(func(cfgCopy *Config) {
				cfgCopy.Models = map[SelectedModelType]SelectedModel{
					SelectedModelTypeSmart: {Provider: "openai", Model: "gpt-5"},
				}
			})
			return nil // Success: the tick proceeds to the models diff.
		},
		// Non-nil so the successful-reload path takes configSyncMu in
		// configSyncOnModelsChangedFunc while Stop waits on the WG.
		OnModelsChanged: func(ModelsChange) {},
	})
	// A content change makes the next tick call the blocking Reload.
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-9"}}`)
	<-started

	done := make(chan struct{})
	go func() {
		s.StopConfigDiskSync()
		close(done)
	}()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("StopConfigDiskSync did not return; deadlock on configSyncMu across WG.Wait")
	}

	// Start after Stop is refused: the cancelled context guarantees no new
	// poller goroutine.
	s.StartConfigDiskSync(ConfigDiskSyncOptions{
		Reload: func(context.Context) error { return nil },
	})
	require.Error(t, s.configSyncCtx.Err(), "Start after Stop is refused")
}

func TestConfigDiskSyncStopJoinsCancelledPoller(t *testing.T) {
	// Revert-check: a StopConfigDiskSync that cancels without joining
	// returns while the poller goroutine is still blocked after cancellation.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	entered, cancelled := make(chan struct{}, 2), make(chan struct{}, 2)
	var releaseOnce sync.Once
	release := make(chan struct{})
	releasePoller := func() { releaseOnce.Do(func() { close(release) }) }
	oldWait := configDiskSyncWait
	configDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		select {
		case cancelled <- struct{}{}:
		default:
		}
		<-release
		return ctx.Err()
	}
	// Registered after the stub assignment and restoring it last, after the
	// release, so a failing body can never leave the stub blocked.
	t.Cleanup(func() {
		releasePoller()
		configDiskSyncWait = oldWait
	})
	s.StartConfigDiskSync(ConfigDiskSyncOptions{Reload: func(context.Context) error { return nil }})
	awaitCodexSignal(t, entered)
	stopped := make(chan struct{})
	go func() { defer close(stopped); s.StopConfigDiskSync() }()
	awaitCodexSignal(t, cancelled)
	returnedEarly := false
	select {
	case <-stopped:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	require.False(t, returnedEarly, "StopConfigDiskSync returned before the cancelled poller goroutine was released")
	releasePoller()
	awaitCodexSignal(t, stopped)
	require.False(t, s.ConfigDiskSyncRunning())
}

func TestConfigDiskSyncLifecycle(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	s, err := Load(dir, dir, false)
	require.NoError(t, err)

	var reloads atomic.Int32
	var ticks atomic.Int32
	release := make(chan struct{})
	oldWait := configDiskSyncWait
	configDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
		ticks.Add(1)
		select {
		case <-release:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	oldAfter := configDiskSyncAfterTick
	configDiskSyncAfterTick = func(_ *ConfigStore, result ConfigDiskSyncResult) {
		if result.Reloaded {
			reloads.Add(1)
		}
	}
	t.Cleanup(func() {
		configDiskSyncWait, configDiskSyncAfterTick = oldWait, oldAfter
		close(release)
	})

	s.StartConfigDiskSync(ConfigDiskSyncOptions{Reload: func(context.Context) error { return nil }})
	s.StartConfigDiskSync(ConfigDiskSyncOptions{Reload: func(context.Context) error { return nil }})
	require.Eventually(t, func() bool { return ticks.Load() >= 1 }, 2*time.Second, 5*time.Millisecond)
	first := ticks.Load()
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, first, ticks.Load(), "StartConfigDiskSync twice must spawn one goroutine")

	s.StopConfigDiskSync()
	joined := ticks.Load()
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, joined, ticks.Load(), "StopConfigDiskSync must join the poller")

	// The background poller is dead: no more ticks even though the file
	// changed. PollConfigDiskOnce is an explicit refresh API and still
	// works on demand.
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-5"}}`)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, joined, ticks.Load(), "no background tick fires after Stop")
	result, err := s.PollConfigDiskOnce(context.Background())
	require.NoError(t, err)
	require.True(t, result.Reloaded, "explicit polls keep working after Stop")

	s.StartConfigDiskSync(ConfigDiskSyncOptions{Reload: func(context.Context) error { return nil }})
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, joined, ticks.Load(), "Start after Stop is refused")
	require.Zero(t, reloads.Load())

	// A store without a working directory never starts anything.
	library := NewLibraryStore(&Config{}, "")
	library.StartConfigDiskSync(ConfigDiskSyncOptions{})
	require.False(t, library.configSyncStarted)
	// A non-empty working directory with nothing tracked (SDK library mode's
	// virtual root) starts nothing either: P3-6.
	virtual := NewLibraryStore(&Config{}, t.TempDir())
	virtual.StartConfigDiskSync(ConfigDiskSyncOptions{})
	require.False(t, virtual.configSyncStarted)
}

// TestConfigDiskSyncJitterBounds pins the delay contract: the first delay
// is uniform in [0, interval) and every later delay is in
// [0.8, 1.2] * interval.
func TestConfigDiskSyncJitterBounds(t *testing.T) {
	require.Equal(t, 5*time.Second, configDiskSyncInterval)
	for range 1000 {
		delay := configDiskSyncDelay()
		require.GreaterOrEqual(t, delay, 4*time.Second)
		require.LessOrEqual(t, delay, 6*time.Second)
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

// TestConfigDiskSyncOnModelsChangedOnlyOnModelDiff pins that
// OnModelsChanged fires only when the effective role models differ after a
// reload, not for unrelated content changes such as recent_models.
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
func TestReloadRejectsUnsignedModelsEdit(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	require.NoError(t, os.WriteFile(globalPath, []byte("{}"), 0o600))
	s, err := Load(dir, dir, false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Equal(t, "gpt-4", s.Config().Models[SelectedModelTypeSmart].Model)

	// Hand edit: change the model id AND adopt an unrelated options field,
	// leaving the original cache_rev untouched (stale signature).
	raw := string(fileBytes(t, configPath))
	edited, err := sjson.Set(raw, "models.smart.model", "hacked")
	require.NoError(t, err)
	edited, err = sjson.Set(edited, "options.debug", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte(edited), 0o600))

	require.NoError(t, s.ReloadFromDisk(context.Background()))
	require.Equal(t, "gpt-4", s.Config().Models[SelectedModelTypeSmart].Model,
		"an unsigned models edit must keep the previous role models")
	require.True(t, s.Config().Options.Debug,
		"the rest of the candidate must still publish")
}

// TestConfigDiskSyncPathSetDriftReloads pins that candidate-path drift alone
// triggers a reload in both directions. Catches: the forced-tick drift check
// only setting statChanged while the reload branch stays gated on the digest
// match over the OLD tracked paths, so a project-boundary change with every
// previously tracked file byte-identical never reloads.
func TestConfigDiskSyncPathSetDriftReloads(t *testing.T) {
	t.Run("Growth", func(t *testing.T) {
		dir, configPath, globalPath := configSyncIsolate(t)
		writeSignedModelsFixture(t, globalPath, `{"smart":{"provider":"openai","model":"gpt-global"}}`)
		// A DIRECTORY at the project candidate path is dropped by
		// eligibleConfigCandidate, so the candidate (and tracked) set at
		// Load excludes it.
		require.NoError(t, os.Mkdir(configPath, 0o755))
		s, err := Load(dir, "", false)
		require.NoError(t, err)
		t.Cleanup(func() { s.StopConfigDiskSync() })
		require.Equal(t, "gpt-global", s.Config().Models[SelectedModelTypeSmart].Model)

		var st configDiskSyncState
		result, err := s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, err)
		require.False(t, result.Reloaded, "an unchanged candidate set must not reload")
		gen := s.Generation()

		// The directory becomes a real eligible config: the candidate set
		// GREW while every previously tracked file is byte-identical.
		require.NoError(t, os.Remove(configPath))
		writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-project"}}`)

		st2 := configDiskSyncState{force: true}
		result, err = s.syncConfigFromDisk(context.Background(), &st2)
		require.NoError(t, err)
		require.True(t, result.Reloaded, "path-set drift must reload on its own")
		require.Greater(t, s.Generation(), gen)
		require.Equal(t, "gpt-project", s.Config().Models[SelectedModelTypeSmart].Model,
			"the new eligible config must reach the store")
		require.True(t, slices.Contains(s.loadSnapshot().trackedConfigPaths, canonicalConfigPath(configPath)),
			"the new candidate must join the tracked set")

		// Drift cleared: a later forced tick does not reload again.
		result, err = s.syncConfigFromDisk(context.Background(), &st2)
		require.NoError(t, err)
		require.False(t, result.Reloaded, "drift must be exactly-once per event")
	})

	t.Run("Removal", func(t *testing.T) {
		dir, configPath, globalPath := configSyncIsolate(t)
		writeSignedModelsFixture(t, globalPath, `{"smart":{"provider":"openai","model":"gpt-global"}}`)
		writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-project"}}`)
		s, err := Load(dir, "", false)
		require.NoError(t, err)
		t.Cleanup(func() { s.StopConfigDiskSync() })
		require.Equal(t, "gpt-project", s.Config().Models[SelectedModelTypeSmart].Model)
		require.True(t, slices.Contains(s.loadSnapshot().trackedConfigPaths, canonicalConfigPath(configPath)))
		gen := s.Generation()

		// The eligible project config leaves the candidate set: a directory
		// at the same path is dropped by eligibleConfigCandidate, so the
		// reload's merge must shed the removed file's contributions.
		require.NoError(t, os.Remove(configPath))
		require.NoError(t, os.Mkdir(configPath, 0o755))

		st := configDiskSyncState{force: true}
		result, err := s.syncConfigFromDisk(context.Background(), &st)
		require.NoError(t, err)
		require.True(t, result.Reloaded, "the removed candidate must leave the merge")
		require.Greater(t, s.Generation(), gen)
		require.Equal(t, "gpt-global", s.Config().Models[SelectedModelTypeSmart].Model,
			"the removed file's models must revert to the remaining sources")
		require.False(t, slices.Contains(s.loadSnapshot().trackedConfigPaths, canonicalConfigPath(configPath)),
			"the removed candidate must leave the tracked set")
	})
}

// TestConfigDiskSyncDriftFailureRecoversWhenFileFixed pins recovery after a
// failed drift reload. Catches: failedKey hashing only the previously
// tracked paths, so fixing a newly eligible malformed config never retries
// the drift reload.
func TestConfigDiskSyncDriftFailureRecoversWhenFileFixed(t *testing.T) {
	dir, configPath, globalPath := configSyncIsolate(t)
	writeSignedModelsFixture(t, globalPath, `{"smart":{"provider":"openai","model":"gpt-global"}}`)
	// A DIRECTORY at the project candidate path is dropped by
	// eligibleConfigCandidate, so Load's candidate set excludes it.
	require.NoError(t, os.Mkdir(configPath, 0o755))
	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	gen := s.Generation()

	var buf syncBuffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	// The directory becomes an eligible but MALFORMED config: the drift
	// reload is attempted once and fails.
	require.NoError(t, os.Remove(configPath))
	require.NoError(t, os.WriteFile(configPath, []byte("{not json"), 0o600))

	st := configDiskSyncState{force: true}
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.Error(t, result.ReloadErr)
	require.False(t, result.Reloaded)
	require.EqualValues(t, 1, st.reloadAttempts)
	require.Equal(t, gen, s.Generation(), "a failed reload must keep the published snapshot")
	require.Equal(t, 1, strings.Count(buf.String(), "Config disk sync reload failed"))

	// Unchanged bytes: the memo keeps the tick quiescent.
	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.EqualValues(t, 1, st.reloadAttempts, "unchanged candidates must not retry")
	require.Equal(t, 1, strings.Count(buf.String(), "Config disk sync reload failed"))

	// Fixing the newly eligible file changes the candidate key: exactly one
	// recovery reload runs and the fixed config reaches the store.
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-fixed"}}`)
	result, err = s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.NoError(t, result.ReloadErr)
	require.True(t, result.Reloaded, "fixing the malformed candidate must retry the drift reload")
	require.EqualValues(t, 2, st.reloadAttempts)
	require.Greater(t, s.Generation(), gen)
	require.Equal(t, "gpt-fixed", s.Config().Models[SelectedModelTypeSmart].Model)
	require.Equal(t, 1, strings.Count(buf.String(), "Config disk sync recovered"))
}
