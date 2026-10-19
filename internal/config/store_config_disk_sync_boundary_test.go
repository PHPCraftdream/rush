// Boundary/drift family of the config disk-sync poller tests, split from
// store_config_disk_sync_test.go (file-length limit): candidate-path drift
// in both directions, recovery after a failed drift reload, a candidate-set
// transition on the global-config path (drift-only oracle, no git), and the
// first-delay/jitter scheduling contract.

// REVERT-CHECKS (orchestrator runs the mutants); one line per test.
// TestConfigDiskSyncPathSetDriftReloads -> the forced-tick drift check only setting statChanged while the reload branch stays digest-gated over the OLD tracked paths (store_config_disk_sync.go).
// TestConfigDiskSyncDriftFailureRecoversWhenFileFixed -> hashing the failedKey only over the previously tracked paths (store_config_disk_sync.go).
// TestConfigDiskSyncCandidateSetDriftBoundary -> removing the forced-tick candidate-set drift check (store_config_disk_sync.go:196): the global-config growth transition (dir → eligible file) is invisible to the digest gate, which only examines tracked paths, so the mutant settles without reloading.
// TestConfigDiskSyncJitterBounds -> replacing the first-delay uniform draw in runConfigDiskSync with configDiskSyncDelay, or replacing defaultConfigDiskSyncFirstDelay's [0, interval) draw with an out-of-bounds draw (store_config_disk_sync.go).
// TestConfigDiskSyncFirstDelayBoundaryDraws -> making runConfigDiskSync clamp or rewrite the configDiskSyncFirstDelay draw before passing it to the wait (store_config_disk_sync.go).
package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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

// TestConfigDiskSyncCandidateSetDriftBoundary pins a candidate-path-set
// transition that lookupConfigCandidates itself cannot observe as a byte
// change (role-models-a.fix1#1, redesigned round 6): the global CONFIG file
// (GlobalConfig(), isolated via RUSH_GLOBAL_CONFIG — note this is NEITHER
// the workspace path NOR the global data path, which store_config_disk_sync.go's
// configDiskSyncCandidatePaths appends unconditionally and which therefore
// can never drift) is replaced by a DIRECTORY (eligibleConfigCandidate
// drops directories, load_files.go:184-200), shrinking the candidate set
// while every previously tracked file is byte-identical, and back to a real
// signed file, growing it again. Each transition must perform exactly one
// reload via the forced tick's sameReloadPathSet drift check
// (store_config_disk_sync.go:196), and a quiescent third tick must not
// reload. No git needed.
// REVERT-CHECK: removing the `drift := forced && !sameReloadPathSet(...)`
// line fails the GROWTH assertion — the newly eligible global-config file is
// not in the tracked set, so configDiskSyncDigestsMatch (which only examines
// tracked paths) matches and the mutant settles without reloading. (The
// SHRINK direction is also caught by the digest gate, since the
// now-ineligible tracked path's fingerprint read fails; growth is the
// drift-only oracle.)
func TestConfigDiskSyncCandidateSetDriftBoundary(t *testing.T) {
	dir, configPath, _ := configSyncIsolate(t)
	globalCfg := GlobalConfig()
	require.NotEqual(t, configPath, globalCfg)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-project"}}`)
	// A DIRECTORY at the global-config candidate path: dropped by
	// eligibleConfigCandidate, so the candidate (and tracked) set at Load
	// excludes it. The store's models come from the project file only.
	require.NoError(t, os.MkdirAll(filepath.Dir(globalCfg), 0o755))
	require.NoError(t, os.Mkdir(globalCfg, 0o755))
	s, err := Load(dir, dir, false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Equal(t, "gpt-project", s.Config().Models[SelectedModelTypeSmart].Model)
	require.False(t, slices.Contains(s.loadSnapshot().trackedConfigPaths, canonicalConfigPath(globalCfg)),
		"the ineligible global-config path must not be tracked at Load")

	// Settle on the initial candidate set.
	var st configDiskSyncState
	st.force = true
	result, err := s.syncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.False(t, result.Reloaded, "an unchanged candidate set must not reload")
	baseAttempts := st.reloadAttempts
	gen := s.Generation()

	// Transition 1 (GROWTH): the directory becomes a real eligible signed
	// config — the candidate set GREW while every previously tracked file
	// is byte-identical. Exactly one reload, driven by the drift check
	// alone.
	//
	// NOTE: Load was called with (dir, dir, false), so dir/rush.json is
	// BOTH the project candidate and the workspace path, and the workspace
	// config merges LAST (append([][]byte{mustMarshalConfig(cfg)}, wsData),
	// load.go:132-138; same in ReloadFromDisk, store_reload.go:452-457) —
	// the workspace file SHADOWS the newly eligible global-config candidate
	// for models.smart, so no assertion here may depend on "gpt-globalcfg"
	// reaching the store. Growth is proven by the drift reload itself
	// (Reloaded, exactly one reloadAttempts, generation bump) plus the
	// candidate joining the tracked set below. This keeps the drift-only
	// oracle intact: the digest gate only examines TRACKED paths, and the
	// new candidate is not tracked.
	require.NoError(t, os.Remove(globalCfg))
	writeSignedModelsFixture(t, globalCfg, `{"smart":{"provider":"openai","model":"gpt-globalcfg"}}`)
	st2 := configDiskSyncState{force: true}
	result, err = s.syncConfigFromDisk(context.Background(), &st2)
	require.NoError(t, err)
	require.True(t, result.Reloaded, "the candidate-set growth must reload on the drift check alone")
	require.EqualValues(t, baseAttempts+1, st2.reloadAttempts, "exactly one reload per transition")
	require.Greater(t, s.Generation(), gen)
	require.True(t, slices.Contains(s.loadSnapshot().trackedConfigPaths, canonicalConfigPath(globalCfg)),
		"the new candidate must join the tracked set")

	// Transition 2 (SHRINK): the file becomes a directory again — the
	// candidate set shrinks back. Exactly one more reload.
	require.NoError(t, os.Remove(globalCfg))
	require.NoError(t, os.Mkdir(globalCfg, 0o755))
	result, err = s.syncConfigFromDisk(context.Background(), &st2)
	require.NoError(t, err)
	require.True(t, result.Reloaded, "the candidate-set shrink must reload")
	require.EqualValues(t, baseAttempts+2, st2.reloadAttempts, "exactly one reload per transition")
	require.Equal(t, "gpt-project", s.Config().Models[SelectedModelTypeSmart].Model,
		"the removed candidate's models must revert to the remaining sources")

	// No third reload: the drift is exactly-once per transition.
	result, err = s.syncConfigFromDisk(context.Background(), &st2)
	require.NoError(t, err)
	require.False(t, result.Reloaded)
	require.EqualValues(t, baseAttempts+2, st2.reloadAttempts)
}

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
	// coverage is per-draw bounds here plus the injectable seam's
	// passthrough boundary draws in TestConfigDiskSyncFirstDelayBoundaryDraws.
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
