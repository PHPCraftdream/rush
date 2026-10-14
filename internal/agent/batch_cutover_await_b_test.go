// R12-E AW-b acceptance test (docs/plans/2026-10-07-job-batches-design.md
// rev.4 ~1870-1908): stop admission, rule 4b, and exactly-once delivery,
// driven through the REAL batch runtime. COMPILE-ONLY IN THIS WORKTREE.
//
// AW-b combined sequence (max_concurrent 1): D = settled independent
// completion debt held pending; R = gated root stopped (held termination);
// B = queued behind R and admitted only at R's confirmed settlement. The
// debt-free variant drops D and splits the fences EARLY/FINAL.
package agent

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAWbStopAdmissionFourBAndDelivery(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	gatedView := newGatedLeaf("view")
	gatedRun := newGatedLeaf("run_command")
	fast := &countingTool{name: "fast"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 64},
		gatedView, gatedRun, fast)
	releaseAllAt(t, gatedView, gatedRun)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// 1. D: a detached fast root settles; its delivery is NOT pulled (held).
	env.invokeDetached("call-b-d", "fast", fast)
	rootD := env.rootID("call-b-d")
	require.Equal(t, "completed", env.rootTerminal(rootD))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "D debt committed")
	delivery, _, _, _ := env.nodeNotice(rootD)
	require.Equal(t, "pending", delivery, "D delivery held")
	t.Logf("AW-b1 D root=%s settled, delivery pending", rootD)

	// 2. R: a gated root runs (held termination).
	env.invokeDetached("call-b-r", "view", gatedView)
	gatedView.waitStarted(t, "call-b-r")
	rootR := env.rootID("call-b-r")

	// 3. Stop: accepted, submitted, NEVER settles; R stays live.
	out := cancelBatchCall(env.ctx, env.install.RunnerHandle, jobbatch.ID(rootR))
	require.NoError(t, out.Err, "Stop accepted")
	require.True(t, out.Submitted)
	require.False(t, out.Settled, "a Stop acknowledgement never settles")
	require.True(t, cutoverStateLive(env.nodeState(rootR)), "R stays live after Stop")
	t.Logf("AW-b3 stop R=%s state=%s", rootR, env.nodeState(rootR))

	// The ALL worker's ENTRY capture happens NOW: {R} only (B does not
	// exist yet).
	captured := env.capture()
	require.Len(t, captured.Refs(), 1, "only R is in the worker's captured set")
	resCh := env.awaitAsync(batchAwaitModeAll, captured, true, 10*time.Second)

	// 4. B is submitted AFTER the stop: admission is NOT freed by the stop
	// intent; B's root row stays Pending while R holds the only slot.
	env.invokeDetached("call-b-b", "run_command", gatedRun)
	rootB := env.rootID("call-b-b")
	require.Eventually(t, func() bool { return env.nodeState(rootB) == "pending" },
		2*time.Second, 5*time.Millisecond, "B queued pending behind R")
	require.True(t, cutoverStateLive(env.nodeState(rootR)), "R still live")
	t.Logf("AW-b4 B root=%s pending while R live", rootB)

	// 5. Rule 4b: with D's completion debt pending, the armed sleep defers
	// on the live work (R and B both nonterminal).
	armSleepAll(t, coord, env.sessionID)
	facts := readFactsON(t, coord, env)
	require.True(t, facts.Debt.PendingIncl, "phase 5: D's debt is pending")
	require.True(t, facts.Debt.CompletionOnly, "phase 5: D is completion-class")
	require.GreaterOrEqual(t, facts.Session.RunningOwnJobs, 1, "phase 5: R (and B) live")
	pv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, "sleeping until all tasks finish", pv.reason, "phase 5: 4b defers")
	require.Equal(t, "pending", env.nodeState(rootB), "B still Pending at the 4b fence")
	t.Logf("AW-b5 facts runningOwn=%d reason=%q", facts.Session.RunningOwnJobs, pv.reason)

	// 6. Release R's held leaf: R settles through the real engine (the stop
	// cause is latched; the terminal state is cancelled when the engine
	// records the stop, completed when the release wins the race). The ALL
	// worker (captured {R} only) wakes on R; B was never captured.
	gatedView.letGo("call-b-r")
	stateR := env.rootTerminal(rootR)
	t.Logf("AW-b6 R terminal state=%s", stateR)
	require.Contains(t, []string{"completed", "cancelled"}, stateR,
		"R settles through the real engine after the held release")
	select {
	case wout := <-resCh:
		require.False(t, wout.TimedOut, "the worker wakes on R's settlement")
		require.Equal(t, 1, wout.FinishedCount)
		require.Equal(t, rootR, wout.Finished[0].Ref.RootID)
	case <-time.After(5 * time.Second):
		t.Fatal("ALL worker never woke on R")
	}
	// D's debt and delivery are untouched.
	dDelivery, _, _, _ := env.nodeNotice(rootD)
	require.Equal(t, "pending", dDelivery, "D delivery untouched by R's settle")

	// 7. R's elected completion is consumed EXACTLY once (an inline answer
	// delivered after confirmed settlement), while D stays held.
	params := message.CreateMessageParams{
		Role: message.User, BackgroundJobNotice: true,
		Parts: []message.ContentPart{message.TextContent{Text: "batch R result"}},
	}
	a, err := env.store.BatchAnnounceInlineResult(env.ctx, env.messages, env.sessionID, rootR, params)
	require.NoError(t, err)
	require.EqualValues(t, 1, a.DeliveryRows, "the first inline election wins")
	_, err = env.store.BatchAnnounceInlineResult(env.ctx, env.messages, env.sessionID, rootR, params)
	require.ErrorIs(t, err, session.ErrBatchAnnouncementLost, "the second election must lose")
	require.EqualValues(t, 1, env.backgroundNoticeCount(),
		"exactly one delivered notice (R); D is still pending")
	rDelivery, _, _, _ := env.nodeNotice(rootR)
	require.Equal(t, "done", rDelivery, "R's root notice consumed by the election")

	// 8. 4b STILL waits: B is live.
	require.Equal(t, "pending", env.nodeState(rootB), "B admits only at R's confirmed settle")
	facts = readFactsON(t, coord, env)
	require.GreaterOrEqual(t, facts.Session.RunningOwnJobs, 1, "phase 8: B live")
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, "sleeping until all tasks finish", pv.reason, "phase 8: 4b still defers on B")

	// 9. B completes: no live work left -> 4b lifts; the launch proceeds
	// subject to the other gates.
	gatedRun.letGo("call-b-b")
	require.Equal(t, "completed", env.rootTerminal(rootB))
	require.Eventually(t, func() bool {
		f, err := coord.readTurnFacts(env.ctx, env.sessionID, siteCLI)
		return err == nil && f.Session.RunningOwnJobs == 0
	}, 5*time.Second, 5*time.Millisecond, "phase 9: no live work")
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.NotEqual(t, "sleeping until all tasks finish", pv.reason,
		"phase 9: 4b lifted, exactly one eligible launch subject to other gates")

	// 10. Release D: the production pull delivers it; exactly TWO notices
	// exist in history (R's election + D).
	env.requireBatchPull(1)
	require.EqualValues(t, 2, env.backgroundNoticeCount(), "R election + D delivery")
	dDelivery, _, _, _ = env.nodeNotice(rootD)
	require.Equal(t, "done", dDelivery, "D delivered")
	require.Zero(t, env.asyncJobsCount())
	env.logDebtRows(t)
	env.logNodeRows(t, rootD, rootR, rootB)
}

