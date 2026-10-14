// R12-E per-finding consumer tests: the ON-path fixes (no legacy-table
// reads, delegation identity composition, chain guard, supervision,
// notice pull, exact-row settle accounting). COMPILE-ONLY IN THIS WORKTREE.
package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// ---- Fix 3 proof: with async_jobs DROPPED, every ON consumer answers from
// batch reads alone; any legacy read would fail with "no such table". ----

func TestBatchCutoverNoLegacyTableON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	leaf := newGatedLeaf("view")
	fast := &countingTool{name: "grep"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf, fast)
	releaseAllAt(t, leaf)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)

	// No table references async_jobs (no FK in any migration), so a single
	// DROP removes the legacy ledger wholesale.
	_, err := env.conn.ExecContext(env.ctx, `DROP TABLE async_jobs`)
	require.NoError(t, err)
	// ALSO point the coordinator's legacy store at the SAME (now headless)
	// conn: any accidental legacy read hits the dropped table. No Close
	// cleanup: the conn is shared with the env.
	coord.asyncJobs.store = session.NewAsyncJobStore(env.conn, env.dataDir, os.Getpid(), "host-nolegacy")

	// Before any debt: the ON debt answer never touches the old table.
	debt, err := coord.ReactionDebtExists(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.False(t, debt)
	ddebt, dv, err := coord.drainDecision(env.ctx, env.sessionID, false)
	require.NoError(t, err)
	require.False(t, ddebt)
	t.Logf("NoLegacy: empty decision=%v verdict=%v", ddebt, dv)

	// A real detached root settles: the ON path sees the debt ...
	env.invokeDetached("call-nolegacy", "grep", fast)
	rootID := env.rootID("call-nolegacy")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "root debt from batch reads")
	debt, err = coord.ReactionDebtExists(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.True(t, debt)
	ddebt, _, err = coord.drainDecision(env.ctx, env.sessionID, false)
	require.NoError(t, err)
	require.True(t, ddebt)
	// ... and the ON readTurnFacts succeeds (no "no such table").
	_ = readFactsON(t, coord, env)

	// The new notice pulls work without the legacy void queries.
	pulled, err := facts.PullBatchNotices(env.ctx, env.messages, env.sessionID)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	pulled, err = facts.PullPendingSessionNotices(env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Empty(t, pulled)
	held, err := facts.HeldQuestions(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Empty(t, held)

	// The held leaf stays live; release before cleanup.
	leaf.letGoAll()
	env.logDebtRows(t)
	env.logNodeRows(t, rootID)
}

// ---- Fix 2 proof: the node-backed arbiter produces a verdict -- the
// parent's live scan answers RunningDelegation; the historical node read
// preserves the released-child identity (rule 7) after completion. ----

func TestBatchCutoverDelegationFactsON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64})
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// Sessions P (parent) and C (delegation child of P).
	parent := "sess-p-" + t.Name()
	child := "sess-c-" + t.Name()
	_, err := env.q.CreateSession(env.ctx, db.CreateSessionParams{ID: parent, Title: parent})
	require.NoError(t, err)
	_, err = env.q.CreateSession(env.ctx, db.CreateSessionParams{
		ID: child, ParentSessionID: nullString(parent), Title: child,
	})
	require.NoError(t, err)

	// Reserve a P-owned tree whose child node names C (running state via
	// projection): CHECKs hold -- root kind batch/mode, child kind task
	// with task_kind, tool_call_id/input_hash NULL, parent_id set.
	res, err := env.store.Reserve(env.ctx, session.BatchReservation{
		Owner: parent, Host: "host-del", RunnerID: "runner-del",
		ToolCallID: "call-del", InputHash: "hash-del", Sequence: 1,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "running", ControlState: "none"},
			Children: []session.BatchReservationNode{
				{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: nullString("agent"), State: "pending", ControlState: "none", ChildSessionID: nullString(child)}},
			},
		},
	})
	require.NoError(t, err)
	childNode := res.Snapshot.Nodes[1].NodeID
	require.NoError(t, env.store.Project(env.ctx, session.BatchProjection{
		Host: "host-del", RunnerID: "runner-del", Sequence: 2,
		Roots: []session.BatchRootProjection{{
			Owner: parent, Root: res.Snapshot.Root.RootID,
			Updates: []db.ProjectBatchNodeParams{{NodeID: childNode, State: "running", ControlState: "none"}},
		}},
	}))

	// While the delegation runs: RunningDelegation true (the parent's live
	// scan names C) and DurableChild true (rule 7 identity).
	f, err := coord.readTurnFacts(env.ctx, child, siteCLI)
	require.NoError(t, err)
	require.True(t, f.Session.RunningDelegation, "a live node under P names C")
	require.True(t, f.Session.DurableChild, "C is durably a delegation child")

	// Project the child terminal: the live half drops, the historical half
	// keeps the identity -- the released-child rule 7 still applies.
	require.NoError(t, env.store.Project(env.ctx, session.BatchProjection{
		Host: "host-del", RunnerID: "runner-del", Sequence: 3,
		Roots: []session.BatchRootProjection{{
			Owner: parent, Root: res.Snapshot.Root.RootID,
			Updates: []db.ProjectBatchNodeParams{{NodeID: childNode, State: "completed", ControlState: "none"}},
		}},
	}))
	f, err = coord.readTurnFacts(env.ctx, child, siteCLI)
	require.NoError(t, err)
	require.False(t, f.Session.RunningDelegation, "no live node names C anymore")
	require.True(t, f.Session.DurableChild, "the historical identity outlives completion")
}

