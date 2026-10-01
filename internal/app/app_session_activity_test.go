package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// The reader's batch oracle (plan R-ACT rework): the batched pass must
// answer exactly what the single-session reader answers for the same ids on
// a real SQLite store -- the single reader IS the batch reader with one id,
// so any divergence is a fact-read that behaves differently when batched
// (the class R7C-2/R8C-8 came from). The shapes cover every fact kind and
// every architect decision that shapes the reader:
//
//   - D5: a driver marker on a DEAD host is a crashed fact, not dropped;
//   - D6: debt seeds only as a retry wait behind a live driver;
//   - D7: a CHILD session's ended_reason is read (the top-level session
//     list does not carry children), and a DELETED session reads as no end
//     fact, never as unreadable;
//   - D8: no fact read may fail in this fixture (Unreadable stays empty);
//   - D11: the stem reverse map maps sanitised lock-file names back to the
//     real ids, slug ids included;
//   - D13: the batched debt and wake reads are the same predicates as the
//     single-owner reads (binding tests below).
//
// Revert-check mutants (described per binding test and in the classifier
// table's comment): breaking any batched fact read leaves Unreadable set,
// so the parity require.Equal fails; breaking the stem map fails the D11
// asserts; dropping the dead-host scan fails the deadHost verdict.
func TestSessionActivityBatch_MatchesSingle(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()

	mk := func(id string) string {
		_, err := h.app.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
		return id
	}
	between := mk("between")
	slug := mk("locks/driven x")
	live := mk("live")
	deadHost := mk("deadhost")
	ended := mk("ended")
	scheduled := mk("scheduled")
	child := mk("child-of-something")
	deleted := mk("deleted")

	// between + slug: a live loop with pending debt; the slug's lock file
	// is the sanitised stem, empty (clean release) -- released, not in
	// turn (D10).
	require.NoError(t, h.app.asyncJobStore.ClaimSessionDriver(ctx, between))
	require.NoError(t, h.app.asyncJobStore.ClaimSessionDriver(ctx, slug))
	require.NoError(t, h.app.asyncJobStore.InsertSessionNotice(ctx, between, "wake", "x", true, ""))
	require.NoError(t, h.app.asyncJobStore.InsertSessionNotice(ctx, slug, "wake", "x", true, ""))
	for _, id := range []string{between, slug} {
		require.NoError(t, writeLockRecord(t, h.dataDir, id, "", 0))
	}

	// live: a lock record held by this (alive) process.
	require.NoError(t, writeLockRecord(t, h.dataDir, live, strconv.Itoa(os.Getpid()), 0))

	// deadhost: a marker whose host is provably dead (D5).
	deadStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 4242, "dead")
	require.NoError(t, deadStore.ClaimSessionDriver(ctx, deadHost))
	require.NoError(t, deadStore.SimulateCrashForTest())

	// ended: a recorded reason.
	require.NoError(t, h.app.Sessions.SetEndedReason(ctx, ended, "end_turn"))

	// child: ended_reason on a session that is a child of another (not in
	// the top-level list; read through the same batched statement, D7).
	parent := mk("parent")
	require.NoError(t, h.app.Sessions.SetEndedReason(ctx, child, "end_turn"))
	require.NotEmpty(t, parent)

	// deleted: the session row goes away; the reader must not report an
	// unreadable end fact for it (D7).
	require.NoError(t, h.app.Sessions.Delete(ctx, deleted))

	// scheduled: an open once wake schedule.
	_, err := h.app.WakeScheduleStore().CreateSchedule(ctx, session.CreateWakeScheduleParams{
		Owner: scheduled, Kind: session.WakeKindOnce, Message: "tick",
		RunAt: time.Now().Add(5 * time.Minute),
	}, time.Now())
	require.NoError(t, err)

	ids := []string{between, slug, live, deadHost, ended, scheduled, child, deleted}
	batch, err := h.app.SessionActivityBatch(ctx, ids)
	require.NoError(t, err)
	require.Len(t, batch.ByID, len(ids))

	for _, id := range ids {
		single, err := h.app.SessionActivity(ctx, id)
		require.NoError(t, err, id)
		require.Equal(t, single, batch.ByID[id], "batch diverges from single for %s", id)
		require.Empty(t, batch.ByID[id].Facts.Unreadable, "no fact read may fail in this fixture (%s)", id)
	}

	require.Equal(t, session.ActivityBetweenTurns, batch.ByID[between].Verdict.Kind)
	require.Contains(t, batch.ByID[between].Verdict.WaitingOn, session.WaitRetry)

	require.Equal(t, session.ActivityBetweenTurns, batch.ByID[slug].Verdict.Kind,
		"R7C-2: the slug id is classified through its real id, not its lock stem")

	require.Equal(t, session.ActivityInTurn, batch.ByID[live].Verdict.Kind)

	require.Equal(t, session.ActivityCrashed, batch.ByID[deadHost].Verdict.Kind,
		"D5: a marker on a provably dead host is a crash fact")

	require.Equal(t, session.ActivityEnded, batch.ByID[ended].Verdict.Kind)
	require.Equal(t, "end_turn", batch.ByID[ended].Verdict.EndedReason)

	require.Equal(t, session.ActivityBetweenTurns, batch.ByID[scheduled].Verdict.Kind)
	require.Contains(t, batch.ByID[scheduled].Verdict.WaitingOn, session.WaitSchedule)

	require.Equal(t, session.ActivityEnded, batch.ByID[child].Verdict.Kind,
		"D7: a child session's ended_reason is read like any other")

	require.Equal(t, session.ActivityIdle, batch.ByID[deleted].Verdict.Kind,
		"D7: a deleted session has no end fact and no unreadable liveness")
	require.False(t, batch.ByID[deleted].Facts.EndUnreadable)

	// D11: the reverse map from lock-file stems to real ids.
	require.Equal(t, []string{slug}, batch.LockStems[session.SessionLockStem(slug)])
	require.Equal(t, []string{between}, batch.LockStems[session.SessionLockStem(between)])
	require.NotContains(t, batch.LockStems, "locks/driven x",
		"the map is keyed by the sanitised stem, not the real id")
}

