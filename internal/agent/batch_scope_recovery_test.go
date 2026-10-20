// H4 W2 scope-time batch dead-host recovery: the coordinator hooks
// (runningWorkOpen, CLIActiveWork's batch branch, the turn preamble) must
// recover this owner's dead-host roots before answering, bounded to ONE
// candidate root per call, with negative verdicts (alive/unknown/probe
// error) writing nothing.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchScopeRecoveryFixture is the shared standalone-coordinator fixture: a
// temp data dir + raw conn, a bound node facts source, and a dead host whose
// lock file has been created and released.
func batchScopeRecoveryFixture(t *testing.T, deadHost string) (*coordinator, *session.AsyncJobStore, *sql.DB, string) {
	t.Helper()
	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	lk, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, deadHost))
	require.NoError(t, err)
	require.NoError(t, lk.Release())
	c := &coordinator{}
	c.asyncJobs = newWorkLedger(nil)
	c.asyncJobs.store = store
	bindBatchNodeFacts(t, c, conn, dataDir)
	return c, store, conn, dataDir
}

// batchScopeRootState reads the ROOT node's (state, outcome_code) for rootID.
func batchScopeRootState(t *testing.T, conn *sql.DB, rootID string) (string, string) {
	t.Helper()
	var state, outcome string
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT state, outcome_code FROM batch_nodes WHERE root_id = ? AND kind = 'batch'`, rootID).Scan(&state, &outcome))
	return state, outcome
}

// batchScopeSelectedNotices reads (count, max wake) over the root's selected
// recovery notices.
func batchScopeSelectedNotices(t *testing.T, conn *sql.DB, rootID string) (int, int) {
	t.Helper()
	var count, wake int
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*), coalesce(max(wake), 0) FROM batch_nodes WHERE root_id = ? AND notice_selected = 1`, rootID).Scan(&count, &wake))
	return count, wake
}

