// R12-E per-consumer cutover-facts tests: with the
// node-backed BatchCutoverFacts bound to a coordinator (the wiring R12-H must
// add in production: BindCoordinatorBatchCutoverFacts(c,
// NewBatchCutoverNodeFacts(store, live))), every ported consumer reads its
// facts from batch_nodes/session_notices and NEVER from the removed legacy
// authority. COMPILE-ONLY IN THIS WORKTREE (regime: no executed mutation
// runs; CI runs it).
//
// Flag note: every ON test sets the env explicitly via t.Setenv, which
// forbids t.Parallel(); no test in this file runs in parallel.
package agent

import (
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// cutoverBindFacts is defined in batch_cutover_await_test.go beside the
// coordinator fixture (identical body); the facts tests reuse it.

// newCutoverFactsCoordinator builds the cheap coordinator fixture: a bare
// coordinator with the rehomed runtime state and NO sessions service. Facts
// come from the binding (cutoverBindFacts), never from a store.
func newCutoverFactsCoordinator(t *testing.T) *coordinator {
	t.Helper()
	return newBatchRuntimeCoordinator()
}

// ---- TestBatchCutoverSelectedDebtON: a settled root's selected debt is
// visible through the ON-path drainDecision, with zero async_jobs writes. ----

func TestBatchCutoverSelectedDebtON(t *testing.T) {
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverFactsCoordinator(t)
	cutoverBindFacts(t, coord, env)
	require.NotNil(t, coordinatorBatchCutoverFacts(coord), "flag on: the seam returns a fact source")

	// Before any settlement: no selected debt, so the drain decision reports
	// no debt and never reaches the launch verdict.
	debt, _, err := coord.drainDecision(env.ctx, env.sessionID, false)
	require.NoError(t, err)
	require.False(t, debt)

	// A detached root with a fast leaf settles through the real runner.
	env.invokeDetached("call-fact-debt", "view", fast)
	rootID := env.rootID("call-fact-debt")
	require.Equal(t, "completed", env.rootTerminal(rootID))

	// After the settle commit lands, the ON path sees the debt...
	require.Eventually(t, func() bool {
		debt, _, err := coord.drainDecision(env.ctx, env.sessionID, false)
		return err == nil && debt
	}, 5*time.Second, 5*time.Millisecond, "drainDecision ON path sees the selected debt")
	// ... via the raw facts too.
	facts := coordinatorBatchCutoverFacts(coord)
	selected, err := facts.SelectedDebtExists(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.True(t, selected)
	// ... and the removed legacy authority recorded NOTHING (its table is gone).
	require.Zero(t, env.asyncJobsCount(), "the legacy async_jobs authority is gone")
	env.logNodeRows(t, rootID)
}

// ---- TestBatchCutoverHeldQuestionsON: a pending child-question notice
// appears in the ON facts (HeldQuestions and the pending-inclusive summary). ----

func TestBatchCutoverHeldQuestionsON(t *testing.T) {
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64})
	coord := newCutoverFactsCoordinator(t)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)

	now := time.Now().Unix()
	_, err := env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindChildQuestion,
		Text: "held question?", Wake: 1, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	held, err := facts.HeldQuestions(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, held, 1, "the pending child question is a held question")
	require.Equal(t, session.NoticeKindChildQuestion, held[0].Kind)

	hasDebt, kinds, err := facts.PendingInclusiveDebtSummary(env.ctx, env.sessionID)
	require.NoError(t, err)
	// The summary's bool counts NODE debt only (the batch reaction read):
	// this fixture is notice-only, so hasDebt is FALSE while the kinds
	// still name the child question.
	require.False(t, hasDebt, "the summary bool is NODE debt; notice-only debt is false")
	require.Contains(t, kinds, session.NoticeKindChildQuestion)
	require.Zero(t, env.asyncJobsCount(), "the legacy async_jobs authority is gone")
}

// ---- TestBatchCutoverReactionFailureON: a failed node yields reacted-failed
// facts; settle-with-marker closes the debt; async_jobs stays empty. ----

