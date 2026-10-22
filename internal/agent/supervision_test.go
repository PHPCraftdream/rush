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
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// supervisionTestConns maps each fixture coordinator to its raw connection
// so the notice-read helpers can reach the session_notices store without
// changing the fixture signature. Test-only bookkeeping.
var supervisionTestConns sync.Map // *coordinator -> *sql.DB

// supervisionTestNotices returns the notice store over the fixture's conn.
func supervisionTestNotices(t *testing.T, c *coordinator) *session.SessionNoticeStore {
	t.Helper()
	v, ok := supervisionTestConns.Load(c)
	if !ok {
		t.Fatalf("no connection registered for the supervision fixture coordinator")
	}
	return session.NewSessionNoticeStore(v.(*sql.DB))
}

// latestSessionNoticeText returns the text of owner's most recently
// inserted session_notices row (phase-4 step 3: supervision persists its
// check-in there instead of putting it directly on the woken call's Prompt
// -- a Drain call carries neither text nor NoticeKind of its own, see
// agent_drain.go). Fails the test if there are no rows.
func latestSessionNoticeText(t *testing.T, c *coordinator, owner string) string {
	t.Helper()
	notices, err := supervisionTestNotices(t, c).ListSessionNotices(t.Context(), owner)
	require.NoError(t, err)
	require.NotEmpty(t, notices, "expected at least one session_notices row for %s", owner)
	return notices[len(notices)-1].Text
}

// sessionNoticeTextAt returns the i-th (0-indexed, insertion order)
// session_notices row's text for owner.
func sessionNoticeTextAt(t *testing.T, c *coordinator, owner string, i int) string {
	t.Helper()
	notices, err := supervisionTestNotices(t, c).ListSessionNotices(t.Context(), owner)
	require.NoError(t, err)
	require.Greater(t, len(notices), i, "expected at least %d session_notices row(s) for %s", i+1, owner)
	return notices[i].Text
}

// newSupervisionTestCoordinator builds a bare coordinator wired for
// supervision: the rehomed runtime state, the shared timeout service, and
// the batch node facts bound over a fresh store, with the connection
// registered for the notice-read helpers. Tests that drive
// handleSupervisionDeadline directly do not need the real timer (armFunc is
// used explicitly).
//
// The facts binding matters: supervisionWorkOpen (supervision.go) answers
// the open-work question through the bound facts' LiveWorkSummary -- an
// unbound coordinator would get the conservative unknown source and fail
// OPEN (work always "open", so e.g.
// TestSupervision_NoTickWithoutOpenWork's stale state would never be
// dropped), and a bound coordinator with an empty batch db fails CLOSED
// (scope closed), which is why the open-work tests seed a live batch row
// via seedSupervisionOpenWork below.
func newSupervisionTestCoordinator(t *testing.T) *coordinator {
	coord := newBatchRuntimeCoordinator()
	conn, dataDir := batchFixtureConn(t)
	bindBatchNodeFacts(t, coord, conn, dataDir)
	supervisionTestConns.Store(coord, conn)
	t.Cleanup(func() { supervisionTestConns.Delete(coord) })
	return coord
}

// seedSupervisionOpenWork seeds sessionID's open work in batch_nodes:
//  1. an open scope -- supervisionWorkOpen reads LiveWorkSummary through
//     the bound node facts, so without a live batch row the deadline drops
//     the state as "scope closed" and never ticks;
//  2. launchable debt -- the tick's wake travels wakeSession ->
//     drainDecision, which reads SelectedDebtExists from the bound batch
//     store; an empty batch db means the tick fires but the wake is a no-op
//     and agent.Run is never reached. seedBatchJobDebt supplies a real
//     finished-but-unreacted selected child notice (delivery=pending).
func seedSupervisionOpenWork(t *testing.T, c *coordinator, sessionID, toolCallID string) {
	t.Helper()
	v, ok := supervisionTestConns.Load(c)
	if !ok {
		t.Fatalf("no connection registered for the supervision fixture coordinator")
	}
	conn := v.(*sql.DB)
	seedBatchDelegation(t, conn, sessionID, toolCallID, "", "command", "running", false)
	seedBatchJobDebt(t, conn, sessionID, toolCallID+"-debt", false)
}