// batchScopeInterruptedRoots counts INTERRUPTED ROOT rows among rootIDs.
func batchScopeInterruptedRoots(t *testing.T, conn *sql.DB, rootIDs ...string) int {
	t.Helper()
	var n int
	args := make([]any, 0, len(rootIDs))
	placeholders := ""
	for i, id := range rootIDs {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	require.NoError(t, conn.QueryRowContext(t.Context(), `SELECT count(*) FROM batch_nodes WHERE kind = 'batch' AND state = 'interrupted' AND root_id IN (`+placeholders+`)`, args...).Scan(&n))
	return n
}

// TestBatchScopeRecovery_CLIScopeRecoversDeadCandidate pins runningWorkOpen's
// H4 W2 hook: CLIScope recovers the dead-host root BEFORE the fresh live
// read, so a recovered root (wake=0) with nothing else outstanding closes
// the scope. OFF twin: TestCLIScope_RecoversDeadHostRowBeforeAnswering.
func TestBatchScopeRecovery_CLIScopeRecoversDeadCandidate(t *testing.T) {
	t.Parallel()
	if !batchFlagEnabled() {
		t.Skip("flag OFF never arms the batch scope hook; the OFF twin is TestCLIScope_RecoversDeadHostRowBeforeAnswering")
	}
	ctx := context.Background()
	c, _, conn, _ := batchScopeRecoveryFixture(t, "scope-dead-host")
	root, _ := b1SeedBatchRootOnHost(t, conn, "sess-scope", "call-scope", "scope-dead-host", "running")

	st, err := c.CLIScope(ctx, "sess-scope")
	require.NoError(t, err)
	require.False(t, st.WorkOpen, "a recovered root (wake=0) with nothing else outstanding must close the scope")

	state, outcome := batchScopeRootState(t, conn, root)
	require.Equal(t, "interrupted", state)
	require.Equal(t, "host_lost", outcome)

	count, wake := batchScopeSelectedNotices(t, conn, root)
	require.Equal(t, 1, count)
	require.Equal(t, 0, wake)
}

// TestBatchScopeRecovery_CLIActiveWorkRecoversDeadCandidate pins
// cliActiveWorkBatch's hook: a recovered dead root must not keep the CLI
// active.
func TestBatchScopeRecovery_CLIActiveWorkRecoversDeadCandidate(t *testing.T) {
	t.Parallel()
	if !batchFlagEnabled() {
		t.Skip("flag OFF keeps CLIActiveWork on the legacy ledger path; no batch hook runs there")
	}
	ctx := context.Background()
	c, _, conn, _ := batchScopeRecoveryFixture(t, "scope-cli-dead")
	root, _ := b1SeedBatchRootOnHost(t, conn, "sess-cli", "call-cli", "scope-cli-dead", "running")

	active, err := c.CLIActiveWork(ctx, "sess-cli")
	require.NoError(t, err)
	require.False(t, active, "a recovered dead root must not keep the CLI active")

	state, _ := batchScopeRootState(t, conn, root)
	require.Equal(t, "interrupted", state)
}

// TestBatchScopeRecovery_BudgetExhaustionStaysLiveThenBackgroundCompletes
// pins the one-root scope budget: with TWO roots on one dead host the first
// scope call recovers exactly one and stays conservatively live; the
// background BatchRecoverPass (the App maintenance primitive) completes the
// rest, and a second scope call then reports inactive.
func TestBatchScopeRecovery_BudgetExhaustionStaysLiveThenBackgroundCompletes(t *testing.T) {
	t.Parallel()
	if !batchFlagEnabled() {
		t.Skip("flag OFF never arms the batch scope hook")
	}
	ctx := context.Background()
	c, _, conn, dataDir := batchScopeRecoveryFixture(t, "scope-budget-dead")
	root1, _ := b1SeedBatchRootOnHost(t, conn, "sess-budget", "call-b1", "scope-budget-dead", "running")
	root2, _ := b1SeedBatchRootOnHost(t, conn, "sess-budget", "call-b2", "scope-budget-dead", "running")

	active, err := c.CLIActiveWork(ctx, "sess-budget")
	require.NoError(t, err)
	require.True(t, active, "the one-root scope budget must leave the second root conservatively live")
	require.Equal(t, 1, batchScopeInterruptedRoots(t, conn, root1, root2))

	_, _, done, err := session.NewBatchStore(conn).BatchRecoverPass(context.Background(), session.BatchSharedHostProbe(dataDir),
		session.BatchRecoveryLimits{MaxRoots: 64, PageSize: 64}, session.BatchRecoveryCursorStart)
	require.NoError(t, err)
	require.True(t, done, "the background pass must have scanned the whole candidate set")

	active, err = c.CLIActiveWork(ctx, "sess-budget")
	require.NoError(t, err)
	require.False(t, active, "the background maintenance pass must complete what the scope budget deferred")
	require.Equal(t, 2, batchScopeInterruptedRoots(t, conn, root1, root2))
}

// TestBatchScopeRecovery_ZeroBudgetWritesNothing is SEQUENTIAL (no
// t.Parallel) because it mutates the package-level budget var while other
// tests may read it; sequential top-level tests complete before the deferred
// parallel group resumes, so this is race-free.
func TestBatchScopeRecovery_ZeroBudgetWritesNothing(t *testing.T) {
	old := batchScopeRecoveryBudget
	batchScopeRecoveryBudget = 0
	t.Cleanup(func() { batchScopeRecoveryBudget = old })
	if !batchFlagEnabled() {
		t.Skip("flag OFF never arms the batch scope hook")
	}
	ctx := context.Background()
	c, _, conn, _ := batchScopeRecoveryFixture(t, "scope-zero-dead")
	root, _ := b1SeedBatchRootOnHost(t, conn, "sess-zero", "call-zero", "scope-zero-dead", "running")

	st, err := c.CLIScope(ctx, "sess-zero")
	require.NoError(t, err)
	require.True(t, st.WorkOpen, "an expired recovery budget must leave the scope conservatively live")

	state, _ := batchScopeRootState(t, conn, root)
	require.Equal(t, "running", state, "a zero (immediately expired) recovery budget must write nothing")
	count, _ := batchScopeSelectedNotices(t, conn, root)
	require.Zero(t, count, "a zero recovery budget must select no notice")
}

// TestBatchScopeRecovery_NegativeVerdictsWriteNothing pins the dead-only
// authority: alive hosts, unknown hosts and failed probes never write.
func TestBatchScopeRecovery_NegativeVerdictsWriteNothing(t *testing.T) {
	t.Parallel()
	t.Run("alive", func(t *testing.T) {
		t.Parallel()
		if !batchFlagEnabled() {
			t.Skip("flag OFF never arms the batch scope hook")
		}
		ctx := context.Background()
		store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
		holder, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, "scope-alive-host"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = holder.Release() })
		c := &coordinator{}
		c.asyncJobs = newWorkLedger(nil)
		c.asyncJobs.store = store
		bindBatchNodeFacts(t, c, conn, dataDir)
		root, _ := b1SeedBatchRootOnHost(t, conn, "sess-alive", "call-alive", "scope-alive-host", "running")

		st, err := c.CLIScope(ctx, "sess-alive")
		require.NoError(t, err)
		require.True(t, st.WorkOpen, "a running root on a live host must keep the scope open")
		state, _ := batchScopeRootState(t, conn, root)
		require.Equal(t, "running", state, "a live host's root must never be recovered")
	})

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		if !batchFlagEnabled() {
			t.Skip("flag OFF never arms the batch scope hook")
		}
		ctx := context.Background()
		store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
		require.NoError(t, os.MkdirAll(session.HostLockPath(dataDir, "scope-unknown-host"), 0o755))
		probeStatus, probeErr := session.ProbeHostShared(dataDir, "scope-unknown-host")
		require.Equal(t, session.HostStatusUnknown, probeStatus)
		require.Error(t, probeErr)
		c := &coordinator{}
		c.asyncJobs = newWorkLedger(nil)
		c.asyncJobs.store = store
		bindBatchNodeFacts(t, c, conn, dataDir)
		root, _ := b1SeedBatchRootOnHost(t, conn, "sess-unknown", "call-unknown", "scope-unknown-host", "running")
		beforeNodes, err := db.New(conn).ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: root, OwnerSessionID: "sess-unknown"})
		require.NoError(t, err)

		beforeState, beforeOutcome := batchScopeRootState(t, conn, root)
		require.Equal(t, "running", beforeState)
		require.Empty(t, beforeOutcome)
		st, err := c.CLIScope(ctx, "sess-unknown")
		require.NoError(t, err)
		require.True(t, st.WorkOpen, "an unknown host must keep the scope open (conservative)")
		state, outcome := batchScopeRootState(t, conn, root)
		require.Equal(t, beforeState, state)
		require.Equal(t, beforeOutcome, outcome)
		count, wake := batchScopeSelectedNotices(t, conn, root)
		require.Zero(t, count)
		require.Zero(t, wake)
		afterNodes, err := db.New(conn).ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: root, OwnerSessionID: "sess-unknown"})
		require.NoError(t, err)
		require.Equal(t, beforeNodes, afterNodes)
	})

	t.Run("dead-with-probe-error", func(t *testing.T) {
		t.Parallel()
		// The production scope helper (recoverDeadCandidates) deliberately
		// drops this error (best-effort), which is why the helper-level
		// guarantee is asserted on the store primitive here.
		ctx := context.Background()
		_, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
		lk, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, "scope-err-host"))
		require.NoError(t, err)
		require.NoError(t, lk.Release())
		root, _ := b1SeedBatchRootOnHost(t, conn, "sess-err", "call-err", "scope-err-host", "running")

		probe := func(string) (session.HostLockStatus, error) {
			return session.HostStatusDead, errors.New("probe failure")
		}
		out, _, err := session.NewBatchStore(conn).RecoverHostsOnce(ctx, probe, []string{"scope-err-host"}, 1)
		// RecoverHostsOnce appends every probe error to errs and returns
		// errors.Join(errs...) (internal/session/batch_recovery_candidates.go):
		// a probe error is reported per host, never swallowed, so err is
		// non-nil here.
		require.Error(t, err, "a probe error is reported per host, never swallowed")
		require.Zero(t, out.Interrupted, "a (Dead, err) verdict is not recoverable")
		state, _ := batchScopeRootState(t, conn, root)
		require.Equal(t, "running", state, "a failed probe must write nothing")
	})
}

