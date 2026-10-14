// R12-E AW-b acceptance test (docs/plans/2026-10-07-job-batches-design.md
// rev.4 ~1869-1873): stop admission, rule 4b, inline election and once-only
// delivery, driven through the REAL batch runtime. COMPILE-ONLY IN THIS
// WORKTREE.
//
// Production oracles used here: Runner.Stop returns on ACCEPTANCE and the
// executor Stop's result lands as the child's control_state ('acked' =
// the execution's Stop returned without error, termination not yet
// confirmed); the engine latches the stop cause so a stopped root settles
// CANCELLED whenever its leaf is released; an UNANNOUNCED one-task root's
// synchronous inline election WINS (BatchOutboxInlineElect requires
// announced=0, one task child: delivery=done, wake=0, reacted=1) while an
// announced root's completions deliver only through BatchOutboxPull, each
// selected notice exactly once; debt stays debt while unreacted regardless
// of delivery ("held" = unreacted).
package agent

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestAWbStopAdmissionFourBAndDelivery(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	gatedView := newGatedLeaf("view")
	gatedRun := newGatedLeaf("run_command")
	fast := &countingTool{name: "grep"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 64},
		gatedView, gatedRun, fast)
	releaseAllAt(t, gatedView, gatedRun)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// 1. D: a detached (announced) root settles; its delivery stays HELD
	// (pending, wake=1, unreacted) -- nothing pulls it in this phase.
	env.invokeDetached("call-b-d", "grep", fast)
	rootD := env.rootID("call-b-d")
	require.Equal(t, "completed", env.rootTerminal(rootD))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "D debt committed")
	dDelivery, dWake, dReacted, _ := env.nodeNotice(rootD)
	require.Equal(t, "pending", dDelivery, "D's delivery stays held: nothing pulled it")
	require.EqualValues(t, 1, dWake)
	require.EqualValues(t, 0, dReacted)
	require.True(t, env.debtExists(), "D is the held debt")
	t.Logf("AW-b1 D root=%s delivery=%s", rootD, dDelivery)

	// 2. R: a ONE-ITEM root submitted WITHOUT the started announcement
	// (announced=0: its synchronous inline election may win later); its
	// gated leaf starts and holds.
	rootR := env.submitBatchRootUnannounced("call-b-r", 1, gatedView)
	rCall := leafItemCalls("call-b-r", 1)[0]
	gatedView.waitStarted(t, rCall)

	// The ALL worker's ENTRY capture happens NOW: {R} only (B does not
	// exist yet).
	captured := env.capture()
	require.Len(t, captured.Refs(), 1, "only R is in the worker's captured set")
	resCh := env.awaitAsync(batchAwaitModeAll, captured, true, 10*time.Second)

	// 3. Stop ACCEPTANCE: accepted, submitted, NEVER settles; R stays live.
	out := cancelBatchCall(env.ctx, env.install.RunnerHandle, jobbatch.ID(rootR))
	require.NoError(t, out.Err, "Stop accepted")
	require.True(t, out.Submitted)
	require.False(t, out.Settled, "a Stop acknowledgement never settles")
	require.True(t, cutoverStateLive(env.nodeState(rootR)), "R stays live after Stop")
	t.Logf("AW-b3 stop R=%s state=%s", rootR, env.nodeState(rootR))

	// 4. B is submitted BEFORE the executor Stop returns: admission is NOT
	// freed by the stop intent; B's root row stays Pending while R holds
	// the only slot.
	env.invokeDetached("call-b-b", "run_command", gatedRun)
	rootB := env.rootID("call-b-b")
	require.Eventually(t, func() bool { return env.nodeState(rootB) == "pending" },
		2*time.Second, 5*time.Millisecond, "B queued pending behind R")
	require.True(t, cutoverStateLive(env.nodeState(rootR)), "R still live")
	t.Logf("AW-b4 B root=%s pending while R live", rootB)

	// 5. The executor Stop RETURN fence: 'acked' lands while R is
	// non-terminal and B is still Pending -- acceptance AND a successful
	// Stop return leave R live and B Pending.
	require.Eventually(t, func() bool {
		return env.nodeControl(env.childNodeID(rootR, 1)) == "acked"
	}, 5*time.Second, 5*time.Millisecond, "executor Stop returned (acked)")
	require.NotEqual(t, "cancelled", env.nodeState(rootR),
		"the acked Stop return leaves R non-terminal")
	require.Equal(t, "pending", env.nodeState(rootB), "B still Pending at the Stop-return fence")
	t.Logf("AW-b5 acked R=%s B=%s", env.nodeState(rootR), env.nodeState(rootB))

	// 6. Rule 4b: with D's completion debt pending-inclusive, the armed
	// sleep defers on the live work (R and B both nonterminal).
	armSleepAll(t, coord, env)
	facts := readFactsON(t, coord, env)
	require.True(t, facts.Debt.PendingIncl, "phase 6: D's debt is pending")
	require.True(t, facts.Debt.CompletionOnly, "phase 6: D is completion-class")
	require.GreaterOrEqual(t, facts.Session.RunningOwnJobs, 1, "phase 6: R (and B) live")
	pv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, "sleeping until all tasks finish", pv.reason, "phase 6: 4b defers")
	require.Equal(t, "pending", env.nodeState(rootB), "B still Pending at the 4b fence")
	t.Logf("AW-b6 facts runningOwn=%d reason=%q", facts.Session.RunningOwnJobs, pv.reason)

	// 7. Release R's held leaf: the latched stop cause settles R CANCELLED;
	// the ALL worker (captured {R} only) wakes on R; B was never captured.
	gatedView.letGo(rCall)
	require.Equal(t, "cancelled", env.rootTerminal(rootR),
		"the latched stop cause settles R cancelled")
	select {
	case wout := <-resCh:
		require.False(t, wout.TimedOut, "the worker wakes on R's settlement")
		require.Equal(t, 1, wout.FinishedCount)
		require.Equal(t, rootR, wout.Finished[0].Ref.RootID)
	case <-time.After(5 * time.Second):
		t.Fatal("ALL worker never woke on R")
	}
	// D's held delivery and debt are untouched.
	dDelivery, _, _, _ = env.nodeNotice(rootD)
	require.Equal(t, "pending", dDelivery, "D delivery untouched by R's settle")
	require.True(t, env.debtExists(), "D's debt survives")

	// 8. R's synchronous inline election WINS: the root was never
	// announced and has exactly one task child, so BatchOutboxInlineElect
	// fires (delivery=done, wake=0, reacted=1) -- R's debt rows are
	// CONSUMED. D remains the pending-inclusive debt.
	notices, err := env.store.BatchAnnounceInlineResult(env.ctx, env.messages, env.sessionID, rootR, message.CreateMessageParams{
		Role: message.User, BackgroundJobNotice: true,
		Parts: []message.ContentPart{message.TextContent{Text: "batch R result"}},
	})
	require.NoError(t, err, "the unannounced one-task root inline-elects")
	require.EqualValues(t, 1, notices.DeliveryRows)
	rDelivery, rWake, rReacted, _ := env.nodeNotice(rootR)
	require.Equal(t, "done", rDelivery, "the inline election consumed R's delivery")
	require.EqualValues(t, 0, rWake)
	require.EqualValues(t, 1, rReacted, "R's notice is reacted: its debt rows are gone")
	require.True(t, env.debtExists(), "D still pending/unreacted: the debt remains")

	// 9. 4b STILL defers: B is live.
	require.True(t, cutoverStateLive(env.nodeState(rootB)), "B admitted after R's settle")
	facts = readFactsON(t, coord, env)
	require.GreaterOrEqual(t, facts.Session.RunningOwnJobs, 1, "phase 9: B live")
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, "sleeping until all tasks finish", pv.reason, "phase 9: 4b still defers on B")

	// 10. B completes: no live work left -> 4b lifts; the launch proceeds
	// subject to the other gates (the fresh gate is open: allow).
	gatedRun.letGo("call-b-b")
	require.Equal(t, "completed", env.rootTerminal(rootB))
	require.Eventually(t, func() bool {
		f, err := coord.readTurnFacts(env.ctx, env.sessionID, siteCLI)
		return err == nil && f.Session.RunningOwnJobs == 0
	}, 5*time.Second, 5*time.Millisecond, "phase 10: no live work")
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, drainAllow, pv.kind,
		"phase 10: 4b lifted, exactly one eligible launch subject to other gates")

	// 11. NOW the pull delivers the held completions: D's held notice and
	// B's announced completion, each exactly once (delta-accounted: the
	// started announcements of D and B are already in the before-count).
	before := env.backgroundNoticeCount()
	env.requireBatchPull(1)
	require.EqualValues(t, 2, env.backgroundNoticeCount()-before,
		"exactly D's and B's completions delivered by the pull")
	require.Equal(t, 0, env.pullBatchNotices(), "the second pull is empty: consumed once")
	dDelivery, _, _, _ = env.nodeNotice(rootD)
	require.Equal(t, "done", dDelivery)
	bDelivery, _, _, _ := env.nodeNotice(rootB)
	require.Equal(t, "done", bDelivery, "B's announced completion delivered by the same pull")
	require.True(t, env.debtExists(), "unreacted debt remains (D and B)")
	require.Zero(t, env.asyncJobsCount())
	env.logDebtRows(t)
	env.logNodeRows(t, rootD, rootR, rootB)
}

