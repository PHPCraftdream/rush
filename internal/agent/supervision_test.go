package agent

// Supervision: wake the root session after chat silence while its scope
// still has open work (task #1043, docs/plans/2026-09-27-async-structured-
// concurrency.md §7, docs/plans/2026-09-27-wake-tools-contract.md §5.1/
// §5.2/§6/§7). Style follows work_ledger_timeout_test.go: bare *coordinator
// fixtures, direct manipulation of unexported state for deterministic
// backoff/pause assertions, short real deadlines (tens of ms) with
// require.Eventually for the two tests that exercise the real timer
// goroutine end to end.

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// latestSessionNoticeText returns the text of owner's most recently
// inserted session_notices row (phase-4 step 3: supervision now persists
// its check-in there instead of putting it directly on the woken call's
// Prompt -- a Drain call carries neither text nor NoticeKind of its own,
// see agent_drain.go). Fails the test if there are no rows.
func latestSessionNoticeText(t *testing.T, l *workLedger, owner string) string {
	t.Helper()
	notices, err := l.store.ListSessionNotices(t.Context(), owner)
	require.NoError(t, err)
	require.NotEmpty(t, notices, "expected at least one session_notices row for %s", owner)
	return notices[len(notices)-1].Text
}

// sessionNoticeTextAt returns the i-th (0-indexed, insertion order)
// session_notices row's text for owner.
func sessionNoticeTextAt(t *testing.T, l *workLedger, owner string, i int) string {
	t.Helper()
	notices, err := l.store.ListSessionNotices(t.Context(), owner)
	require.NoError(t, err)
	require.Greater(t, len(notices), i, "expected at least %d session_notices row(s) for %s", i+1, owner)
	return notices[i].Text
}

// newSupervisionTestLedger builds a bare workLedger+coordinator pair wired
// for supervision, with no timer service (tests that drive
// handleSupervisionDeadline directly do not need the real timer; armFunc is
// nil-receiver-safe, so a stray re-arm call from within it is a silent
// no-op).
func newSupervisionTestLedger(t *testing.T) (*workLedger, *coordinator) {
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry()}
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	l.coord = coord
	l.supervision = newSupervisionRegistry()
	coord.asyncJobs = l
	return l, coord
}

// startOpenJob registers and acknowledges a plain job for sessionID, exactly
// like startChildOwnedJob (work_ledger_delegation_test.go) -- l.running(id)
// is true afterward.
func startOpenJob(t *testing.T, l *workLedger, sessionID, toolCallID string) {
	t.Helper()
	_, _, err := l.Start(sessionID, toolCallID, "", "bash", "", true, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, sessionID, toolCallID))
}

// TestSupervision_NoteWorkStartedSkipsDelegatedChildArmsRoot: a session
// currently registered as a delegation driver's child is never armed for
// supervision (only the root is supervised, design doc §7); a session with
// no driver registered is armed.
//
// Revert-check performed: removed the
// `if _, isDelegatedChild := ...; isDelegatedChild { return }` guard from
// noteWorkStarted -- this test's child assertion FAILED (state was armed
// for the delegated child too). Restored the guard; re-ran, passed.
func TestSupervision_NoteWorkStartedSkipsDelegatedChildArmsRoot(t *testing.T) {
	l, coord := newSupervisionTestLedger(t)
	coord.subAgentDrivers.register("delegated-child", subAgentDriver{agent: &mockSessionAgent{}})

	l.noteWorkStarted(context.Background(), "delegated-child")
	l.supervision.mu.Lock()
	_, childArmed := l.supervision.byRoot["delegated-child"]
	l.supervision.mu.Unlock()
	require.False(t, childArmed, "a delegated child must never be armed for supervision directly")

	l.noteWorkStarted(context.Background(), "plain-root")
	l.supervision.mu.Lock()
	_, rootArmed := l.supervision.byRoot["plain-root"]
	l.supervision.mu.Unlock()
	require.True(t, rootArmed, "a session with no registered driver (the root) must be armed")
}