// ---- Fix 6b proof: the chain guard owns pure node-completion debt on the
// ON path (three self-sustaining links, no notice rows). ----

func TestBatchCutoverChainGuardON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)

	// Node debt: a settled detached root (delivery still pending -- the
	// pending-inclusive rows include it).
	env.invokeDetached("call-chain", "view", fast)
	rootID := env.rootID("call-chain")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "node debt present")

	// Three self-sustaining chain links over BATCH evidence: each leg
	// recorded the launched root's tool_call_id (the started response's own
	// identity) and carries the pending-inclusive node debt rows of exactly
	// that root.
	debtRows, err := env.store.BatchReactionPendingInclusiveDebtRows(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, debtRows.Nodes, 1, "the settled root is the one node debt row")
	att := &drainAttempt{chainBatchRoots: []string{"call-chain"}, batchSnapshot: session.BatchDebtSnapshot{Owner: env.sessionID, Nodes: debtRows.Nodes}}
	for i := 0; i < 3; i++ {
		coord.arb.chainLink(env.sessionID, att, session.DebtSnapshot{})
	}
	require.GreaterOrEqual(t, coord.arb.chainLinksOf(env.sessionID), 3)

	pv := coord.drainPolicy(env.ctx, env.sessionID, false)
	require.Equal(t, reactionChainReason, pv.reason,
		"the chain guard defers on node-completion debt the chain launched")
	t.Logf("ChainGuard defer reason=%q", pv.reason)

	// A NON-completion row breaks the chain ownership: a pending child
	// question in the rows makes rule 5's rows.Notices==0 condition fail,
	// so it cannot fire even with the links counted.
	now := time.Now().Unix()
	_, err = env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindChildQuestion,
		Text: "chain question?", Wake: 1, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.NotEqual(t, reactionChainReason, pv.reason,
		"a mixed-in question row breaks the chain ownership")

	// An UNRELATED completion (a root the chain never launched) is NOT
	// owned: the ownership predicate fails and the defer disappears even
	// though the debt is pure node-completion again.
	env.invokeDetached("call-chain-other", "view", fast)
	otherID := env.rootID("call-chain-other")
	require.Equal(t, "completed", env.rootTerminal(otherID))
	require.Eventually(t, func() bool { return env.debtExists() },
		5*time.Second, 5*time.Millisecond, "the unrelated root's debt committed")
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.NotEqual(t, reactionChainReason, pv.reason,
		"an unrelated completion is not chain-owned: no defer")

	// Without node debt (settle it away) there is no defer.
	env.requireBatchPull(2)
	snapB, err := facts.CaptureVisibleDebt(env.ctx, env.sessionID)
	require.NoError(t, err)
	_, err = facts.SettleReactedFailedWithMarker(env.ctx, env.sessionID, snapB, "chain probe")
	require.NoError(t, err)
	pv = coord.drainPolicy(env.ctx, env.sessionID, false)
	require.NotEqual(t, reactionChainReason, pv.reason, "no node debt: no chain defer")
}

// ---- Fix 6a proof: supervision's scope input and summary are facts-driven
// on the ON path. ----