// D13 binding tests: the batched statements answer the same as the
// single-owner reads they replace, on identical seeded rows.
func TestSessionActivityBatch_BindingTests(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()

	withDebt, without := "bind-debt", "bind-clean"
	for _, id := range []string{withDebt, without} {
		_, err := h.app.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
	}
	require.NoError(t, h.app.asyncJobStore.InsertSessionNotice(ctx, withDebt, "wake", "x", true, ""))

	// Debt: batched vs single-owner EXISTS on the same rows.
	debts, err := h.app.asyncJobStore.ReactionDebtOwners(ctx, []string{withDebt, without})
	require.NoError(t, err)
	singleDebt, err := h.app.asyncJobStore.ReactionDebtExists(ctx, withDebt)
	require.NoError(t, err)
	singleClean, err := h.app.asyncJobStore.ReactionDebtExists(ctx, without)
	require.NoError(t, err)
	require.Equal(t, singleDebt, debts[withDebt])
	require.Equal(t, singleClean, debts[without])

	// Wake schedules: batched vs single-owner read on the same rows.
	next := time.Now().Add(5 * time.Minute).Truncate(time.Second).UTC()
	_, err = h.app.WakeScheduleStore().CreateSchedule(ctx, session.CreateWakeScheduleParams{
		Owner: withDebt, Kind: session.WakeKindOnce, Message: "tick", RunAt: next,
	}, time.Now())
	require.NoError(t, err)
	batchedWake, err := session.OpenOnceWakeSchedulesForOwners(ctx, h.app.WakeScheduleStore(), []string{withDebt, without})
	require.NoError(t, err)
	singleWake, err := session.OpenOnceWakeSchedules(ctx, h.app.WakeScheduleStore(), withDebt)
	require.NoError(t, err)
	require.Equal(t, singleWake, batchedWake[withDebt])
	require.Empty(t, batchedWake[without])
}

// writeLockRecord writes a lock record carrying pid and back-dates its
// mtime by age (0 keeps it fresh) -- freshness must not matter (D10).
func writeLockRecord(t *testing.T, dataDir, sessionID, pid string, age time.Duration) error {
	t.Helper()
	path := session.SessionLockPath(dataDir, sessionID)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	if err := os.WriteFile(path, []byte(pid), 0o644); err != nil {
		return err
	}
	if age > 0 {
		past := time.Now().Add(-age)
		return os.Chtimes(path, past, past)
	}
	return nil
}