// TestSupervision_NoTickWithoutOpenWork: handleSupervisionDeadline for a
// session with no open work (l.running == false) must not wake anyone, and
// must drop the (now-stale) supervision state.
//
// Revert-check performed: removed the `if !l.running(rootSessionID) { ...;
// return }` block -- this test FAILED (the spy agent's Run was invoked with
// no open work at all). Restored the block; re-ran, passed.
func TestSupervision_NoTickWithoutOpenWork(t *testing.T) {
	l, coord := newSupervisionTestLedger(t)
	spy := &mockSessionAgent{runFunc: func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		t.Fatal("must not wake a session with no open work")
		return nil, nil
	}}
	coord.subAgentDrivers.register("idle-root", subAgentDriver{agent: spy})

	l.supervision.byRoot["idle-root"] = &supervisionState{
		rootSessionID: "idle-root",
		cfg:           DefaultSupervisionConfig(),
		interval:      DefaultSupervisionConfig().Interval,
		generation:    1,
	}

	l.handleSupervisionDeadline("idle-root", 1)
	time.Sleep(50 * time.Millisecond) // let any (wrongly) spawned goroutine run

	l.supervision.mu.Lock()
	_, stillArmed := l.supervision.byRoot["idle-root"]
	l.supervision.mu.Unlock()
	require.False(t, stillArmed, "supervision state for a session with no open work must be dropped")
}

// TestSupervision_NoTickWhileTurnRunning: handleSupervisionDeadline must not
// wake a session whose turn is currently running (IsSessionBusy == true),
// and must leave its tick counters untouched (a busy skip is not a
// no-progress tick).
//
// Revert-check performed: removed the
// `if l.coord.agentFor(...).IsSessionBusy(...) { return }` check -- this
// test FAILED (the busy spy's Run was invoked). Restored the check; re-ran,
// passed.
func TestSupervision_NoTickWhileTurnRunning(t *testing.T) {
	l, coord := newSupervisionTestLedger(t)
	startOpenJob(t, l, "busy-root", "job-1")

	busy := &busyStubAgent{}
	busy.setBusy(true)
	coord.subAgentDrivers.register("busy-root", subAgentDriver{agent: busy})

	l.supervision.byRoot["busy-root"] = &supervisionState{
		rootSessionID: "busy-root",
		cfg:           DefaultSupervisionConfig(),
		interval:      DefaultSupervisionConfig().Interval,
		generation:    1,
	}

	l.handleSupervisionDeadline("busy-root", 1)
	time.Sleep(50 * time.Millisecond)

	l.supervision.mu.Lock()
	st := l.supervision.byRoot["busy-root"]
	l.supervision.mu.Unlock()
	require.NotNil(t, st, "a busy skip must not drop the supervision state")
	require.Zero(t, st.tickCount, "a busy skip must not count as a no-progress tick")
}