// startOpenJob seeds a live running batch root for sessionID -- the open
// work a supervision tick requires.
func startOpenJob(t *testing.T, c *coordinator, sessionID, toolCallID string) {
	t.Helper()
	v, ok := supervisionTestConns.Load(c)
	if !ok {
		t.Fatalf("no connection registered for the supervision fixture coordinator")
	}
	seedBatchDelegation(t, v.(*sql.DB), sessionID, toolCallID, "", "command", "running", false)
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
	coord := newSupervisionTestCoordinator(t)
	coord.subAgentDrivers.register("delegated-child", subAgentDriver{agent: &mockSessionAgent{}})

	coord.noteWorkStarted(context.Background(), "delegated-child")
	coord.supervision.mu.Lock()
	_, childArmed := coord.supervision.byRoot["delegated-child"]
	coord.supervision.mu.Unlock()
	require.False(t, childArmed, "a delegated child must never be armed for supervision directly")

	coord.noteWorkStarted(context.Background(), "plain-root")
	coord.supervision.mu.Lock()
	_, rootArmed := coord.supervision.byRoot["plain-root"]
	coord.supervision.mu.Unlock()
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
	coord := newSupervisionTestCoordinator(t)
	spy := &mockSessionAgent{runFunc: func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		t.Fatal("must not wake a session with no open work")
		return nil, nil
	}}
	coord.subAgentDrivers.register("idle-root", subAgentDriver{agent: spy})

	coord.supervision.byRoot["idle-root"] = &supervisionState{
		rootSessionID: "idle-root",
		cfg:           DefaultSupervisionConfig(),
		interval:      DefaultSupervisionConfig().Interval,
		generation:    1,
	}

	coord.handleSupervisionDeadline("idle-root", 1)
	time.Sleep(50 * time.Millisecond) // let any (wrongly) spawned goroutine run

	coord.supervision.mu.Lock()
	_, stillArmed := coord.supervision.byRoot["idle-root"]
	coord.supervision.mu.Unlock()
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
	coord := newSupervisionTestCoordinator(t)
	seedSupervisionOpenWork(t, coord, "busy-root", "job-1")

	busy := &busyStubAgent{}
	busy.setBusy(true)
	coord.subAgentDrivers.register("busy-root", subAgentDriver{agent: busy})

	coord.supervision.byRoot["busy-root"] = &supervisionState{
		rootSessionID: "busy-root",
		cfg:           DefaultSupervisionConfig(),
		interval:      DefaultSupervisionConfig().Interval,
		generation:    1,
	}

	coord.handleSupervisionDeadline("busy-root", 1)
	time.Sleep(50 * time.Millisecond)

	coord.supervision.mu.Lock()
	st := coord.supervision.byRoot["busy-root"]
	coord.supervision.mu.Unlock()
	require.NotNil(t, st, "a busy skip must not drop the supervision state")
	require.Zero(t, st.tickCount, "a busy skip must not count as a no-progress tick")
}

// TestSupervision_DeadlineWithoutAgentIsANoOpNotAPanic: handleSupervisionDeadline
// runs on the timeout service's goroutine, so a nil dereference there kills the
// whole process — and the full internal/agent run died exactly this way
// (supervision.go's agentFor call, a fire armed by an earlier test whose
// coordinator was a bare struct literal). agentFor falls back to the
// coordinator's currentAgent, and a fixture coordinator has none: the fire
// must be a no-op — no tick, no check-in notice, state untouched — never a
// dereference.
//
// Revert-check performed: removed the agent==nil guard — this test CRASHED the
// test binary with a nil pointer dereference at the agentFor call. Restored
// the guard; re-ran, passed.
func TestSupervision_DeadlineWithoutAgentIsANoOpNotAPanic(t *testing.T) {
	coord := newSupervisionTestCoordinator(t)
	require.Nil(t, coord.currentAgent,
		"precondition: the fixture coordinator has no agent — exactly the crash shape")

	// Under flag ON the open-work read goes through the bound batch facts,
	// so the seed must be dual-mode (see seedSupervisionOpenWork).
	seedSupervisionOpenWork(t, coord, "agentless-root", "sup-no-agent")
	coord.noteWorkStarted(context.Background(), "agentless-root")
	coord.supervision.mu.Lock()
	st, armed := coord.supervision.byRoot["agentless-root"]
	var gen uint64
	if st != nil {
		gen = st.generation
	}
	coord.supervision.mu.Unlock()
	require.True(t, armed, "precondition: the root was armed for supervision")

	coord.handleSupervisionDeadline("agentless-root", gen)

	coord.supervision.mu.Lock()
	_, stillArmed := coord.supervision.byRoot["agentless-root"]
	coord.supervision.mu.Unlock()
	require.True(t, stillArmed, "a fire with no agent to ask must not mutate the state")
	notices, err := supervisionTestNotices(t, coord).ListSessionNotices(t.Context(), "agentless-root")
	require.NoError(t, err)
	require.Empty(t, notices, "no check-in notice may be persisted without an agent")
}

