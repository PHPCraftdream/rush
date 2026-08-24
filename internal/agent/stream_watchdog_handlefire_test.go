// Tests for sessionAgent.handleWatchdogFire, the production fire handler
// the watchdog onFire closure dispatches to: cause storage and the async
// goroutine-dump write (against the real crushlog write path), plus use of
// the largeModel snapshot in the diagnostic log. Hosts lockedBuffer.

package agent

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	crushlog "github.com/PHPCraftdream/rush/internal/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionAgent_HandleWatchdogFire_StoresCauseAndDispatchesDumpAsync is
// the regression test for task #243. TestStreamWatchdog_CauseStoredBeforeSlowDiagnosticWork
// and TestStreamWatchdog_AsyncDiagnosticWorkDoesNotDelayCancel (task #232,
// above) only prove that a TEST-LOCAL COPY of onFire's shape behaves as
// documented — neither one builds a *sessionAgent or calls the real
// production method, so reverting agent.go's actual ordering left both of
// those tests green (they test the copy, not the original). This test closes
// that gap by calling sessionAgent.handleWatchdogFire directly — the exact
// method runTurn's onFire closure dispatches to (see agent.go) — with no
// watchdog, no turn, no VCR involved.
//
// The write is the REAL crushlog.WriteGoroutineDump (not a fake). To make
// "the write is dispatched async and does not delay the return" a
// deterministic, non-flaky assertion rather than a timing guess against an
// ordinarily near-instant local disk write, this test installs a
// crushlog.SetWriteDelayHookForTest hook that blocks the real write at a
// known point until the test explicitly releases it — a genuine
// synchronization barrier, not a race against wall-clock I/O speed.
//
// An earlier version of this test tried to manufacture "slow" instead of
// "blocked": it pointed crushlog.SetLogDirForTest at a deliberately deep,
// not-yet-created 300-level directory so os.MkdirAll would take measurably
// long. That failed outright on macOS CI (build(macos-latest), 3 consecutive
// fork/main pushes) because the kernel enforces PATH_MAX=1024 there; the
// constructed path was ~3000+ bytes, so MkdirAll returned ENAMETOOLONG
// immediately instead of running slowly — the write never happened, so the
// polling assertion below spun to its budget every time regardless of size
// (task #320 misdiagnosed this as the CI runner being too slow under -race
// and only widened the budget, which could never fix a hard error). The
// hook-based version below has no directory depth and no platform-dependent
// timing assumption at all.
//
// It proves, against the REAL method:
//  1. watchdogCauseVal is stored with the real fired cause by the time
//     handleWatchdogFire returns (this is the field runTurn's post-stream
//     error path reads via watchdogCause(watchdogCauseVal.Load()) to decide
//     the user-facing finish message and tool-result text).
//  2. The method returns BEFORE the (deliberately held-open) real write has
//     completed — proving the write is genuinely dispatched on its own
//     goroutine, not awaited.
//  3. The write nonetheless completes shortly after being released and
//     lands real, readable goroutine-stack content on disk.
func TestSessionAgent_HandleWatchdogFire_StoresCauseAndDispatchesDumpAsync(t *testing.T) {
	dumpDir := t.TempDir()
	crushlog.SetLogDirForTest(t, dumpDir)

	hookEntered := make(chan struct{})
	release := make(chan struct{})
	crushlog.SetWriteDelayHookForTest(t, func() {
		close(hookEntered)
		<-release
	})

	smartModel := Model{
		ModelCfg: config.SelectedModel{
			Provider: "test-provider",
			Model:    "test-model",
		},
	}
	a := &sessionAgent{
		smartModel:     csync.NewValue(smartModel),
		timeoutHardCap: 5 * time.Second,
	}

	var watchdogCauseVal atomic.Int32
	watchdogCauseVal.Store(int32(causeIdleStall)) // zero value; must be overwritten

	const sessionID = "sess-243-test"
	const toolMaxDuration = 5 * time.Second
	const idleTimeout = 3 * time.Minute

	callReturned := make(chan struct{})
	go func() {
		a.handleWatchdogFire(causeToolTimeout, 7*time.Second, sessionID, &watchdogCauseVal, toolMaxDuration, idleTimeout, smartModel)
		close(callReturned)
	}()

	select {
	case <-callReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("handleWatchdogFire did not return — it must not block on the async goroutine-dump write")
	}

	// (1) The real cause must be stored by the time the call returns.
	assert.Equal(t, int32(causeToolTimeout), watchdogCauseVal.Load(),
		"handleWatchdogFire must store the real fired cause, not leave the zero value, "+
			"by the time it returns — this is what runTurn's error path reads to build the finish message")

	// The async goroutine must actually have started and reached the write
	// hook — proves the dump dispatch wasn't silently skipped, independent
	// of how fast the real I/O underneath it would have run.
	select {
	case <-hookEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the async goroutine-dump write never started — dispatch may have been skipped")
	}

	pattern := filepath.Join(dumpDir, "goroutines-"+strconv.Itoa(os.Getpid())+"-*.txt")

	// (2) The method must have returned BEFORE the real write, which is
	// still blocked on `release`, could possibly have completed. This is
	// now a deterministic fact (the write goroutine is parked inside the
	// hook), not a race against real disk speed.
	immediate, _ := filepath.Glob(pattern)
	assert.Empty(t, immediate,
		"handleWatchdogFire must return before the dump write completes; "+
			"a file present this early means the write was awaited inline, not dispatched async")

	close(release) // let the real write proceed

	// (3) The real write must complete shortly after being released and
	// land actual goroutine-stack content on disk — proving the async
	// dispatch genuinely happened rather than being silently skipped. The
	// content check lives INSIDE the predicate (not after it) so the poll
	// waits for the file to contain real data, not merely to exist:
	// os.WriteFile creates the file before writing its ~1 MiB body, so a
	// naive existence-only poll can observe a zero-byte file mid-write and
	// pass on empty content. 5s is a generous margin over the real,
	// now-unblocked MkdirAll+WriteFile cost — there is no more artificial
	// slowness to wait out.
	var dumpContent []byte
	var matches []string
	require.Eventually(t, func() bool {
		var err error
		matches, err = filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			return false
		}
		dumpContent, err = os.ReadFile(matches[0])
		if err != nil {
			return false
		}
		return bytes.Contains(dumpContent, []byte("stream watchdog fired")) &&
			bytes.Contains(dumpContent, []byte("goroutine "))
	}, 5*time.Second, 20*time.Millisecond, "expected a goroutine dump with real content to appear matching %s", pattern)
	require.Len(t, matches, 1, "expected exactly one dump file from this call")
	assert.Contains(t, string(dumpContent), "stream watchdog fired",
		"the dump must carry the reason handleWatchdogFire passes to crushlog.CaptureGoroutineStack")
	assert.Contains(t, string(dumpContent), "goroutine ", "the dump must contain real goroutine stacks")
}