// TestSupervision_TicksAfterSilenceWithOpenWork: the real timer goroutine,
// armed with a short deadline, wakes the root through coordinator.wakeSession
// with NoticeKind "supervision" and a summary naming the open job.
//
// Revert-check performed: added an unconditional `return` at the top of
// handleSupervisionDeadline (after the nil checks) -- this test FAILED
// (timed out waiting for the wake). Removed the early return; re-ran,
// passed.
func TestSupervision_TicksAfterSilenceWithOpenWork(t *testing.T) {
	t.Parallel()
	l, coord := newSupervisionTestLedger(t)
	l.timeouts = newTimeoutService(l)
	defer l.timeouts.close()

	received := make(chan SessionAgentCall, 1)
	agent := &mockSessionAgent{runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		received <- call
		return agentResultWithText("ok, waiting"), nil
	}}
	coord.subAgentDrivers.register("tick-root", subAgentDriver{agent: agent})

	startOpenJob(t, l, "tick-root", "call-abc")

	l.supervision.byRoot["tick-root"] = &supervisionState{
		rootSessionID: "tick-root",
		cfg:           SupervisionConfig{Enabled: true, Interval: 30 * time.Millisecond, MaxInterval: 200 * time.Millisecond, MaxNoProgress: 6},
		interval:      30 * time.Millisecond,
		generation:    1,
	}
	l.timeouts.armFunc(time.Now().Add(30*time.Millisecond), func() { l.handleSupervisionDeadline("tick-root", 1) })

	select {
	case call := <-received:
		require.True(t, call.IsDrain, "a supervision tick wakes via a Drain call, not a text-carrying one")
		require.Empty(t, call.NoticeKind, "a Drain call itself carries no NoticeKind -- the pull reconstructs it from the row")
	case <-time.After(2 * time.Second):
		t.Fatal("supervision tick never reached agent.Run")
	}

	text := latestSessionNoticeText(t, l, "tick-root")
	require.Contains(t, text, "Supervision check-in (tick 1")
	require.Contains(t, text, "1 background job(s) running")
	require.Contains(t, text, "call-abc")
	require.Contains(t, text, "job_output")
	require.Contains(t, text, "job_kill")
	require.NotContains(t, text, "inspect_agent")
	require.NotContains(t, text, "stop_agent")
}

// TestSupervision_RecordProgressPushesDeadlineForward: recordProgress
// invalidates a pending (here, a simulated grown-by-backoff) deadline and
// re-arms one at the BASE interval from now -- proving progress resets the
// countdown instead of leaving the stale, far-future deadline to fire.
// Margins are deliberately huge (500ms stale deadline vs. a sub-300ms
// expected fire) to stay robust against scheduler jitter on a shared,
// memory-capped test machine, per the task's "no real multi-minute sleeps"
// but otherwise ordinary timing-test discipline.
//
// Revert-check performed: made recordProgress a no-op (early return) --
// this test FAILED (timed out waiting almost 500ms for the stale deadline,
// past the test's 300ms bound, instead of firing quickly after
// recordProgress). Restored recordProgress; re-ran, passed.
func TestSupervision_RecordProgressPushesDeadlineForward(t *testing.T) {
	t.Parallel()
	l, coord := newSupervisionTestLedger(t)
	l.timeouts = newTimeoutService(l)
	defer l.timeouts.close()

	var mu sync.Mutex
	var calls []time.Time
	snapshotCalls := func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), calls...)
	}
	done := make(chan struct{}, 1)
	agent := &mockSessionAgent{runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		mu.Lock()
		calls = append(calls, time.Now())
		mu.Unlock()
		done <- struct{}{}
		return agentResultWithText("ok"), nil
	}}
	coord.subAgentDrivers.register("reset-root", subAgentDriver{agent: agent})
	startOpenJob(t, l, "reset-root", "call-1")

	start := time.Now()
	// MaxNoProgress: 1 makes the post-recordProgress tick pause immediately
	// instead of re-arming a legitimate NEXT tick under a newer generation --
	// isolating "did the invalidated generation-1 deadline itself fire" from
	// "does the system correctly keep ticking on its own" (covered by
	// TestSupervision_BackoffGrowsThenPausesThenResetsOnProgress), which
	// would otherwise produce more real calls during the long wait below and
	// make this assertion meaningless.
	cfg := SupervisionConfig{Enabled: true, Interval: 50 * time.Millisecond, MaxInterval: 500 * time.Millisecond, MaxNoProgress: 1}
	// Simulate a deadline already grown far out by prior no-progress ticks.
	// nextGen is seeded to match the manually-assigned generation so
	// recordProgress's bump produces a GENUINELY new value (2, not a
	// coincidental collision back to 1) -- otherwise the staleness check
	// below would pass for the wrong reason.
	l.supervision.nextGen = 1
	l.supervision.byRoot["reset-root"] = &supervisionState{rootSessionID: "reset-root", cfg: cfg, interval: 500 * time.Millisecond, generation: 1}
	l.timeouts.armFunc(start.Add(500*time.Millisecond), func() { l.handleSupervisionDeadline("reset-root", 1) })

	time.Sleep(20 * time.Millisecond)
	l.recordProgress("reset-root") // resets interval to cfg.Interval (50ms) and re-arms from now, invalidating generation 1

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervision tick never fired after recordProgress")
	}
	got := snapshotCalls()
	require.Len(t, got, 1, "recordProgress's invalidated generation-1 deadline must not ALSO fire")
	require.Less(t, got[0].Sub(start), 300*time.Millisecond,
		"progress must pull the tick back to roughly base-interval-from-now, not leave the stale ~500ms grown deadline to fire")

	// Prove the staleness check, not timing luck, is what protects this:
	// wait past the original (now-invalidated) 500ms deadline and confirm
	// it never produces a second call.
	time.Sleep(600 * time.Millisecond)
	require.Len(t, snapshotCalls(), 1, "the stale generation-1 deadline must never fire, even once its original 500ms elapses")
}

