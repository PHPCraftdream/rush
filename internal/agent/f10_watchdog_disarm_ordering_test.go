// Test for F10 (task #572/#573 family): the stream watchdog must be
// disarmed on EVERY runTurn return path, not only the success path.
//
// wd.disarm() sits right before the success-path joinTitle() call inside
// runTurn (agent_turn.go, near the end of the function). Every early
// return — including the ordinary "the main provider call errored" path —
// only runs the DEFERRED joinTitle()/cancel() pair (registered near the top
// of runTurn, well before wd.disarm() is reached), so on those paths
// disarm() never executes. The deferred joinTitle() can still block for up
// to titleJoinGrace waiting on a hung title provider, and --timeout-hard-cap
// is a wall-clock deadline from turn start, independent of provider
// activity — so a turn whose main call already failed (or succeeded) just
// inside the hard cap can be pushed past it by the title-join wait alone,
// producing a spurious watchdog fire (a stall-dump log line and a second,
// pointless cancel of the very title being waited for) for a turn that had
// already finished its real work.
package agent

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// erroringModel's Stream fails immediately with a plain (non-cancel,
// non-deadline) error, driving runTurn's generic error early-return path
// (agent_turn.go: the `return nil, SessionAgentCall{}, false, err` at the
// tail of the post-stream error-handling block) — one of the return
// statements that sits BEFORE wd.disarm() and therefore only runs the
// deferred joinTitle()/cancel(), never the disarm call.
type erroringModel struct{}

func (erroringModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("boom: main provider call failed")
}

func (erroringModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("boom: main provider call failed")
}

func (erroringModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("boom: main provider call failed")
}

func (erroringModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("boom: main provider call failed")
}

func (erroringModel) Provider() string { return "test" }
func (erroringModel) Model() string    { return "erroring" }

// hangingTitleModel's Stream blocks until its ctx is cancelled, simulating a
// title provider that never returns on its own — exactly the scenario
// titleJoinGrace/joinTitle exists to bound. Returning ctx.Err() on unblock
// (rather than hanging the test) keeps the goroutine from leaking once the
// test's own deadline machinery (genCtx's eventual cancel) kicks in.
//
// done is closed by Stream right before it returns its ctx.Err(), so the
// test can PROVE the blocked call has actually unblocked before the test
// function itself returns — instead of assuming titleGenerationMaxDuration
// (1s here) is short enough that the orphaned goroutine can't outlive the
// test and race a later test's teardown. Construct via
// newHangingTitleModel; a zero-value model would close a nil channel and
// panic, so always use the constructor.
type hangingTitleModel struct {
	done chan struct{}
}

func newHangingTitleModel() hangingTitleModel {
	return hangingTitleModel{done: make(chan struct{})}
}

func (hangingTitleModel) Generate(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m hangingTitleModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	<-ctx.Done()
	// Signal the test that this call has genuinely unblocked and is
	// returning. generateTitle calls Stream on this model at most once per
	// Run (on error it falls through to the smart model, never back here),
	// and the test runs Run exactly once, so a plain close cannot
	// double-fire.
	close(m.done)
	return nil, ctx.Err()
}