func TestBatchScopeRecovery_DeadHostMissingLock(t *testing.T) {
	t.Parallel()
	if !batchFlagEnabled() {
		t.Skip("flag OFF never arms the batch scope hook")
	}
	ctx := context.Background()
	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	c := &coordinator{}
	c.asyncJobs = newWorkLedger(nil)
	c.asyncJobs.store = store
	bindBatchNodeFacts(t, c, conn, dataDir)
	_, err := os.Stat(session.HostLockPath(dataDir, "scope-missing-lock"))
	require.ErrorIs(t, err, os.ErrNotExist)
	probeStatus, probeErr := session.ProbeHostShared(dataDir, "scope-missing-lock")
	require.Equal(t, session.HostStatusDead, probeStatus)
	require.NoError(t, probeErr)
	root, _ := b1SeedBatchRootOnHost(t, conn, "sess-missing", "call-missing", "scope-missing-lock", "running")
	st, err := c.CLIScope(ctx, "sess-missing")
	require.NoError(t, err)
	require.False(t, st.WorkOpen)
	state, outcome := batchScopeRootState(t, conn, root)
	require.Equal(t, "interrupted", state)
	require.Equal(t, "host_lost", outcome)
	count, wake := batchScopeSelectedNotices(t, conn, root)
	require.Equal(t, 1, count)
	require.Zero(t, wake)
}