// TestSupervision_PushDeadlineOnTurnEndKeepsBackoffButReschedules: unlike
// recordProgress, a plain turn-end push (onSessionIdleHook's contribution --
// "any new message... resets the countdown" for a turn with no async-job
// notice at all) must NOT reset the backoff it already grew: only the
// deadline moves, using the CURRENT (already-grown) interval.
//
// Revert-check performed: changed pushDeadlineOnTurnEnd's re-arm to also
// reset st.interval/st.tickCount to base (copying recordProgress's body) --
// this test FAILED ("tick 3" became "tick 1"). Restored the narrower
// deadline-only push; re-ran, passed.
func TestSupervision_PushDeadlineOnTurnEndKeepsBackoffButReschedules(t *testing.T) {
	t.Parallel()
	l, coord := newSupervisionTestLedger(t)
	l.timeouts = newTimeoutService(l)
	defer l.timeouts.close()

	var prompts []string
	done := make(chan struct{}, 1)
	agent := &mockSessionAgent{runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		prompts = append(prompts, call.Prompt)
		done <- struct{}{}
		return agentResultWithText("ok"), nil
	}}
	coord.subAgentDrivers.register("turnend-root", subAgentDriver{agent: agent})
	startOpenJob(t, l, "turnend-root", "call-1")

	cfg := SupervisionConfig{Enabled: true, Interval: 10 * time.Millisecond, MaxInterval: 500 * time.Millisecond, MaxNoProgress: 6}
	// Simulate a session already two ticks into backoff (grown to 40ms),
	// currently scheduled far in the future.
	l.supervision.byRoot["turnend-root"] = &supervisionState{
		rootSessionID: "turnend-root", cfg: cfg, interval: 40 * time.Millisecond, tickCount: 2, generation: 1,
	}
	l.timeouts.armFunc(time.Now().Add(2*time.Second), func() { l.handleSupervisionDeadline("turnend-root", 1) })

	time.Sleep(20 * time.Millisecond)
	l.pushDeadlineOnTurnEnd("turnend-root") // simulates onSessionIdleHook firing after an unrelated turn ends

	l.supervision.mu.Lock()
	st := l.supervision.byRoot["turnend-root"]
	require.Equal(t, 2, st.tickCount, "pushDeadlineOnTurnEnd must not touch tickCount")
	require.Equal(t, 40*time.Millisecond, st.interval, "pushDeadlineOnTurnEnd must not touch interval size")
	l.supervision.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the rescheduled deadline never fired")
	}
	require.Len(t, prompts, 1)
	text := latestSessionNoticeText(t, l, "turnend-root")
	require.Contains(t, text, "tick 3", "backoff continues from where it left off (3rd tick), not reset to 1")
}