func TestBatchCutoverSupervisionON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	l := coord.asyncJobs

	env.invokeDetached("call-super", "view", leaf)
	leaf.waitStarted(t, "call-super")
	rootID := env.rootID("call-super")

	require.True(t, l.supervisionWorkOpen(env.sessionID), "the live gated root keeps the scope open")
	text, flagged := l.buildSupervisionSummaryAt(time.Now(), env.sessionID, 1, time.Minute, false)
	require.Contains(t, text, "call-super", "the summary names the live node's tool call id")
	require.Nil(t, flagged, "silence escalation awaits the R12-F batch output-buffer seam; check-ins, pause wording, scope and held-question policy are preserved")
	t.Logf("SupervisionON text=%q", text)

	leaf.letGo("call-super")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool { return !l.supervisionWorkOpen(env.sessionID) },
		5*time.Second, 5*time.Millisecond, "the scope closed after the settle")

	// Synthetic delegation descendant (the DelegationFactsON reservation
	// pattern): a live child node naming a delegation child session must
	// increment the header's delegation count.
	child := "sess-super-child"
	res, err := env.store.Reserve(env.ctx, session.BatchReservation{
		Owner: env.sessionID, Host: "host-super", RunnerID: "runner-super",
		ToolCallID: "call-super-del", InputHash: "hash-super-del", Sequence: 100,
		Root: session.BatchReservationNode{
			Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: nullString("parallel"), State: "running", ControlState: "none"},
			Children: []session.BatchReservationNode{
				{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: nullString("agent"), State: "pending", ControlState: "none", ChildSessionID: nullString(child)}},
			},
		},
	})
	require.NoError(t, err)
	childNode := res.Snapshot.Nodes[1].NodeID
	require.NoError(t, env.store.Project(env.ctx, session.BatchProjection{
		Host: "host-super", RunnerID: "runner-super", Sequence: 101,
		Roots: []session.BatchRootProjection{{
			Owner: env.sessionID, Root: res.Snapshot.Root.RootID,
			Updates: []db.ProjectBatchNodeParams{{NodeID: childNode, State: "running", ControlState: "none"}},
		}},
	}))
	require.True(t, l.supervisionWorkOpen(env.sessionID), "the delegation keeps the scope open")
	text, flagged = l.buildSupervisionSummaryAt(time.Now(), env.sessionID, 2, time.Minute, false)
	require.Contains(t, text, "1 sub-agent delegation(s) in progress",
		"the Descendants fact increments the delegation count")
	require.Contains(t, text, "call-super-del", "the summary names the delegation's tool call id")
	require.Nil(t, flagged)

	// A pending child_question notice adds the awaiting clause and the
	// guidance prefix; settling (reacting) the question removes both.
	now := time.Now().Unix()
	_, err = env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindChildQuestion,
		Text: "super question?", Wake: 1, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	text, flagged = l.buildSupervisionSummaryAt(time.Now(), env.sessionID, 3, time.Minute, false)
	require.Contains(t, text, "(1 awaiting your answer)", "the awaiting clause appears for the held question")
	require.Contains(t, text, "A sub-agent is blocked on your answer; waiting will not progress it.",
		"the awaiting guidance prefix appears")
	require.Nil(t, flagged)
	_, err = env.conn.ExecContext(env.ctx,
		`UPDATE session_notices SET reacted = 1 WHERE owner = ?1 AND kind = ?2`,
		env.sessionID, session.NoticeKindChildQuestion)
	require.NoError(t, err)
	text, flagged = l.buildSupervisionSummaryAt(time.Now(), env.sessionID, 4, time.Minute, false)
	require.NotContains(t, text, "awaiting your answer", "the awaiting clause disappears once settled")
	require.NotContains(t, text, "blocked on your answer")
	require.Nil(t, flagged, "silence escalation awaits the R12-F batch output-buffer seam; check-ins, pause wording, scope and held-question policy are preserved")
}

// ---- Fix 5 proof: the ON notice pull delivers batch notices and session
// notices WITHOUT the legacy void queries (async_jobs is dropped). ----