// TestAWbDebtFreeVariant is AW-b WITHOUT D: stop intent alone creates no
// debt (EARLY fence: rule 1, no launch, no 4b verdict); B's own settlement
// commits durable completion debt (FINAL fence: 4b lifts).
func TestAWbDebtFreeVariant(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	gatedView := newGatedLeaf("view")
	gatedRun := newGatedLeaf("run_command")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 64},
		gatedView, gatedRun)
	releaseAllAt(t, gatedView, gatedRun)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// R: gated root, then Stop: accepted, not settled, R stays live.
	env.invokeDetached("call-bf-r", "view", gatedView)
	gatedView.waitStarted(t, "call-bf-r")
	rootR := env.rootID("call-bf-r")
	out := cancelBatchCall(env.ctx, env.install.RunnerHandle, jobbatch.ID(rootR))
	require.NoError(t, out.Err)
	require.True(t, out.Submitted)
	require.False(t, out.Settled)
	require.True(t, cutoverStateLive(env.nodeState(rootR)))

	// EARLY fence: with no completion debt anywhere, rule 1 answers: no
	// launch, no 4b verdict -- a stop intent is never debt evidence.
	require.False(t, env.debtExists(), "stop intent created no debt")
	debt, dv, err := coord.drainDecision(env.ctx, env.sessionID, false)
	require.NoError(t, err)
	require.False(t, debt, "EARLY: no pending-inclusive debt, rule 1, nothing to launch")
	t.Logf("AW-b-free EARLY debt=%v verdict=%v", debt, dv)

	// B queued pending behind R (admission not freed by the stop intent).
	env.invokeDetached("call-bf-b", "run_command", gatedRun)
	rootB := env.rootID("call-bf-b")
	require.Eventually(t, func() bool { return env.nodeState(rootB) == "pending" },
		2*time.Second, 5*time.Millisecond, "B queued pending behind R")

	// Release R: R settles; B is admitted and runs.
	gatedView.letGo("call-bf-r")
	_ = env.rootTerminal(rootR)
	require.Eventually(t, func() bool {
		st := env.nodeState(rootB)
		return st == "running" || st == "starting" || st == "completed"
	}, 5*time.Second, 5*time.Millisecond, "B admitted once R settled")
	gatedRun.letGo("call-bf-b")
	require.Equal(t, "completed", env.rootTerminal(rootB))

	// FINAL fence: B's settlement commits durable completion debt; with
	// R/B settled, 4b no longer defers -- the final evaluation launches.
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "FINAL: B's settlement committed debt")
	armSleepAll(t, coord, env.sessionID)
	facts := readFactsON(t, coord, env)
	require.True(t, facts.Debt.PendingIncl, "FINAL: durable completion debt")
	require.True(t, facts.Debt.CompletionOnly, "FINAL: completion-class")
	require.Equal(t, 0, facts.Session.RunningOwnJobs, "FINAL: no live work")
	pv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.NotEqual(t, "sleeping until all tasks finish", pv.reason,
		"FINAL: 4b no longer defers; one eligible launch")
	t.Logf("AW-b-free FINAL reason=%q", pv.reason)
	env.logDebtRows(t)
	env.logNodeRows(t, rootR, rootB)
}