// TestSupervision_PushDeadlineOnTurnEndSkipsPausedSession: a mere turn end
// must not resume a paused session -- only recordProgress does.
//
// Revert-check performed: removed the `|| st.paused` clause from
// pushDeadlineOnTurnEnd's stale-check -- this test FAILED (generation
// changed for a paused session). Restored the clause; re-ran, passed.
func TestSupervision_PushDeadlineOnTurnEndSkipsPausedSession(t *testing.T) {
	l, _ := newSupervisionTestLedger(t)
	l.supervision.byRoot["paused-root"] = &supervisionState{rootSessionID: "paused-root", cfg: DefaultSupervisionConfig(), paused: true, generation: 5}

	l.pushDeadlineOnTurnEnd("paused-root")

	l.supervision.mu.Lock()
	st := l.supervision.byRoot["paused-root"]
	l.supervision.mu.Unlock()
	require.EqualValues(t, 5, st.generation, "a paused session's generation must not change on a mere turn-end push")
	require.True(t, st.paused)
}

// TestSupervision_AfterTurnPushesDeadline: the turn epilogue (runOwned's
// afterTurn) pushes the supervision deadline after every turn that reached
// the provider -- but not after a Drain that ended without reaching it
// (opening a tab must not reset the root's silence timer).
//
// Revert-check: dropping the pushDeadlineOnTurnEnd call from afterTurn turns
// the first assertion red; pushing for every Drain leg turns the second red.
func TestSupervision_AfterTurnPushesDeadline(t *testing.T) {
	l, _ := newSupervisionTestLedger(t)
	l.supervision.nextGen = 1
	for _, id := range []string{"hook-root", "quiet-root"} {
		l.supervision.byRoot[id] = &supervisionState{
			rootSessionID: id, cfg: DefaultSupervisionConfig(), interval: 5 * time.Millisecond, generation: 1,
		}
	}
	sa := &sessionAgent{asyncJobs: l}

	sa.afterTurn(SessionAgentCall{SessionID: "hook-root"}, nil, nil, false)
	l.supervision.mu.Lock()
	pushed := l.supervision.byRoot["hook-root"].generation
	l.supervision.mu.Unlock()
	require.NotEqual(t, uint64(1), pushed, "a turn that reached the provider must push the supervision deadline")

	noTurn := &drainAttempt{sessionID: "quiet-root", outcome: drainNoTurn}
	sa.afterTurn(newDrainCall(SessionAgentCall{SessionID: "quiet-root"}), noTurn, nil, false)
	l.supervision.mu.Lock()
	quiet := l.supervision.byRoot["quiet-root"].generation
	l.supervision.mu.Unlock()
	require.EqualValues(t, 1, quiet, "a no-turn Drain must not reset the silence timer")
}