func TestBatchCutoverNoticePullON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	leaf := newGatedLeaf("view")
	fast := &countingTool{name: "grep"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf, fast)
	releaseAllAt(t, leaf)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)
	_, err := env.conn.ExecContext(env.ctx, `DROP TABLE async_jobs`)
	require.NoError(t, err)

	// Batch notices: a settled detached root delivers exactly once.
	env.invokeDetached("call-pull", "grep", fast)
	rootID := env.rootID("call-pull")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	pulled, err := facts.PullBatchNotices(env.ctx, env.messages, env.sessionID)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	delivery, _, _, _ := env.nodeNotice(rootID)
	require.Equal(t, "done", delivery)
	pulled, err = facts.PullBatchNotices(env.ctx, env.messages, env.sessionID)
	require.NoError(t, err)
	require.Empty(t, pulled, "consumed once")

	// Safe session pull, live scope: a supervision check-in DELIVERS while
	// the gated root holds the scope open.
	now := time.Now().Unix()
	_, err = env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindSupervision,
		Text: "check-in one", Wake: 1, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	env.invokeDetached("call-pull-live", "view", leaf)
	leaf.waitStarted(t, "call-pull-live")
	sp, err := facts.PullPendingSessionNotices(env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, sp, 1, "the check-in delivers while the scope is open")

	// Scope closed: a later check-in is VOIDED, not delivered.
	leaf.letGo("call-pull-live")
	require.Equal(t, "completed", env.rootTerminal(env.rootID("call-pull-live")))
	now = time.Now().Unix()
	_, err = env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindSupervision,
		Text: "check-in two", Wake: 1, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	sp, err = facts.PullPendingSessionNotices(env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Empty(t, sp, "the closed-scope check-in is voided")
	var delivery2 string
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT delivery FROM session_notices WHERE owner = ?1 AND text = 'check-in two'`,
		env.sessionID).Scan(&delivery2))
	require.Equal(t, "void", delivery2)

	// A child question (job_tool_call_id NULL in batch mode) delivers.
	now = time.Now().Unix()
	_, err = env.q.InsertSessionNotice(env.ctx, db.InsertSessionNoticeParams{
		Owner: env.sessionID, Kind: session.NoticeKindChildQuestion,
		Text: "held question?", Wake: 1, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	sp, err = facts.PullPendingSessionNotices(env.ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, sp, 1, "the child question delivers without legacy validation")
	env.logDebtRows(t)
	env.logNodeRows(t, rootID)
}

// ---- Fix 4 proof: the accounting settles EXACTLY the acted-on rows. ----

func TestBatchCutoverSettleExactRowsON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)

	// Two real roots, both settled AND pulled (visible debt).
	env.invokeDetached("call-exact-a", "view", fast)
	rootA := env.rootID("call-exact-a")
	require.Equal(t, "completed", env.rootTerminal(rootA))
	env.invokeDetached("call-exact-b", "view", fast)
	rootB := env.rootID("call-exact-b")
	require.Equal(t, "completed", env.rootTerminal(rootB))
	env.requireBatchPull(2)
	snapAll, err := facts.CaptureVisibleDebt(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, snapAll.Nodes, 2, "both roots are visible debt")

	require.NoError(t, facts.IncrementWakeAttempts(env.ctx, env.sessionID, snapAll))
	rows, err := facts.AttemptRows(env.ctx, env.sessionID, snapAll, drainFailureSettleThreshold)
	require.NoError(t, err)
	require.GreaterOrEqual(t, rows.Open, 2, "both rows counted")

	// Settle ONLY A's row: B's debt survives.
	var snapA session.BatchDebtSnapshot
	snapA.Owner = snapAll.Owner
	for _, ref := range snapAll.Nodes {
		if ref.RootID == rootA {
			snapA.Nodes = append(snapA.Nodes, ref)
		}
	}
	require.Len(t, snapA.Nodes, 1)
	settled, err := facts.SettleReactedFailedWithMarker(env.ctx, env.sessionID, snapA, "exact settle probe")
	require.NoError(t, err)
	require.EqualValues(t, 1, settled, "exactly A's row settled")

	after, err := facts.CaptureVisibleDebt(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, after.Nodes, 1, "only B remains")
	require.Equal(t, rootB, after.Nodes[0].RootID, "B's debt is untouched")
	visible, err := env.store.BatchReactionVisibleDebtExists(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.True(t, visible)

	// The marker names BOTH roots' call ids before the partial settle.
	ids := facts.DebtToolCallIDs(env.ctx, snapAll)
	require.Contains(t, ids, "call-exact-a")
	require.Contains(t, ids, "call-exact-b")
	require.Len(t, ids, 2, "root-row tool_call_ids deduped per root")
	env.logDebtRows(t)
	env.logNodeRows(t, rootA, rootB)
}

func ctx2Background() context.Context { return context.Background() }
