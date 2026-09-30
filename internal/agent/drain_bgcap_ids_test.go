// The bg-shell cap identifies over-cap completions by their notice ROW IDS
// (R5B-1, R5B-2; plan amendment (ad)): a re-check or a queued Drain's
// turn-start check defers a debt only when EVERY row of it is an over-cap row.
package agent

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// gatedProvider is a provider handler that reports every request on entered and
// answers it only once release is closed.
func gatedProvider(entered chan<- struct{}, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		textFinishResponse(w, "reacted")
	}
}

func awaitRequest(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the provider was never called")
	}
}

// insertSlotRow persists a bg-shell completion the way a slot holder's row
// exists: durable, wake=1, not routed through the over-cap accounting.
func (f *attemptFixture) insertSlotRow(ctx context.Context) {
	f.t.Helper()
	require.NoError(f.t, f.store.InsertSessionNotice(ctx, f.sessID, session.NoticeKindBGShellDone, "slot row", true, ""))
}

// settleNotice marks a notice row as pulled and reacted (what a Drain that got
// past the row does), leaving every other row alone.
func (f *attemptFixture) settleNotice(ctx context.Context, id int64) {
	f.t.Helper()
	f.exec(ctx, fmt.Sprintf(`UPDATE session_notices SET delivery='done', reacted=1 WHERE id=%d`, id))
}

// R5B-1 (variant A/B): a Drain QUEUED behind the last permitted Drain commits
// at its turn start on debt its launch decision never saw. The last slot's
// Drain D5 is streaming; a re-check queues Dq behind it; an over-cap completion
// lands after D5's last pull. D5 reacts to its own row; Dq then finds only the
// over-cap row and must NOT reach the provider (a sixth automatic turn).
//
// Revert-check: passing spent=true from decideDrainTurn (the turn-start check
// never compares the cap) sends a second request and turns this red.
func TestBGShellCap_QueuedDrainNeverCommitsOnAnOverCapRow(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan struct{}, 8), make(chan struct{})
	f, runs := newBGShellCapFixture(t, "cap-queued-drain", attemptFixtureOpts{handler: gatedProvider(entered, release)})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	f.insertSlotRow(ctx) // slot 5's row

	d5 := make(chan error, 1)
	go func() { d5 <- f.coord.wakeSession(ctx, f.sessID, true) }() // D5 (its slot is spent)
	awaitRequest(t, entered)

	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, false), "the re-check queues Dq behind D5")
	require.EqualValues(t, 2, runs.runs.Load())
	require.False(t, f.coord.persistBGShellCompletion(f.sessID, "sh6", "done"), "no slot left: over the cap")
	require.EqualValues(t, 1, f.coord.bgShellOverCapCount(f.sessID))

	close(release)
	require.NoError(t, <-d5)
	f.coord.waitRecheckWakes()
	require.Eventually(t, func() bool { return !f.sa.IsSessionBusy(f.sessID) }, 10*time.Second, 5*time.Millisecond)

	require.EqualValues(t, 1, f.requests.Load(), "exactly one provider request: the over-cap row got no turn")
	require.True(t, f.hasDebt(ctx), "the over-cap row stays owed")
}

// The other direction: a slot Drain's own row is not an over-cap row, so the
// turn-start comparison never refuses it. D5 launched by its own fact reaches
// the provider and reacts.
//
// Revert-check: deferring at the cap without the row comparison (any bg-shell
// debt at the turn start) refuses D5 and turns the request count red.
func TestBGShellCap_LastSlotDrainRunsAtTurnStart(t *testing.T) {
	ctx := context.Background()
	f, _ := newBGShellCapFixture(t, "cap-last-slot", attemptFixtureOpts{})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	f.insertSlotRow(ctx)

	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, true))

	require.EqualValues(t, 1, f.requests.Load(), "the slot row's Drain reaches the provider")
	require.False(t, f.hasDebt(ctx), "and reacts")
}

// A Drain queued for the LAST slot behind the fourth slot's Drain still runs:
// its row was admitted with a slot, so at its turn start the debt is a slot row
// and not an over-cap one.
//
// Revert-check: the same row-comparison removal as above turns the second
// request red.
func TestBGShellCap_QueuedDrainForTheLastSlotStillRuns(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan struct{}, 8), make(chan struct{})
	f, _ := newBGShellCapFixture(t, "cap-queued-last-slot", attemptFixtureOpts{handler: gatedProvider(entered, release)})
	for range maxConsecutiveAutoResumes - 1 {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	f.insertSlotRow(ctx) // slot 4's row

	d4 := make(chan error, 1)
	go func() { d4 <- f.coord.wakeSession(ctx, f.sessID, true) }()
	awaitRequest(t, entered)

	// Completion 5 takes the last slot while D4 streams: its Drain queues.
	require.True(t, f.coord.persistBGShellCompletion(f.sessID, "sh5", "done"))
	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, true))

	close(release)
	require.NoError(t, <-d4)
	f.coord.waitRecheckWakes()
	require.Eventually(t, func() bool { return !f.sa.IsSessionBusy(f.sessID) && !f.hasDebt(ctx) }, 10*time.Second, 5*time.Millisecond)

	require.EqualValues(t, 2, f.requests.Load(), "the queued Drain reacted to slot 5's row")
}