// lockedBuffer is a concurrency-safe bytes.Buffer sink for slog.TextHandler,
// needed because handleWatchdogFire dispatches an async goroutine that also
// calls slog.Warn while the test reads the buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSessionAgent_HandleWatchdogFire_UsesPassedLargeModelSnapshot is the
// regression test for task #252. Task #243 (b55d701a) extracted runTurn's
// onFire closure into the named handleWatchdogFire method and claimed the
// extraction was "byte-for-byte the same logic". It was not: the original
// closure CAPTURED largeModel — the runTurn local taken ONCE at turn start
// (agent.go: `largeModel := a.largeModel.Get()`, before the turn loop) — so
// the diagnostic slog.Warn always named the model the turn STARTED with.
// The extraction instead re-read a.largeModel.Get() inside the method, at
// fire time. a.largeModel is MUTABLE during a turn: sessionAgent.SetModels
// is called from coordinator.UpdateModels and the web-UI override path, so a
// model switch mid-turn (provider A hangs, user switches to provider B in
// the web UI, watchdog fires after the switch) would log provider B as the
// one that hung — misattributing the diagnostic. This affects ONLY the
// diagnostic log, not the user-facing finish message, hence LOW severity.
//
// This test proves the fix: it calls handleWatchdogFire with an explicit
// largeModel parameter whose Provider/Model differ from a.largeModel's
// CURRENT value, and asserts the captured diagnostic log names the PASSED
// snapshot, not the live field. Because the diagnostic goes through slog
// (not a return value), the default slog.Logger is temporarily swapped for a
// TextHandler writing to a mutex-guarded buffer.
func TestSessionAgent_HandleWatchdogFire_UsesPassedSmartModelSnapshot(t *testing.T) {
	// Redirect the goroutine-dump dir so the async WriteGoroutineDump inside
	// handleWatchdogFire does not pollute the real log dir; a shallow tempdir
	// also keeps that write near-instant.
	crushlog.SetLogDirForTest(t, t.TempDir())

	// a.largeModel's CURRENT value — what the BUGGY code would read at fire
	// time via a.largeModel.Get().
	a := &sessionAgent{
		smartModel: csync.NewValue(Model{
			ModelCfg: config.SelectedModel{
				Provider: "current-provider",
				Model:    "current-model",
			},
		}),
		timeoutHardCap: 5 * time.Second,
	}

	// The SNAPSHOT runTurn would pass in — taken once at turn start. Simulate
	// a mid-turn model switch by making it differ from a.largeModel's value.
	snapshotModel := Model{
		ModelCfg: config.SelectedModel{
			Provider: "snapshot-provider",
			Model:    "snapshot-model",
		},
	}

	// Capture slog output: swap the default logger for a TextHandler over a
	// mutex-guarded buffer. handleWatchdogFire's own async dump-write
	// goroutine calls slog.Warn too, so the sink must be concurrency-safe.
	var logBuf lockedBuffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prevDefault)

	var watchdogCauseVal atomic.Int32
	a.handleWatchdogFire(
		causeIdleStall, 42*time.Second, "sess-252-test",
		&watchdogCauseVal, 5*time.Second, 3*time.Minute,
		snapshotModel,
	)

	require.Equal(t, int32(causeIdleStall), watchdogCauseVal.Load(),
		"cause must still be stored regardless of the model-source fix")

	logged := logBuf.String()
	assert.Contains(t, logged, "provider=snapshot-provider",
		"the diagnostic log must name the largeModel SNAPSHOT passed to handleWatchdogFire "+
			"(the runTurn value captured once at turn start), not the live a.largeModel field — "+
			"a mid-turn model switch (SetModels via web-UI) must not rewrite which provider/model the watchdog blames")
	assert.Contains(t, logged, "model=snapshot-model",
		"the diagnostic log must name the snapshot model, not the live field")
	assert.NotContains(t, logged, "current-provider",
		"the live a.largeModel.Provider (current-provider) must NOT appear — it reflects a model switch that happened AFTER the turn started, not the model that actually hung")
	assert.NotContains(t, logged, "current-model",
		"the live a.largeModel.Model (current-model) must NOT appear in the diagnostic")
}