// TestAWbDebtFreeVariant is AW-b WITHOUT D: stop intent alone creates no
// debt (EARLY fence: rule 1, no launch, no 4b verdict); the sleep is armed
// through the production ALL path while R is LIVE (the coordinator rejects
// an empty live set); R settles CANCELLED, B is admitted and completes, and
// the FINAL fence is the unannounced one-task root's inline election WIN
// plus B's once-only pulled delivery -- explicitly NOT debt-free.
func TestAWbDebtFreeVariant(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	gatedView := newGatedLeaf("view")
	gatedRun := newGatedLeaf("run_command")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 64},
		gatedView, gatedRun)
	releaseAllAt(t, gatedView, gatedRun)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// R: an UNANNOUNCED one-item gated root, then Stop: accepted, not
	// settled, R stays live, the executor's Stop returns (acked) while R is
	// non-terminal.
	rootR := env.submitBatchRootUnannounced("call-bf-r", 1, gatedView)
	rCall := leafItemCalls("call-bf-r", 1)[0]
	gatedView.waitStarted(t, rCall)
	out := cancelBatchCall(env.ctx, env.install.RunnerHandle, jobbatch.ID(rootR))
	require.NoError(t, out.Err)
	require.True(t, out.Submitted)
	require.False(t, out.Settled)
	require.True(t, cutoverStateLive(env.nodeState(rootR)))
	require.Eventually(t, func() bool {
		return env.nodeControl(env.childNodeID(rootR, 1)) == "acked"
	}, 5*time.Second, 5*time.Millisecond, "executor Stop returned (acked)")

	// EARLY fence: with no completion debt anywhere, rule 1 answers: no
	// launch, no 4b verdict -- a stop intent is never debt evidence.
	require.False(t, env.debtExists(), "stop intent created no debt")
	debt, dv, err := coord.drainDecision(env.ctx, env.sessionID, false)
	require.NoError(t, err)
	require.False(t, debt, "EARLY: no pending-inclusive debt, rule 1, nothing to launch")
	t.Logf("AW-b-free EARLY debt=%v verdict=%v", debt, dv)

	// Arm the sleep through the PRODUCTION ALL path while R is LIVE, right
	// after the acked fence and before B completes (the coordinator rejects
	// an empty live set at coordinator_await_tasks.go).
	armSleepAll(t, coord, env)
	require.True(t, readFactsON(t, coord, env).Session.SleepAll, "the armed sleep")

	// B queued pending behind R (admission not freed by the stop intent).
	env.invokeDetached("call-bf-b", "run_command", gatedRun)
	rootB := env.rootID("call-bf-b")
	require.Eventually(t, func() bool { return env.nodeState(rootB) == "pending" },
		2*time.Second, 5*time.Millisecond, "B queued pending behind R")

	// Release R: the latched stop cause settles R cancelled; B is admitted
	// and runs.
	gatedView.letGo(rCall)
	require.Equal(t, "cancelled", env.rootTerminal(rootR))
	require.Eventually(t, func() bool {
		st := env.nodeState(rootB)
		return st == "running" || st == "starting" || st == "completed"
	}, 5*time.Second, 5*time.Millisecond, "B admitted once R settled")
	gatedRun.letGo("call-bf-b")
	require.Equal(t, "completed", env.rootTerminal(rootB))

	// FINAL fence: B's settlement commits durable completion debt; with R/B
	// settled, 4b no longer defers: exactly one eligible launch (the fresh
	// gate is open). This final state is explicitly NOT debt-free.
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "FINAL: B's settlement committed debt")
	require.Eventually(t, func() bool {
		f, err := coord.readTurnFacts(env.ctx, env.sessionID, siteCLI)
		return err == nil && f.Session.RunningOwnJobs == 0
	}, 5*time.Second, 5*time.Millisecond, "FINAL: no live work")
	pv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, drainAllow, pv.kind,
		"FINAL: 4b lifted, one eligible launch (subject to other gates)")

	// R's synchronous inline election WINS here too (unannounced one-task
	// root): delivery=done, wake=0, reacted=1.
	notices, err := env.store.BatchAnnounceInlineResult(env.ctx, env.messages, env.sessionID, rootR, message.CreateMessageParams{
		Role: message.User, BackgroundJobNotice: true,
		Parts: []message.ContentPart{message.TextContent{Text: "batch R result"}},
	})
	require.NoError(t, err, "the unannounced one-task root inline-elects")
	require.EqualValues(t, 1, notices.DeliveryRows)
	rDelivery, rWake, rReacted, _ := env.nodeNotice(rootR)
	require.Equal(t, "done", rDelivery)
	require.EqualValues(t, 0, rWake)
	require.EqualValues(t, 1, rReacted, "R's debt rows consumed by the inline election")

	// B's announced completion stays pending until pulled: one pull
	// delivers it, the second returns 0; the debt remains unreacted.
	bDelivery, _, _, _ := env.nodeNotice(rootB)
	require.Equal(t, "pending", bDelivery, "B's completion stays held before the pull")
	env.requireBatchPull(1)
	require.Equal(t, 0, env.pullBatchNotices(), "nothing left pending")
	bDelivery, _, _, _ = env.nodeNotice(rootB)
	require.Equal(t, "done", bDelivery)
	require.True(t, env.debtExists(), "unreacted completion debt remains")
	t.Logf("AW-b-free FINAL kind=%v reason=%q", pv.kind, pv.reason)
	env.logDebtRows(t)
	env.logNodeRows(t, rootR, rootB)
}