// TestSupervision_BackoffGrowsThenPausesThenResetsOnProgress drives
// consecutive ticks manually (no real timer) through the exact sequence the
// design doc's "5 -> 10 -> 20 -> ... -> 60, cap; pause after 6" example
// describes, scaled to milliseconds, then proves recordProgress resumes a
// paused session from its base interval.
//
// Revert-check performed (two, restored after each): (1) removed
// `st.interval = growInterval(...)` -- interval stayed at its base value
// forever, and the "interval capped at MaxInterval" assertion below FAILED.
// (2) removed `if lastTick { st.paused = true }` -- a 4th tick fired instead
// of being blocked, and the "exactly 3 calls, then none" assertion FAILED.
// Both restored; re-ran, passed.
func TestSupervision_BackoffGrowsThenPausesThenResetsOnProgress(t *testing.T) {
	l, coord := newSupervisionTestLedger(t)
	startOpenJob(t, l, "backoff-root", "call-1")

	var mu sync.Mutex
	var prompts []string
	addPrompt := func(p string) {
		mu.Lock()
		prompts = append(prompts, p)
		mu.Unlock()
	}
	promptCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(prompts)
	}
	agent := &mockSessionAgent{runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		addPrompt(call.Prompt)
		return agentResultWithText("ok"), nil
	}}
	coord.subAgentDrivers.register("backoff-root", subAgentDriver{agent: agent})

	cfg := SupervisionConfig{Enabled: true, Interval: 10 * time.Millisecond, MaxInterval: 40 * time.Millisecond, MaxNoProgress: 3}
	l.supervision.byRoot["backoff-root"] = &supervisionState{rootSessionID: "backoff-root", cfg: cfg, interval: cfg.Interval, generation: 1}

	fireAndWait := func(gen uint64, wantCalls int) {
		t.Helper()
		l.handleSupervisionDeadline("backoff-root", gen)
		require.Eventually(t, func() bool { return promptCount() >= wantCalls }, 2*time.Second, 5*time.Millisecond)
	}

	fireAndWait(1, 1) // tick 1: 10ms -> 20ms
	l.supervision.mu.Lock()
	st := l.supervision.byRoot["backoff-root"]
	require.Equal(t, 1, st.tickCount)
	require.Equal(t, 20*time.Millisecond, st.interval)
	require.False(t, st.paused)
	gen2 := st.generation
	l.supervision.mu.Unlock()
	require.Contains(t, sessionNoticeTextAt(t, l, "backoff-root", 0), "tick 1")

	fireAndWait(gen2, 2) // tick 2: 20ms -> 40ms (== cap)
	l.supervision.mu.Lock()
	st = l.supervision.byRoot["backoff-root"]
	require.Equal(t, 2, st.tickCount)
	require.Equal(t, 40*time.Millisecond, st.interval, "must cap at MaxInterval, not keep doubling")
	gen3 := st.generation
	l.supervision.mu.Unlock()

	fireAndWait(gen3, 3) // tick 3 == MaxNoProgress: last tick, then pause
	l.supervision.mu.Lock()
	st = l.supervision.byRoot["backoff-root"]
	require.Equal(t, 3, st.tickCount)
	require.True(t, st.paused)
	staleGenAfterPause := st.generation
	l.supervision.mu.Unlock()
	require.Contains(t, sessionNoticeTextAt(t, l, "backoff-root", 2), "paused after 3 consecutive check-ins")

	// A stale fire against the paused generation must not produce a 4th call.
	l.handleSupervisionDeadline("backoff-root", staleGenAfterPause)
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, 3, promptCount(), "a paused session must not tick again on its own")

	// recordProgress resumes it: base interval, tickCount cleared, unpaused.
	l.recordProgress("backoff-root")
	l.supervision.mu.Lock()
	st = l.supervision.byRoot["backoff-root"]
	require.False(t, st.paused)
	require.Zero(t, st.tickCount)
	require.Equal(t, cfg.Interval, st.interval)
	resumedGen := st.generation
	l.supervision.mu.Unlock()
	require.NotEqual(t, staleGenAfterPause, resumedGen)

	fireAndWait(resumedGen, 4)
	require.Contains(t, sessionNoticeTextAt(t, l, "backoff-root", 3), "tick 1", "a resumed session's next tick starts counting from 1 again")
}

// TestSupervision_StateRemovedOnScopeClose: once a session's last open job
// is delivered, deliverLocked drops its supervision state in the SAME
// critical section that empties bySession[x].jobs -- the exact primitive
// rush run's own scope predicate (doc sec.3.5, coordinator.ScopeOpen) reads
// via l.running -- so a supervision timer can never outlive, or delay
// noticing, the scope it was armed for.
//
// Revert-check performed: removed both
// `if len(s.jobs) == 0 { l.clearSupervisionIfPresent(owner) }` calls from
// deliverLocked -- this test FAILED (supervision state was still present
// after the job finished). Restored both; re-ran, passed.
func TestSupervision_StateRemovedOnScopeClose(t *testing.T) {
	l, _ := newSupervisionTestLedger(t)
	startOpenJob(t, l, "closing-root", "call-1")
	l.supervision.byRoot["closing-root"] = &supervisionState{rootSessionID: "closing-root", cfg: DefaultSupervisionConfig(), generation: 1}
	require.True(t, l.running("closing-root"), "the job must still be open before it finishes")

	l.finish(jobOf(l, "closing-root", "call-1"), jobResult{content: "done"})

	l.supervision.mu.Lock()
	_, present := l.supervision.byRoot["closing-root"]
	l.supervision.mu.Unlock()
	require.False(t, present, "supervision state must be dropped the instant the scope's last job is delivered")

	// The same critical section is what l.running (and, through it, the
	// scope predicate rush run's own exit condition reads, doc sec.3.5)
	// observes -- proving supervision cannot hold the scope open.
	require.False(t, l.running("closing-root"), "the scope must be closed, unblocked by any supervision timer")
}

