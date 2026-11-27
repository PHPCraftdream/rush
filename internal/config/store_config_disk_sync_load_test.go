package config

// Tests moved back from the cfgstore leaf (store_config_disk_sync_test.go):
// they drive the full config.Load stack and root test helpers, so they cannot
// live in the leaf without an import cycle. Leaf-specific access is via the
// cfgstore white-box surface (pointer seam aliases live in
// store_leaf_aliases_test.go).

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/store/cfgstore"

	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
	"go.uber.org/goleak"
)

func TestConfigDiskSyncInvalidJSONKeepsSnapshotAndWarnsOnce(t *testing.T) {
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	s, err := Load(dir, "", false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })

	var buf syncBuffer
	origLogger := slog.Default()
	oldLevel := slog.SetLogLoggerLevel(slog.LevelDebug)
	defer slog.SetLogLoggerLevel(oldLevel)
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	s.ConfigSyncMuForTest().Lock()
	*s.ConfigSyncOptsForTest() = ConfigDiskSyncOptions{Reload: s.ReloadFromDisk}
	s.ConfigSyncMuForTest().Unlock()

	gen := s.Generation()
	// Corrupt the workspace file (.rush/rush.json).
	workspace := (*s.LoadSnapshot().SnapWorkspacePath())
	require.NotEmpty(t, workspace)
	require.NoError(t, os.MkdirAll(filepath.Dir(workspace), 0o755))
	require.NoError(t, os.WriteFile(workspace, []byte("{not json"), 0o600))

	st := cfgstore.NewConfigDiskSyncStateForTest(false)
	for i := range 3 {
		result, tickErr := s.SyncConfigFromDisk(context.Background(), &st)
		require.NoError(t, tickErr)
		if i == 0 {
			require.Error(t, result.ReloadErr, "the first tick attempts the reload and reports its error")
		} else {
			require.NoError(t, result.ReloadErr, "identical digests are not retried")
			require.EqualValues(t, 1, *cfgstore.ConfigDiskSyncStateReloadAttempts(&st), "no retry until the digest set changes")
		}
		require.False(t, result.Reloaded)
		require.Equal(t, gen, s.Generation(), "a failed reload must keep the published snapshot")
	}
	require.EqualValues(t, 1, stAttempts(&st), "exactly one reload attempt per distinct digest set")
	warns := strings.Count(buf.String(), "Config disk sync reload failed")
	require.Equal(t, 1, warns, "one warning per distinct failed digest set")

	// Fix the file; the digest set changed, so exactly one more reload
	// succeeds and recovery is logged once.
	writeSignedModelsFixture(t, workspace, `{}`)
	result, err := s.SyncConfigFromDisk(context.Background(), &st)
	require.NoError(t, err)
	require.True(t, result.Reloaded)
	require.EqualValues(t, 2, stAttempts(&st))
	require.Equal(t, 1, strings.Count(buf.String(), "Config disk sync recovered"))
}

// attempts counts the reload attempts recorded on the state.
func stAttempts(st *cfgstore.ConfigDiskSyncState) uint64 {
	return *cfgstore.ConfigDiskSyncStateReloadAttempts(st)
}

// TestConfigDiskSyncIneligibleTrackedFileReloadsOnce pins the
// outcome-independent attempt memo: a tracked file the loader ignores (a
// directory at the path) keeps its zero fingerprint in the published
// snapshot while the digests never match, so without the memo every tick
// would run a full reload forever.