// TestSupervision_TicksAfterSilenceWithOpenWork: the real timer goroutine,
// armed with a short deadline, wakes the root through coordinator.wakeSession
// with NoticeKind "supervision" and a summary naming the open job.
//
// Revert-check performed: added an unconditional `return` at the top of
// handleSupervisionDeadline (after the nil checks) -- this test FAILED
// (timed out waiting for the wake). Removed the early return; re-ran,
// passed.
//
// LEGACY-AUTHORITY ORACLE, flag OFF: the asserted check-in text names the
// LEGACY guidance and tool set ("job_output", "job_kill", "inspect_agent",
// "stop_agent", produced by buildSupervisionSummaryAt's legacy branch,
// supervision.go:516). Under RUSH_BATCH_RUNTIME=1 the batch summary is a
// different, deliberately narrower text (buildSupervisionSummaryBatch,
// supervision.go:601: "Inspect with batch_output, or stop with
// batch_stop."); no batch equivalent of the legacy tool guidance exists,
// so this test pins the legacy branch only.
func TestSupervision_TicksAfterSilenceWithOpenWork(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "0")
	coord := newSupervisionTestCoordinator(t)
	coord.timeouts = newTimeoutService()
	defer coord.timeouts.close()

	received := make(chan SessionAgentCall, 1)
	agent := &mockSessionAgent{runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		received <- call
		return agentResultWithText("ok, waiting"), nil
	}}
	coord.subAgentDrivers.register("tick-root", subAgentDriver{agent: agent})

	startOpenJob(t, coord, "tick-root", "call-abc")

	coord.supervision.byRoot["tick-root"] = &supervisionState{
		rootSessionID: "tick-root",
		cfg:           SupervisionConfig{Enabled: true, Interval: 30 * time.Millisecond, MaxInterval: 200 * time.Millisecond, MaxNoProgress: 6},
		interval:      30 * time.Millisecond,
		generation:    1,
	}
	coord.timeouts.armFunc(time.Now().Add(30*time.Millisecond), func() { coord.handleSupervisionDeadline("tick-root", 1) })

	select {
	case call := <-received:
		require.True(t, call.IsDrain, "a supervision tick wakes via a Drain call, not a text-carrying one")
		require.Empty(t, call.NoticeKind, "a Drain call itself carries no NoticeKind -- the pull reconstructs it from the row")
	case <-time.After(2 * time.Second):
		t.Fatal("supervision tick never reached agent.Run")
	}

	text := latestSessionNoticeText(t, coord, "tick-root")
	require.Contains(t, text, "Supervision check-in (tick 1")
	require.Contains(t, text, "1 background job(s) running")
	require.Contains(t, text, "call-abc")
	require.Contains(t, text, "batch_output", "the batch inspection guidance survives")
	require.Contains(t, text, "batch_stop")
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
	coord := newSupervisionTestCoordinator(t)
	coord.timeouts = newTimeoutService()
	defer coord.timeouts.close()

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
	seedSupervisionOpenWork(t, coord, "reset-root", "call-1")

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
	coord.supervision.nextGen = 1
	coord.supervision.byRoot["reset-root"] = &supervisionState{rootSessionID: "reset-root", cfg: cfg, interval: 500 * time.Millisecond, generation: 1}
	coord.timeouts.armFunc(start.Add(500*time.Millisecond), func() { coord.handleSupervisionDeadline("reset-root", 1) })

	time.Sleep(20 * time.Millisecond)
	coord.recordProgress("reset-root") // resets interval to cfg.Interval (50ms) and re-arms from now, invalidating generation 1

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
	coord := newSupervisionTestCoordinator(t)
	coord.timeouts = newTimeoutService()
	defer coord.timeouts.close()

	var prompts []string
	done := make(chan struct{}, 1)
	agent := &mockSessionAgent{runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		prompts = append(prompts, call.Prompt)
		done <- struct{}{}
		return agentResultWithText("ok"), nil
	}}
	coord.subAgentDrivers.register("turnend-root", subAgentDriver{agent: agent})
	seedSupervisionOpenWork(t, coord, "turnend-root", "call-1")

	cfg := SupervisionConfig{Enabled: true, Interval: 10 * time.Millisecond, MaxInterval: 500 * time.Millisecond, MaxNoProgress: 6}
	// Simulate a session already two ticks into backoff (grown to 40ms),
	// currently scheduled far in the future.
	coord.supervision.byRoot["turnend-root"] = &supervisionState{
		rootSessionID: "turnend-root", cfg: cfg, interval: 40 * time.Millisecond, tickCount: 2, generation: 1,
	}
	coord.timeouts.armFunc(time.Now().Add(2*time.Second), func() { coord.handleSupervisionDeadline("turnend-root", 1) })

	time.Sleep(20 * time.Millisecond)
	coord.pushDeadlineOnTurnEnd("turnend-root") // simulates onSessionIdleHook firing after an unrelated turn ends

	coord.supervision.mu.Lock()
	st := coord.supervision.byRoot["turnend-root"]
	require.Equal(t, 2, st.tickCount, "pushDeadlineOnTurnEnd must not touch tickCount")
	require.Equal(t, 40*time.Millisecond, st.interval, "pushDeadlineOnTurnEnd must not touch interval size")
	coord.supervision.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the rescheduled deadline never fired")
	}
	require.Len(t, prompts, 1)
	text := latestSessionNoticeText(t, coord, "turnend-root")
	require.Contains(t, text, "tick 3", "backoff continues from where it left off (3rd tick), not reset to 1")
}