// TestBatchScopeRecovery_PreambleHookRecoversBeforePull pins the turn
// preamble ordering (agent_turn.go): the scope hook runs BEFORE
// pullPendingNotices, so the recovery notice committed by the hook is
// pulled in the SAME turn.
//
// Limit of this test: it pins the ordering at COMPOSITION level (helper
// then pull), not through runTurn itself. The production entry (runTurn's
// ON hook, agent_turn.go feeding pullPendingNotices right after) needs a
// live provider stream + session lock + mailbox; the only existing harness
// that drives runTurn end-to-end is the SSE provider fixture in
// drain_notice_pull_test.go, which would re-test the entire turn rather
// than the recovery hook (grep for `runTurn(` in internal/agent/*_test.go
// finds no lighter seam). The ordering is also pinned at code level by the
// adjacent placement of the hook and the pull in agent_turn.go.
func TestBatchScopeRecovery_PreambleHookRecoversBeforePull(t *testing.T) {
	t.Parallel()
	if !batchFlagEnabled() {
		t.Skip("flag OFF never arms the batch scope hook")
	}
	ctx := context.Background()
	c, store, conn, _ := batchScopeRecoveryFixture(t, "scope-preamble-dead")
	root, _ := b1SeedBatchRootOnHost(t, conn, "sess-preamble", "call-preamble", "scope-preamble-dead", "running")

	led := newWorkLedger(nil)
	led.coord = c
	led.store = store
	a := &sessionAgent{asyncJobs: led, messages: message.NewService(db.New(conn))}

	recoverBatchScopeDeadCandidates(ctx, c, "sess-preamble")
	pulled := a.pullPendingNotices(ctx, "sess-preamble")
	require.Len(t, pulled, 1, "the recovery notice committed by the preamble hook must be pulled in the SAME turn")
	require.Contains(t, pulled[0].Parts[0].(message.TextContent).Text, "was interrupted")

	state, _ := batchScopeRootState(t, conn, root)
	require.Equal(t, "interrupted", state)
}

// TestBatchScopeRecovery_OffModeInert pins the OFF coordinator's inertness:
// an unbound (OFF) coordinator's hook is a no-op — the node state must stay
// "running" and no notice may be selected.
func TestBatchScopeRecovery_OffModeInert(t *testing.T) {
	if batchFlagEnabled() {
		t.Skip("OFF-only: the ON coordinator's hook does recover (see the siblings above)")
	}
	ctx := context.Background()
	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	lk, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, "scope-off-dead"))
	require.NoError(t, err)
	require.NoError(t, lk.Release())
	// NO facts binding: the flag-OFF App never binds the node source, so the
	// hook's zero source must recover nothing.
	c := &coordinator{}
	c.asyncJobs = newWorkLedger(nil)
	c.asyncJobs.store = store
	root, _ := b1SeedBatchRootOnHost(t, conn, "sess-off", "call-off", "scope-off-dead", "running")

	recoverBatchScopeDeadCandidates(ctx, c, "sess-off")

	state, _ := batchScopeRootState(t, conn, root)
	require.Equal(t, "running", state, "the OFF coordinator's hook must be a no-op")
	count, _ := batchScopeSelectedNotices(t, conn, root)
	require.Zero(t, count)
}

// TestBatchNoticeClassification_RecoveredNodeIsInterruption pins
// buildBatchNoticeMessageParams's H4 W2 classification: state "interrupted"
// or outcome_code "host_lost" reads as an interruption; a plain failure
// stays a failure.
func TestBatchNoticeClassification_RecoveredNodeIsInterruption(t *testing.T) {
	t.Parallel()

	textOf := func(row session.BatchNoticeRow) string {
		params := buildBatchNoticeMessageParams(row)
		require.Len(t, params.Parts, 1)
		return params.Parts[0].(message.TextContent).Text
	}

	t.Run("state interrupted", func(t *testing.T) {
		t.Parallel()
		text := textOf(session.BatchNoticeRow{ToolCallID: "w2-i", Kind: "root", State: "interrupted", OutcomeCode: "host_lost", OutcomeSummary: "Host lost; execution was not resumed"})
		require.Contains(t, text, "was interrupted", "a recovered node must be presented as an interruption, not a completion")
	})

	t.Run("outcome host_lost", func(t *testing.T) {
		t.Parallel()
		text := textOf(session.BatchNoticeRow{ToolCallID: "w2-l", Kind: "root", State: "completed", OutcomeCode: "host_lost", OutcomeSummary: "Host lost; execution was not resumed"})
		require.Contains(t, text, "was interrupted", "the outcome-code half of the classification must also read as an interruption")
	})

	t.Run("plain failure stays a failure", func(t *testing.T) {
		t.Parallel()
		text := textOf(session.BatchNoticeRow{ToolCallID: "w2-f", Kind: "root", State: "failed", OutcomeSummary: "boom"})
		require.Contains(t, text, "failed")
		require.NotContains(t, text, "was interrupted", "no over-classification: a plain failure is not an interruption")
	})
}