func TestConfigDiskSyncStopJoinsDuringInFlightReload(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	s, err := Load(dir, "", false)
	require.NoError(t, err)

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseReload := func() { releaseOnce.Do(func() { close(release) }) }
	// Fast deterministic ticks: the real first delay is random in
	// [0, interval), which a unit test must not wait on.
	oldWait := *configDiskSyncWait
	*configDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-time.After(5 * time.Millisecond):
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	stopDone := make(chan struct{})
	var stopLaunched atomic.Bool
	// Cleanup registered BEFORE anything blocks: it releases the parked
	// reload, restores the wait stub, and joins Stop with a bound, so ANY
	// failure path (assertion or deadline) drains the reload goroutine and
	// lets Stop's join finish — no goroutine leak, no goleak noise.
	t.Cleanup(func() {
		releaseReload()
		if stopLaunched.Load() {
			select {
			case <-stopDone:
			case <-time.After(2 * time.Second):
				// Only reachable under an already-failing deadlocked
				// mutant; the goroutine leak is that mutant's doing.
			}
		}
		*configDiskSyncWait = oldWait
	})
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
			s.UpdateConfig(func(cfgCopy *Config) {
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
	// Capture the poller's context UNDER the lock, BEFORE launching Stop:
	// Stop is what cancels it (store_config_disk_sync.go:121-123), so on
	// the normal path observing cancellation before Stop is launched can
	// never work. The observer below polls ctx.Err() — channel-free and,
	// crucially, lock-free: a mutant Stop that holds configSyncMu across
	// its WG join must not be able to starve the observation itself.
	s.ConfigSyncMuForTest().Lock()
	pollerCtx := *s.ConfigSyncCtxForTest()
	s.ConfigSyncMuForTest().Unlock()
	require.NotNil(t, pollerCtx, "StartConfigDiskSync must install a poller context")
	// A content change makes the next tick call the blocking Reload.
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-9"}}`)
	<-started

	// Stop FIRST, then observe its cancellation through the captured
	// context without touching configSyncMu.
	stopLaunched.Store(true)
	go func() {
		s.StopConfigDiskSync()
		close(stopDone)
	}()
	require.Eventually(t, func() bool {
		return pollerCtx.Err() != nil
	}, 2*time.Second, 5*time.Millisecond, "StopConfigDiskSync must cancel the poller context")

	// While the reload stays parked on release, Stop must NOT have returned.
	returnedEarly := false
	select {
	case <-stopDone:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	require.False(t, returnedEarly, "Stop must not return while the in-flight reload is still parked")
	releaseReload()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("StopConfigDiskSync did not return; deadlock on configSyncMu across WG.Wait")
	}

	// Start after Stop is refused: the cancelled context guarantees no new
	// poller goroutine.
	s.StartConfigDiskSync(ConfigDiskSyncOptions{
		Reload: func(context.Context) error { return nil },
	})
	require.Error(t, (*s.ConfigSyncCtxForTest()).Err(), "Start after Stop is refused")
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
	oldWait := *configDiskSyncWait
	*configDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
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
		*configDiskSyncWait = oldWait
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
	oldWait := *configDiskSyncWait
	*configDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
		ticks.Add(1)
		select {
		case <-release:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	oldAfter := *configDiskSyncAfterTick
	*configDiskSyncAfterTick = func(_ *ConfigStore, result ConfigDiskSyncResult) {
		if result.Reloaded {
			reloads.Add(1)
		}
	}
	t.Cleanup(func() {
		*configDiskSyncWait, *configDiskSyncAfterTick = oldWait, oldAfter
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
	library := cfgstore.NewLibraryStore(&Config{}, "")
	library.StartConfigDiskSync(ConfigDiskSyncOptions{})
	require.False(t, (*library.ConfigSyncStartedForTest()))
	// A non-empty working directory with nothing tracked (SDK library mode's
	// virtual root) starts nothing either: P3-6.
	virtual := cfgstore.NewLibraryStore(&Config{}, t.TempDir())
	virtual.StartConfigDiskSync(ConfigDiskSyncOptions{})
	require.False(t, (*virtual.ConfigSyncStartedForTest()))
}

// TestConfigDiskSyncOnModelsChangedOnlyOnModelDiff pins that
// OnModelsChanged fires only when the effective role models differ after a
// reload, not for unrelated content changes such as recent_models.

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
	// The edit targets the ACTIVE generation: with the gate on Load has
	// already migrated rush.json to rush.ktav, and an edit of the retained
	// rush.json would be ignored.
	active := selectConfigSlot(configPath)
	rawJSON, err := decodeConfigDocument(active, fileBytes(t, active))
	require.NoError(t, err)
	edited, err := sjson.Set(string(rawJSON), "models.smart.model", "hacked")
	require.NoError(t, err)
	edited, err = sjson.Set(edited, "options.debug", true)
	require.NoError(t, err)
	out := []byte(edited)
	if isKtavConfigPath(active) {
		out, err = ktavcodec.ConvertJSONToKtav(out)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(active, out, 0o600))

	require.NoError(t, s.ReloadFromDisk(context.Background()))
	require.Equal(t, "gpt-4", s.Config().Models[SelectedModelTypeSmart].Model,
		"an unsigned models edit must keep the previous role models")
	require.True(t, s.Config().Options.Debug,
		"the rest of the candidate must still publish")
}

// TestConfigDiskSyncStopBeforeStartIsTerminal pins the terminal stop state
// (role-models-a#1/#2): StopConfigDiskSync called BEFORE any Start records
// the stopped state even with nothing started, so a LATER Start is refused
// and launches no poller goroutine. Catches: dropping the
// `s.configSyncStopped ||` conjunct in StartConfigDiskSync lets the later
// Start create a fresh context and launch a poller — the tick counter below
// would move.

func TestConfigDiskSyncStopBeforeStartIsTerminal(t *testing.T) {
	dir, configPath, _ := configSyncIsolate(t)
	writeSignedModelsFixture(t, configPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	s, err := Load(dir, "", false)
	require.NoError(t, err)

	// Tick counter + parking wait stub: any (mutant) poller goroutine shows
	// up here and stays parked until cleanup releases it.
	var ticks atomic.Int32
	release := make(chan struct{})
	oldWait := *configDiskSyncWait
	*configDiskSyncWait = func(ctx context.Context, _ time.Duration) error {
		ticks.Add(1)
		select {
		case <-release:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() {
		s.StopConfigDiskSync() // idempotent; joins any (mutant) poller
		close(release)
		*configDiskSyncWait = oldWait
	})

	s.StopConfigDiskSync() // BEFORE any Start: must be terminal.
	require.False(t, s.ConfigDiskSyncRunning())

	s.StartConfigDiskSync(ConfigDiskSyncOptions{Reload: func(context.Context) error { return nil }})
	s.ConfigSyncMuForTest().Lock()
	started := *s.ConfigSyncStartedForTest()
	s.ConfigSyncMuForTest().Unlock()
	require.False(t, started, "Start after a terminal Stop must be refused")
	require.False(t, s.ConfigDiskSyncRunning())
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, ticks.Load(), "no poller goroutine may run after a terminal Stop")
}

// TestConfigDiskSyncOnModelsChangedEffortOnlyFires pins the COMPLETE
// effective-selection diff (role-models-a#4): each single-field change under
// the SAME provider/model id must fire OnModelsChanged exactly once, and a
// bytes change that leaves the effective selection identical must not.
// Catches (one assertion per mutant):
//   - dropping ReasoningEffort from configDiskModelProfile → the effort-only
//     step's diff is empty → no fire;
//   - dropping MaxTokens → the max_tokens-only step does not fire;
//   - dropping Think → the think-only step does not fire;
//   - dropping Temperature (P2-4) → the options-only step does not fire;
//   - dropping ProviderOptions (P2-4) → the provider_options-only step does
//     not fire;
//   - dropping TopP → the top_p-only step does not fire;
//   - dropping TopK → the top_k-only step does not fire;
//   - dropping FrequencyPenalty → the frequency_penalty-only step does not
//     fire;
//   - dropping PresencePenalty → the presence_penalty-only step does not
//     fire.

func TestReloadKeepsGlobalModelIntegrityAfterMidSessionKtavMigration(t *testing.T) {
	dir, _, globalPath := configSyncIsolate(t)
	restoreGate := SetKtavAutoMigrateForTest(true)
	t.Cleanup(restoreGate)

	// Startup observes an absent global slot and retains its JSON spelling.
	s, err := Load(dir, dir, false)
	require.NoError(t, err)
	t.Cleanup(func() { s.StopConfigDiskSync() })
	require.Equal(t, normalizeReloadPath(filepath.Join(filepath.Dir(globalPath), "rush.json")), *s.GlobalDataPathForTest())

	writeSignedModelsFixture(t, globalPath, `{"smart":{"provider":"openai","model":"gpt-4"}}`)
	require.NoError(t, s.ReloadFromDisk(context.Background()))
	active := selectConfigSlot(globalPath)
	require.True(t, isKtavConfigPath(active), "global config should migrate during reload")
	require.Equal(t, "gpt-4", s.Config().Models[SelectedModelTypeSmart].Model)

	// Change models without refreshing cache_rev.
	raw, err := os.ReadFile(active)
	require.NoError(t, err)
	jsonBytes, err := decodeConfigDocument(active, raw)
	require.NoError(t, err)
	edited, err := sjson.Set(string(jsonBytes), "models.smart.model", "hacked")
	require.NoError(t, err)
	editedBytes := []byte(edited)
	if isKtavConfigPath(active) {
		editedBytes, err = ktavcodec.ConvertJSONToKtav(editedBytes)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(active, editedBytes, 0o600))

	require.NoError(t, s.ReloadFromDisk(context.Background()))
	require.Equal(t, "gpt-4", s.Config().Models[SelectedModelTypeSmart].Model,
		"a stale global cache_rev must preserve the previous model after migration")
}