// R5B-2: a completion whose notice insert FAILED left no row, so it must not
// count as over-cap: it cannot hide the slot row that IS owed. Slot 5's row is
// debt (its Drain failed earlier); shell 6 finishes while the notice table is
// unwritable; the re-check must still retry the slot row.
//
// Revert-check: recording a completion whose insert failed (a phantom id) turns
// the count assertion red; with the round-4 rule (a bare count, no prune) the
// slot row is deferred and the second assertion turns red too.
func TestBGShellCap_FailedInsertIsNotOverCap(t *testing.T) {
	ctx := context.Background()
	f, _ := newBGShellCapFixture(t, "cap-failed-insert", attemptFixtureOpts{})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	f.insertSlotRow(ctx)

	f.exec(ctx, `ALTER TABLE session_notices RENAME TO fx_session_notices`)
	require.False(t, f.coord.persistBGShellCompletion(f.sessID, "sh6", "done"))
	f.exec(ctx, `ALTER TABLE fx_session_notices RENAME TO session_notices`)

	require.Zero(t, f.coord.bgShellOverCapCount(f.sessID), "a failed insert counts nothing")
	deferred, err := f.coord.bgShellCapDeferred(ctx, f.sessID)
	require.NoError(t, err)
	require.False(t, deferred, "the slot row is still owed; the lost completion has no row to defer")
}

// The (aa).5 residual: the slot row's pull failed while a newer over-cap row
// was pulled and reacted. The old row-count rule saw one row against one
// over-cap completion and deferred the slot row forever; the id rule sees a
// debt row that is not over-cap and retries it.
//
// Revert-check: the round-4 rule (compare the debt's size with a bare count of
// over-cap completions, no per-row ids) defers here, launches nothing and turns
// the request count red.
func TestBGShellCap_SlotRowWhosePullFailedIsRetriedBehindAReactedOverCapRow(t *testing.T) {
	ctx := context.Background()
	f, _ := newBGShellCapFixture(t, "cap-old-row-retried", attemptFixtureOpts{})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	f.insertSlotRow(ctx) // slot 5's row: stays pending (its pull failed)
	require.False(t, f.coord.persistBGShellCompletion(f.sessID, "sh6", "done"))
	notices, err := f.store.ListSessionNotices(ctx, f.sessID)
	require.NoError(t, err)
	require.Len(t, notices, 2)
	f.settleNotice(ctx, notices[1].ID) // the newer, over-cap row was reacted

	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, false))

	require.EqualValues(t, 1, f.requests.Load(), "the slot row is retried like any debt")
	require.False(t, f.hasDebt(ctx))
}

// Over-cap ids that left the debt (reacted or closed) are dropped by the next
// check, so the set follows the durable debt instead of growing with every
// completion; an id still owed stays.
//
// Revert-check: dropping the prune from bgShellCapDeferred leaves both ids in
// the set and turns the count assertions red.
func TestBGShellCapDeferred_PrunesOverCapIDsThatLeftTheDebt(t *testing.T) {
	ctx := context.Background()
	f, _ := newBGShellCapFixture(t, "cap-prune", attemptFixtureOpts{})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	for range 3 {
		require.False(t, f.coord.persistBGShellCompletion(f.sessID, "sh", "done"))
	}
	require.EqualValues(t, 3, f.coord.bgShellOverCapCount(f.sessID))
	notices, err := f.store.ListSessionNotices(ctx, f.sessID)
	require.NoError(t, err)
	require.Len(t, notices, 3)
	f.settleNotice(ctx, notices[0].ID)
	f.settleNotice(ctx, notices[1].ID)

	deferred, err := f.coord.bgShellCapDeferred(ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, deferred, "the remaining row is over-cap")
	require.EqualValues(t, 1, f.coord.bgShellOverCapCount(f.sessID), "reacted ids are pruned")

	f.settleNotice(ctx, notices[2].ID)
	deferred, err = f.coord.bgShellCapDeferred(ctx, f.sessID)
	require.NoError(t, err)
	require.False(t, deferred, "no debt: nothing to defer")
	require.Zero(t, f.coord.bgShellOverCapCount(f.sessID))
}

// A completion's arrival prunes the ids that left the debt too, so a session
// that never sees a re-check does not accumulate them: two of three recorded
// rows were reacted; the next arrival leaves the still-owed one and itself.
//
// Revert-check: dropping pruneBGShellOverCap from persistBGShellCompletion
// leaves all four ids and turns the count red.
func TestPersistBGShellCompletion_PrunesOverCapIDsThatLeftTheDebt(t *testing.T) {
	ctx := context.Background()
	f, _ := newBGShellCapFixture(t, "cap-arrival-prune", attemptFixtureOpts{})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	for range 3 {
		require.False(t, f.coord.persistBGShellCompletion(f.sessID, "sh", "done"))
	}
	notices, err := f.store.ListSessionNotices(ctx, f.sessID)
	require.NoError(t, err)
	f.settleNotice(ctx, notices[0].ID)
	f.settleNotice(ctx, notices[1].ID)

	require.False(t, f.coord.persistBGShellCompletion(f.sessID, "sh", "done"))

	require.EqualValues(t, 2, f.coord.bgShellOverCapCount(f.sessID), "the owed row and the new one")
}