// TestSupervision_CancelSessionAlsoClearsState covers cancelSession's path
// (session-level cancel), which deletes a plain job directly rather than
// through deliverLocked -- clearSupervisionIfPresent's own call site inside
// cancelSession is what covers this, not deliverLocked's hook.
func TestSupervision_CancelSessionAlsoClearsState(t *testing.T) {
	l, _ := newSupervisionTestLedger(t)
	startOpenJob(t, l, "cancel-root", "call-1")
	l.supervision.byRoot["cancel-root"] = &supervisionState{rootSessionID: "cancel-root", cfg: DefaultSupervisionConfig(), generation: 1}

	l.cancelSession("cancel-root")

	l.supervision.mu.Lock()
	_, present := l.supervision.byRoot["cancel-root"]
	l.supervision.mu.Unlock()
	require.False(t, present, "cancelSession must drop supervision state for the cancelled session")
}

// TestSupervision_SingleGoroutineRegardlessOfSessionCount mirrors
// TestTimeoutService_SingleGoroutineForManyJobs: arming supervision for N
// distinct root sessions must not spawn N goroutines -- the SAME shared
// timeoutService services both asyncJob deadlines and supervision deadlines.
func TestSupervision_SingleGoroutineRegardlessOfSessionCount(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()
	l, _ := newSupervisionTestLedger(t)
	l.timeouts = newTimeoutService(l)
	defer l.timeouts.close()

	const n = 50
	for i := 0; i < n; i++ {
		sessionID := fmt.Sprintf("root-%d", i)
		startOpenJob(t, l, sessionID, "call-1")
		l.noteWorkStarted(context.Background(), sessionID)
	}

	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	require.LessOrEqual(t, after-before, 3,
		"arming supervision for %d sessions must not spawn %d goroutines, got a delta of %d", n, n, after-before)
}

// TestDropSupersededSupervisionNotices: only the newest NoticeKind
// "supervision" message keeps its text; older ones stay in place (role
// alternation is preserved) with a short marker; everything else is untouched.
func TestDropSupersededSupervisionNotices(t *testing.T) {
	t.Parallel()
	mk := func(id, noticeKind, text string) message.Message {
		return message.Message{
			ID: id, Role: message.User, NoticeKind: noticeKind,
			Parts: []message.ContentPart{message.TextContent{Text: text}},
		}
	}
	in := []message.Message{
		mk("1", "", "hello"),
		mk("2", noticeKindSupervision, "tick 1"),
		mk("3", "job_stopped", "stopped"),
		mk("4", noticeKindSupervision, "tick 2"),
		mk("5", noticeKindSupervision, "tick 3 paused"),
		mk("6", "", "bye"),
	}
	out := dropSupersededSupervisionNotices(in)

	require.Equal(t, []string{"1", "2", "3", "4", "5", "6"}, idsOf(out), "no message is removed")
	texts := make([]string, len(out))
	for i, m := range out {
		texts[i] = m.Content().Text
	}
	require.Equal(t, []string{"hello", supersededSupervisionText, "stopped", supersededSupervisionText, "tick 3 paused", "bye"}, texts)
	require.Equal(t, "tick 1", in[1].Content().Text, "the input slice must not be mutated")
}

func idsOf(msgs []message.Message) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}