func TestBatchCutoverReactionFailureON(t *testing.T) {
	failing := &errLeaf{countingTool: countingTool{name: "view"}}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, failing)
	coord := newCutoverFactsCoordinator(t)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)
	inv := env.invocation("call-fact-fail")
	inv.Delivery.Mode = batchDeliveryDetached
	_, err := env.install.Runtime.Invoke(env.ctx, inv, failing,
		fantasy.ToolCall{ID: "call-fact-fail", Name: "view", Input: `{"p":1}`})
	require.NoError(t, err)
	rootID := env.rootID("call-fact-fail")
	require.Equal(t, "failed", env.rootTerminal(rootID), "the failing root settles as failed")

	// The failure debt is visible, with no reacted-failed marker yet.
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "failed root creates selected debt")
	// VISIBLE (delivery='done') debt requires a pull: deliver the failure.
	env.requireBatchPull(1)
	texts, err := facts.ReactedFailedTexts(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Empty(t, texts, "no marker before the settle-by-failure")

	// Settle-by-failure with marker over the CAPTURED visible snapshot: the
	// debt closes and the marker lands.
	snap, err := facts.CaptureVisibleDebt(env.ctx, env.sessionID)
	require.NoError(t, err)
	n, err := facts.SettleReactedFailedWithMarker(env.ctx, env.sessionID, snap, "wake failed: probe")
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, int64(1), "at least the failed row settled")
	require.False(t, env.debtExists(), "the visible debt is closed by the failure settle")
	texts, err = facts.ReactedFailedTexts(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, texts, 1, "the node row's own outcome summary is the reacted-failed text")
	// NotifyAll selects the STRUCTURAL ROOT (jobbatch notice.go), and the
	// root settle keeps its OWN OutcomeSummary -- it never copies the
	// failed child's "boom" (tree_apply.go). The marker's production
	// carrier is the wake_failed session_notices row asserted below.
	require.Empty(t, texts[0].Text, "the NotifyAll root's own outcome summary (never the child's)")
	// The wake_failed marker itself landed as a session_notices row.
	var markers int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT count(*) FROM session_notices WHERE owner = ?1 AND kind = 'wake_failed' AND text = 'wake failed: probe'`,
		env.sessionID).Scan(&markers))
	require.EqualValues(t, 1, markers, "the wake_failed marker landed in session_notices")
	require.Zero(t, env.asyncJobsCount(), "the legacy async_jobs authority is gone")
	env.logNodeRows(t, rootID)
}

// ---- TestBatchCutoverWakeBreakersON: wake-attempt accounting over real
// settled debt rows. ----

func TestBatchCutoverWakeBreakersON(t *testing.T) {
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverFactsCoordinator(t)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)

	env.invokeDetached("call-fact-wake", "view", fast)
	rootID := env.rootID("call-fact-wake")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "debt exists before accounting")
	// The visible (done) snapshot needs the delivery pulled first.
	env.requireBatchPull(1)
	snap, err := facts.CaptureVisibleDebt(env.ctx, env.sessionID)
	require.NoError(t, err)

	// Before the increment: the snapshot rows have no attempts (count==0
	// rows are skipped, so Open==0) while the pending-inclusive predicate
	// already reports debt.
	rows, err := facts.AttemptRows(env.ctx, env.sessionID, snap, 1)
	require.NoError(t, err)
	require.Equal(t, 0, rows.Open, "no attempts counted yet: count==0 rows are skipped")
	require.Equal(t, 0, rows.Max)
	require.True(t, env.debtExists(), "pending-inclusive debt is true while attempts are zero")

	require.NoError(t, facts.IncrementWakeAttempts(env.ctx, env.sessionID, snap))
	rows, err = facts.AttemptRows(env.ctx, env.sessionID, snap, 1)
	require.NoError(t, err)
	t.Logf("AW-wake rows open=%d max=%d atK=%d", rows.Open, rows.Max, len(rows.AtK.Nodes)+len(rows.AtK.Notices))
	require.GreaterOrEqual(t, rows.Open, 1, "the snapshot rows are open after the increment")
	require.GreaterOrEqual(t, rows.Max, 1, "the increment landed on the real row")

	// Settling the CAPTURED rows closes every accounted row.
	_, err = facts.SettleReactedFailedWithMarker(env.ctx, env.sessionID, snap, "wake failed: accounting")
	require.NoError(t, err)
	rows, err = facts.AttemptRows(env.ctx, env.sessionID, snap, 1)
	require.NoError(t, err)
	require.Zero(t, rows.Open, "the settle closed the accounted rows")
	env.logNodeRows(t, rootID)
}

// ---- TestBatchCutoverScopeON: live work true while the held executor runs,
// false after settle; own vs descendant halves distinct. ----

func TestBatchCutoverScopeON(t *testing.T) {
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)
	coord := newCutoverFactsCoordinator(t)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)

	env.invokeDetached("call-fact-scope", "view", leaf)
	leaf.waitStarted(t, "call-fact-scope")
	rootID := env.rootID("call-fact-scope")

	own, descendants, err := facts.LiveWorkSummary(env.ctx, env.sessionID)
	require.NoError(t, err)
	t.Logf("AW-scope held: own=%d descendants=%d", own, descendants)
	require.Equal(t, 1, own, "the held root is one live own root")
	require.Equal(t, 0, descendants, "no delegation descendants")
	scope, err := facts.LiveScopeForOwners(env.ctx, []string{env.sessionID})
	require.NoError(t, err)
	require.Len(t, scope, 1)
	require.True(t, scope[0].Live, "the CLI scope sees the running work")
	require.True(t, coord.hasLiveTreeWork(env.sessionID), "the rate-limit budget pauses on live work")

	leaf.letGo("call-fact-scope")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool {
		own, descendants, err := facts.LiveWorkSummary(env.ctx, env.sessionID)
		return err == nil && own == 0 && descendants == 0
	}, 5*time.Second, 5*time.Millisecond, "the scope is empty after the settle")
	require.False(t, coord.hasLiveTreeWork(env.sessionID), "no live work after the settle")
	require.Zero(t, env.asyncJobsCount(), "the legacy async_jobs authority is gone")
	env.logNodeRows(t, rootID)
}

// ---- TestBatchCutoverUnknownFailSafeON: flag ON without a binding returns
// the conservative unknown source: debt true, live true, no legacy reads. ---

func TestBatchCutoverUnknownFailSafeON(t *testing.T) {
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64})
	coord := newCutoverFactsCoordinator(t)
	// Deliberately NO binding registered.
	facts := coordinatorBatchCutoverFacts(coord)
	require.NotNil(t, facts)

	selected, err := facts.SelectedDebtExists(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.True(t, selected, "unknown debt reads as debt, never as no debt")
	own, descendants, err := facts.LiveWorkSummary(env.ctx, env.sessionID)
	require.Error(t, err, "the unknown source reports the missing node source")
	require.Equal(t, 1, own)
	require.Equal(t, 1, descendants)
	// F3: hasLiveTreeWork is the rate-limit budget's pause signal; a failed
	// read answers not-live THERE so the budget can exhaust (the loop must
	// never retry a 429 forever on an unreadable source). The conservative
	// "unknown = live" rule belongs to the durable consumers, which do their
	// own reads.
	require.False(t, coord.hasLiveTreeWork(env.sessionID),
		"a failed liveness read must not pause the rate-limit budget forever")
	// The unknown source never touches any legacy authority.
	require.Zero(t, env.asyncJobsCount(), "the legacy async_jobs authority is gone")
}

// ---- TestBatchCutoverAdmissionHeldTerminationON: under the flag, a held
// termination keeps admission until the real settle frees it. ----

func TestBatchCutoverAdmissionHeldTerminationON(t *testing.T) {
	viewLeaf := newGatedLeaf("view")
	argvLeaf := newGatedLeaf("run_command")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 64}, viewLeaf, argvLeaf)
	releaseAllAt(t, viewLeaf, argvLeaf)
	coord := newCutoverFactsCoordinator(t)
	cutoverBindFacts(t, coord, env)

	env.invokeDetached("call-fact-adm-r", "view", viewLeaf)
	viewLeaf.waitStarted(t, "call-fact-adm-r")
	rootR := env.rootID("call-fact-adm-r")

	env.invokeDetached("call-fact-adm-b", "run_command", argvLeaf)
	rootB := env.rootID("call-fact-adm-b")

	out := cancelBatchCall(env.ctx, env.install.Runtime.cfg.Runner, jobbatch.ID(rootR))
	require.NoError(t, out.Err)
	require.True(t, out.Submitted)
	require.False(t, out.Settled, "Stop never settles: admission stays held")

	require.True(t, cutoverStateLive(env.nodeState(rootR)))
	require.Equal(t, "pending", env.nodeState(rootB), "admission held through the Stop")

	viewLeaf.letGo("call-fact-adm-r")
	require.Equal(t, "cancelled", env.rootTerminal(rootR),
		"the latched stop cause settles R cancelled")
	// B is gated: release it so its settle can complete.
	argvLeaf.letGo("call-fact-adm-b")
	require.Eventually(t, func() bool { return env.nodeState(rootB) == "completed" },
		5*time.Second, 5*time.Millisecond, "a new batch admitted only after the settle")
	require.Zero(t, env.asyncJobsCount(), "the legacy async_jobs authority is gone")
	env.logNodeRows(t, rootR, rootB)
}