// TestSupervision_PushDeadlineOnTurnEndSkipsPausedSession: a mere turn end
// must not resume a paused session -- only recordProgress does.
//
// Revert-check performed: removed the `|| st.paused` clause from
// pushDeadlineOnTurnEnd's stale-check -- this test FAILED (generation
// changed for a paused session). Restored the clause; re-ran, passed.
func TestSupervision_PushDeadlineOnTurnEndSkipsPausedSession(t *testing.T) {
	coord := newSupervisionTestCoordinator(t)
	coord.supervision.byRoot["paused-root"] = &supervisionState{rootSessionID: "paused-root", cfg: DefaultSupervisionConfig(), paused: true, generation: 5}

	coord.pushDeadlineOnTurnEnd("paused-root")

	coord.supervision.mu.Lock()
	st := coord.supervision.byRoot["paused-root"]
	coord.supervision.mu.Unlock()
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
	coord := newSupervisionTestCoordinator(t)
	coord.supervision.nextGen = 1
	for _, id := range []string{"hook-root", "quiet-root"} {
		coord.supervision.byRoot[id] = &supervisionState{
			rootSessionID: id, cfg: DefaultSupervisionConfig(), interval: 5 * time.Millisecond, generation: 1,
		}
	}
	sa := &sessionAgent{coord: coord}

	sa.afterTurn(SessionAgentCall{SessionID: "hook-root"}, nil, nil, false)
	coord.supervision.mu.Lock()
	pushed := coord.supervision.byRoot["hook-root"].generation
	coord.supervision.mu.Unlock()
	require.NotEqual(t, uint64(1), pushed, "a turn that reached the provider must push the supervision deadline")

	noTurn := &drainAttempt{sessionID: "quiet-root", outcome: drainNoTurn}
	sa.afterTurn(newDrainCall(SessionAgentCall{SessionID: "quiet-root"}), noTurn, nil, false)
	coord.supervision.mu.Lock()
	quiet := coord.supervision.byRoot["quiet-root"].generation
	coord.supervision.mu.Unlock()
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
	coord := newSupervisionTestCoordinator(t)
	seedSupervisionOpenWork(t, coord, "backoff-root", "call-1")

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
	coord.supervision.byRoot["backoff-root"] = &supervisionState{rootSessionID: "backoff-root", cfg: cfg, interval: cfg.Interval, generation: 1}

	fireAndWait := func(gen uint64, wantCalls int) {
		t.Helper()
		coord.handleSupervisionDeadline("backoff-root", gen)
		require.Eventually(t, func() bool { return promptCount() >= wantCalls }, 2*time.Second, 5*time.Millisecond)
	}

	fireAndWait(1, 1) // tick 1: 10ms -> 20ms
	coord.supervision.mu.Lock()
	st := coord.supervision.byRoot["backoff-root"]
	require.Equal(t, 1, st.tickCount)
	require.Equal(t, 20*time.Millisecond, st.interval)
	require.False(t, st.paused)
	gen2 := st.generation
	coord.supervision.mu.Unlock()
	require.Contains(t, sessionNoticeTextAt(t, coord, "backoff-root", 0), "tick 1")

	fireAndWait(gen2, 2) // tick 2: 20ms -> 40ms (== cap)
	coord.supervision.mu.Lock()
	st = coord.supervision.byRoot["backoff-root"]
	require.Equal(t, 2, st.tickCount)
	require.Equal(t, 40*time.Millisecond, st.interval, "must cap at MaxInterval, not keep doubling")
	gen3 := st.generation
	coord.supervision.mu.Unlock()

	fireAndWait(gen3, 3) // tick 3 == MaxNoProgress: last tick, then pause
	coord.supervision.mu.Lock()
	st = coord.supervision.byRoot["backoff-root"]
	require.Equal(t, 3, st.tickCount)
	require.True(t, st.paused)
	staleGenAfterPause := st.generation
	coord.supervision.mu.Unlock()
	require.Contains(t, sessionNoticeTextAt(t, coord, "backoff-root", 2), "paused after 3 consecutive check-ins")

	// A stale fire against the paused generation must not produce a 4th call.
	coord.handleSupervisionDeadline("backoff-root", staleGenAfterPause)
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, 3, promptCount(), "a paused session must not tick again on its own")

	// recordProgress resumes it: base interval, tickCount cleared, unpaused.
	coord.recordProgress("backoff-root")
	coord.supervision.mu.Lock()
	st = coord.supervision.byRoot["backoff-root"]
	require.False(t, st.paused)
	require.Zero(t, st.tickCount)
	require.Equal(t, cfg.Interval, st.interval)
	resumedGen := st.generation
	coord.supervision.mu.Unlock()
	require.NotEqual(t, staleGenAfterPause, resumedGen)

	fireAndWait(resumedGen, 4)
	require.Contains(t, sessionNoticeTextAt(t, coord, "backoff-root", 3), "tick 1", "a resumed session's next tick starts counting from 1 again")
}