func (hangingTitleModel) GenerateObject(ctx context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hangingTitleModel) StreamObject(ctx context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (hangingTitleModel) Provider() string { return "test" }
func (hangingTitleModel) Model() string    { return "hanging-title" }

// syncBuffer is a concurrency-safe io.Writer sink for slog.TextHandler,
// mirroring lockedBuffer in stream_watchdog_handlefire_test.go (unexported
// there, so duplicated here rather than exported across files for a single
// shared use).
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

// TestRunTurn_DisarmsWatchdogOnErrorReturn_NotJustSuccessPath is the F10
// regression test. It pins WHERE runTurn disarms the watchdog relative to
// an early (error) return — the exact gap
// p548_watchdog_disarm_test.go leaves open, since that file only proves
// startStreamWatchdog's disarm PRIMITIVE works in isolation, never that
// runTurn actually calls it on a non-success path.
//
// Setup: --timeout-hard-cap is set deliberately small (80ms) and
// titleJoinGrace deliberately larger (400ms). The main provider call fails
// immediately (well inside the hard cap), taking runTurn's generic
// post-stream error return. The title provider hangs (blocks on ctx.Done())
// so runTurn's deferred joinTitle() is forced to actually wait out close to
// the full titleJoinGrace window — a window that straddles the hard-cap
// deadline. If the watchdog is NOT disarmed before that wait begins, the
// hard cap fires again partway through it (causeHardCap), which
// handleWatchdogFire logs via slog.Warn as "turn exceeded
// --timeout-hard-cap". With the fix, disarm happens before the title join
// on every return path, so that log line must never appear.
//
// Revert-check: temporarily undoing the fix (moving disarm() back to
// ONLY the success-path location after joinTitle(), i.e. simulating the
// pre-fix ordering) makes this test fail — see the task report for the
// verbatim failure output.
func TestRunTurn_DisarmsWatchdogOnErrorReturn_NotJustSuccessPath(t *testing.T) {
	env := testEnv(t)

	hangingModel := newHangingTitleModel()
	agentIface := testSessionAgent(env, erroringModel{}, hangingModel, "test system prompt")
	sa := agentIface.(*sessionAgent)

	const hardCap = 80 * time.Millisecond
	const titleGrace = 400 * time.Millisecond
	sa.SetTimeoutOptions(false, hardCap)
	sa.titleJoinGrace = titleGrace
	// Must outlast hardCap (80ms) so it can't cut in before the hard cap
	// could refire, but otherwise as short as possible: hangingTitleModel
	// blocks the background title-generation goroutine (detached from
	// Run's own return via titleJoinGrace) for up to this whole duration,
	// orphaned and (before the explicit wait below was added) still
	// running well after this test itself reports its own PASS/FAIL --
	// confirmed via CI diagnostics this session (visible
	// as "sql: database is closed" errors from this exact kind of
	// leftover goroutine racing a later test's own db.Release teardown,
	// and as measurable scheduler/CPU pressure while it overlaps dozens of
	// subsequent tests). 1s is orders of magnitude longer than hardCap and
	// two orders shorter than the old 10s.
	sa.titleGenerationMaxDuration = 1 * time.Second
	sa.streamWatchdogTick = 5 * time.Millisecond // default tick is 30s -- far too coarse to observe an 80ms hard cap within this test's window

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	var logBuf syncBuffer
	prevDefault := slog.Default()
	prevLogOut, prevLogFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer func() {
		// slog.SetDefault(prevDefault) alone does NOT undo this: SetDefault
		// only calls log.SetOutput when the NEW handler isn't a
		// *defaultHandler, so restoring to a defaultHandler-backed logger
		// intentionally skips it (log/slog/logger.go's SetDefault comment) —
		// log's writer would otherwise stay pointed at logBuf, a dead local
		// variable, for the rest of the process, silently swallowing every
		// later slog.Error/Warn call made through the restored default.
		slog.SetDefault(prevDefault)
		log.SetOutput(prevLogOut)
		log.SetFlags(prevLogFlags)
	}()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = agentIface.Run(t.Context(), SessionAgentCall{
			Prompt:          "hello",
			SessionID:       sess.ID,
			MaxOutputTokens: 100,
		})
	}()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return — runTurn's deferred joinTitle()/<-wd.done must still bound the wait even with a hanging title provider")
	}

	// The title-generation goroutine is detached from Run's return by
	// titleJoinGrace and only unblocks when titleCtx's own
	// titleGenerationMaxDuration deadline (1s above) fires — potentially
	// AFTER this test has already reported a result, at which point its
	// wakeup can race a later test's db.Release teardown ("sql: database
	// is closed"). Wait for hangingTitleModel.Stream to actually return so
	// the goroutine is provably gone before this test function returns,
	// instead of assuming 1s is short enough.
	select {
	case <-hangingModel.done:
	case <-time.After(sa.titleGenerationMaxDuration + 2*time.Second):
		t.Fatal("title-generation goroutine never exited after its own internal timeout")
	}

	logged := logBuf.String()
	assert.NotContains(t, logged, "turn exceeded --timeout-hard-cap",
		"the watchdog fired AGAIN (causeHardCap) while runTurn's deferred joinTitle() was waiting out titleJoinGrace on the error-return path — "+
			"this proves wd.disarm() was not called before that wait began. The main provider call failed almost immediately, well inside "+
			"--timeout-hard-cap, so this turn's real work was long finished by the time the join's wait crossed the hard-cap deadline; the watchdog "+
			"must not be able to fire on a turn that already returned its result.")
}