// TestSupervision_StateRemovedOnScopeClose and
// TestSupervision_CancelSessionAlsoClearsState were DELETEd as obsolete
// contracts: their premises were the deleted ledger's deliverLocked /
// cancelSession supervision-clear hooks. In the batch world the stale-state
// drop happens at the DEADLINE (handleSupervisionDeadline drops state whose
// scope is closed -- pinned by TestSupervision_NoTickWithoutOpenWork); a
// delivered delegation is unregistered by design (parkedParentSessions) and
// no coordinator-level cancelSession exists anymore.

// TestSupervision_SingleGoroutineRegardlessOfSessionCount mirrors
// TestTimeoutService_SingleGoroutineForManyJobs: arming supervision for N
// distinct root sessions must not spawn N goroutines -- the SAME shared
// timeoutService services both asyncJob deadlines and supervision deadlines.
func TestSupervision_SingleGoroutineRegardlessOfSessionCount(t *testing.T) {
	coord := newSupervisionTestCoordinator(t)
	coord.timeouts = newTimeoutService()
	defer coord.timeouts.close()

	arm := func(i int) {
		sessionID := fmt.Sprintf("root-%d", i)
		startOpenJob(t, coord, sessionID, "call-1")
		coord.noteWorkStarted(context.Background(), sessionID)
	}
	// Baseline after the fixture and one armed session (see
	// TestTimeoutService_SingleGoroutineForManyJobs).
	arm(0)
	base := goroutineBaseline()

	const n = 50
	for i := 1; i < n; i++ {
		arm(i)
	}

	delta := settledGoroutineDelta(base)
	require.LessOrEqual(t, delta, 2,
		"arming supervision for %d sessions must not spawn %d goroutines, got a delta of %d", n, n, delta)
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
