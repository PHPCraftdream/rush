// The web bg-shell auto-resume cap (R3B-6, R4B-1): maxConsecutiveAutoResumes
// slots per human message, spent once at admission (claimAutoResumeSlot) by the
// completion's own launch. A release or tick re-check compares the cap WITHOUT
// spending a slot and defers the debt only when none of its rows holds a slot,
// i.e. every row is a completion that arrived over the cap. A row whose slot
// was spent but whose launch was paced, refused, held or deferred is retried
// and closed at K=3 like any other debt.
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// finishedBackgroundShell runs a trivial real background shell owned by
// sessionID to completion.
func finishedBackgroundShell(t *testing.T, workDir, sessionID string) *shell.BackgroundShell {
	t.Helper()
	mgr := shell.NewBackgroundShellManager()
	sh, err := mgr.StartOwned(t.Context(), sessionID, workDir, nil, "echo done", "cap test")
	require.NoError(t, err)
	require.True(t, sh.WaitContext(t.Context()))
	return sh
}

// newBGShellCapFixture is a plain web root session (no delegation driver) with
// AutoResumeOnJobDone on and its real agent behind a Run counter. The test must
// not be parallel (the OAuth fixture isolates the global config paths).
func newBGShellCapFixture(t *testing.T, title string, opts attemptFixtureOpts) (*attemptFixture, *countingAgent) {
	t.Helper()
	opts.noIdle, opts.noDriver, opts.oauthProvider = true, true, oauthTestProvider
	f := newAttemptFixture(t, title, opts)
	f.rotateCredentials(oauthTestProvider)
	f.coord.cfg.Config().Options.AutoResumeOnJobDone = boolPtr(true)
	f.coord.SetPersistentMode(true)
	t.Cleanup(f.coord.StopRecheckTicker)
	runs := &countingAgent{SessionAgent: f.sa}
	f.coord.currentAgent = runs
	return f, runs
}

// complete delivers one real background-shell completion and waits for its
// detached wake to return.
func (f *attemptFixture) complete() {
	f.t.Helper()
	f.coord.notifyBackgroundJobDone(f.sessID, finishedBackgroundShell(f.t, f.env.workingDir, f.sessID))
	f.coord.waitRecheckWakes()
}

// expireDrainPause ends the session's launch pause without waiting for it.
func (f *attemptFixture) expireDrainPause() {
	f.coord.seedArbiterState(f.sessID, func(s *arbiterState) {
		s.gate.RetryAt = time.Now().Add(-time.Second)
	})
}

// maxNoticeAttempts is the highest wake_attempts among the visible debt rows.
func (f *attemptFixture) maxNoticeAttempts(ctx context.Context) int {
	f.t.Helper()
	snap, err := f.store.CaptureDebtSnapshot(ctx, f.sessID)
	require.NoError(f.t, err)
	n, err := f.store.MaxWakeAttempts(ctx, f.sessID, snap)
	require.NoError(f.t, err)
	return n
}

func (f *attemptFixture) hasDebt(ctx context.Context) bool {
	f.t.Helper()
	debt, err := f.ledger.reactionDebtExists(ctx, f.sessID)
	require.NoError(f.t, err)
	return debt
}

// A13 (rewritten): six REAL completions through notifyBackgroundJobDone; the
// first five each spend a slot and launch a Drain that really reacts. The sixth
// arrives over the cap: it launches nothing, and neither the release/tick
// re-check nor a direct wake launches it either (its row is only an over-cap
// row); a human message re-arms.
//
// Revert-check: making the arbiter cap rule never defer (or dropping the cap
// comparison from drainPolicy) lets the re-check launch a sixth Drain and turns
// the run count red; refusing the fifth completion at the fact path (a "<" after
// the bump) leaves four Drains and turns the first assertion red.
func TestBGShellCap_ExactlyFiveAutoResumes(t *testing.T) {
	ctx := context.Background()
	f, runs := newBGShellCapFixture(t, "attempt-bgshell-cap", attemptFixtureOpts{})

	for range maxConsecutiveAutoResumes {
		f.complete()
	}
	require.Eventually(t, func() bool { return !f.hasDebt(ctx) && !f.sa.IsSessionBusy(f.sessID) }, 10*time.Second, 5*time.Millisecond)
	f.coord.waitRecheckWakes()
	require.EqualValues(t, maxConsecutiveAutoResumes, runs.runs.Load(), "one Drain per slot")

	f.complete() // the sixth: no slot
	require.EqualValues(t, maxConsecutiveAutoResumes, runs.runs.Load(), "the sixth completion launches nothing")
	require.True(t, f.hasDebt(ctx), "its notice stays debt (visible), deferred")
	require.EqualValues(t, 1, f.coord.bgShellOverCapCount(f.sessID))

	// The release of a Drain and the 60s pass re-check the sixth completion's
	// still-owed debt: only an over-cap row is left, so they must not launch.
	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, false))
	f.pass(ctx)
	require.EqualValues(t, maxConsecutiveAutoResumes, runs.runs.Load(), "a re-check must not launch an over-cap row")
	v := f.coord.drainPermitted(ctx, f.sessID, false)
	require.Equal(t, drainDeferred, v.kind)
	require.Contains(t, v.reason, "cap reached")

	f.coord.ResetAutoResumeCounter(f.sessID)
	require.Zero(t, f.coord.bgShellOverCapCount(f.sessID), "a human message clears the over-cap ids too")
	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, false))
	require.EqualValues(t, maxConsecutiveAutoResumes+1, runs.runs.Load(), "a human message re-arms auto-resume")
}

// R4B-1: a slot is spent at admission, before the launch decision, so a
// completion whose launch was paced still owns it. The first Drain fails (a
// counted, paced attempt); four more shells finish inside the pause and spend
// slots 2-5 without submitting anything. Once the pause is over ONE re-check
// must launch (the debt is all slot rows, none over the cap), and the rows are
// retried and closed at K=3 like any other debt: row 1 at its third attempt,
// rows 2-5 (which joined later) at theirs, one marker per close.
//
// Revert-check: deferring every bg-shell-only debt at the cap for a re-check
// (the arbiter cap rule returning the bgOnly answer alone) launches nothing at
// the tick and turns the run count red at the first pass.
func TestBGShellCap_SpentSlotsBehindAPauseAreRetriedAndClosed(t *testing.T) {
	ctx := context.Background()
	shrinkDrainRetry(t, time.Hour) // the pause ends only when the test says so
	f, runs := newBGShellCapFixture(t, "attempt-bgshell-cap-paused", attemptFixtureOpts{handler: emptyReplyResponse})

	f.complete() // slot 1: its Drain reaches the provider, nothing reacted
	require.EqualValues(t, 1, runs.runs.Load())
	require.EqualValues(t, 1, f.maxNoticeAttempts(ctx))
	require.Equal(t, drainPaced, f.coord.drainPermitted(ctx, f.sessID, false).kind)

	for range maxConsecutiveAutoResumes - 1 {
		f.complete() // slots 2-5: the gate is paced, nothing is submitted
	}
	require.Equal(t, maxConsecutiveAutoResumes, f.coord.consecutiveResume(f.sessID), "every slot is spent")
	require.Zero(t, f.coord.bgShellOverCapCount(f.sessID))
	require.EqualValues(t, 1, runs.runs.Load(), "nothing was submitted inside the pause")

	f.expireDrainPause()
	f.pass(ctx)
	require.EqualValues(t, 2, runs.runs.Load(), "a re-check retries the rows whose slot was spent")
	require.EqualValues(t, 2, f.maxNoticeAttempts(ctx), "the first row's second attempt")

	f.expireDrainPause()
	f.pass(ctx)
	require.EqualValues(t, 3, runs.runs.Load())
	require.Equal(t, 1, f.markers(ctx), "the first row reached K=3 and was closed; the later rows keep their own clock")
	require.True(t, f.hasDebt(ctx))

	f.expireDrainPause()
	f.pass(ctx)
	require.EqualValues(t, 4, runs.runs.Load())
	require.Equal(t, 2, f.markers(ctx), "rows 2-5 reached K=3 and were closed")
	require.False(t, f.hasDebt(ctx), "nothing is left owed")
}

// the arbiter cap rule defers exactly when every slot is spent and the ENTIRE
// debt is bg-shell rows every one of which is an over-cap completion (its id is
// in the set).
//
// Revert-check: deferring when ANY row is over-cap (instead of every row) turns
// the "one row holds a slot" case red; dropping the bgOnly guard defers the
// mixed-debt case; deferring before the cap is reached turns the first case
// red; comparing sizes without the prune (the round-4 rule) turns the "gone
// over-cap row" case red.
func TestBGShellCapDeferred_OnlyWhenEveryDebtRowIsOverCap(t *testing.T) {
	cases := []struct {
		name         string
		slots        int
		rows         int
		overIdx      []int // indexes of the debt rows recorded as over-cap
		gone         int   // recorded over-cap ids whose rows are no longer in the debt
		jobDebt      bool
		wantDeferred bool
	}{
		{"cap not reached", maxConsecutiveAutoResumes - 1, 3, []int{0, 1, 2}, 0, false, false},
		{"every row is over the cap", maxConsecutiveAutoResumes, 3, []int{0, 1, 2}, 0, false, true},
		{"one row holds a slot", maxConsecutiveAutoResumes, 3, []int{1, 2}, 0, false, false},
		{"the OLD row holds a slot, newer ones are over the cap", maxConsecutiveAutoResumes, 2, []int{1}, 0, false, false},
		{"an over-cap row that left the debt hides no slot row", maxConsecutiveAutoResumes, 1, nil, 1, false, false},
		{"only slot rows", maxConsecutiveAutoResumes, 5, nil, 0, false, false},
		{"no debt at all", maxConsecutiveAutoResumes, 0, nil, 0, false, false},
		{"job debt alongside an over-cap row", maxConsecutiveAutoResumes, 1, []int{0}, 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, _ := newBGShellCapFixture(t, "cap-deferred", attemptFixtureOpts{noIdle: true})
			for range tc.slots {
				f.coord.bumpConsecutiveResume(f.sessID)
			}
			ids := make([]int64, 0, tc.rows)
			for range tc.rows {
				id, err := f.store.InsertSessionNoticeReturningID(ctx, f.sessID, session.NoticeKindBGShellDone, "done", true, "")
				require.NoError(t, err)
				ids = append(ids, id)
			}
			f.coord.seedArbiterState(f.sessID, func(s *arbiterState) {
				s.overCap = make(map[int64]struct{})
				for _, i := range tc.overIdx {
					s.overCap[ids[i]] = struct{}{}
				}
				for i := range tc.gone {
					s.overCap[int64(9000+i)] = struct{}{}
				}
			})
			if tc.jobDebt {
				f.seedDebt(ctx, "call-1", false)
			}

			deferred, err := capDeferred(ctx, f.coord, f.sessID)
			require.NoError(t, err)
			require.Equal(t, tc.wantDeferred, deferred)
		})
	}
}

// A check that cannot take the arrival gate (a completion is mid-step) fails
// closed as an error, never as "not deferred": readTurnFacts takes bgArrival
// BEFORE the debt-rows read, so a blocked check reads nothing. Proven
// deterministically: the check signals the seam right before the gate, and
// only then the notices table is made unreadable -- the error must name the
// gate-ordered read, not an earlier one.
//
// Revert-check: removing bgArrival from readTurnFacts (the read answers from
// reads that no longer wait for the arrival) lets the check return without
// the error and turns this red.
func TestBGShellCapDeferred_BlockedByAnArrivalInFlightFailsClosed(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "cap-gate", attemptFixtureOpts{noIdle: true})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	require.NoError(t, f.coord.bgArrival.lock(ctx)) // an arrival mid-step
	unlocked := false
	defer func() {
		if !unlocked {
			f.coord.bgArrival.unlock()
		}
	}()

	entered := make(chan struct{})
	seam := func() { close(entered) }
	readTurnFactsGateSeam.Store(&seam)
	t.Cleanup(func() { readTurnFactsGateSeam.Store(nil) })

	type answer struct {
		deferred bool
		err      error
	}
	done := make(chan answer, 1)
	go func() {
		d, err := capDeferred(ctx, f.coord, f.sessID)
		done <- answer{d, err}
	}()
	<-entered // the check is waiting at the arrival gate, reads not started
	f.exec(ctx, `ALTER TABLE session_notices RENAME TO fx_session_notices`)
	f.coord.bgArrival.unlock()
	unlocked = true
	res := <-done
	f.exec(ctx, `ALTER TABLE fx_session_notices RENAME TO session_notices`)

	require.Error(t, res.err, "a check that cannot pass the gate must fail closed")
	require.Contains(t, res.err.Error(), "debt rows")
	require.False(t, res.deferred)
}

// P1-3 of the R-ARB-2 review: an over-cap completion landing in the seam
// between the DB half and the arrival gate must be seen by the same decision
// -- the debt rows are read UNDER bgArrival, so the completion's row is in
// them and its over-cap mark is kept; the decision defers, it does not run.
//
// Revert-check: reading the rows before the gate (the pre-P1-3 order) misses
// the row: the debt does not read bg-shell-only, the decision runs (VRun),
// and the stale owed-set prune erases the over-cap mark -- every assertion
// goes red.
func TestBGShellCap_OverCapCompletionInTheSeamIsKeptAndDefers(t *testing.T) {
	ctx := context.Background()
	f, _ := newBGShellCapFixture(t, "cap-seam", attemptFixtureOpts{noIdle: true})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}

	seam := func() {
		// An over-cap completion lands exactly between the DB half and the
		// gate: durable row + the refused slot recorded by its id -- what
		// persistBGShellCompletion does under bgArrival.
		id, err := f.store.InsertSessionNoticeReturningID(ctx, f.sessID, session.NoticeKindBGShellDone, "done", true, "")
		require.NoError(t, err)
		require.False(t, f.coord.arb.claimAutoResumeSlot(f.sessID, id, true), "no slot left: over the cap")
	}
	readTurnFactsGateSeam.Store(&seam)
	t.Cleanup(func() { readTurnFactsGateSeam.Store(nil) })

	deferred, err := capDeferred(ctx, f.coord, f.sessID)
	require.NoError(t, err)
	require.True(t, deferred, "the debt is entirely over-cap completions: defer, not VRun")
	require.EqualValues(t, 1, f.coord.bgShellOverCapCount(f.sessID), "the completion's mark is not pruned by the stale read")
	v := f.coord.drainPermitted(ctx, f.sessID, false)
	require.Equal(t, drainDeferred, v.kind)
	require.Contains(t, v.reason, "cap reached")
}

// The completion's row insert and its slot decision are ONE step for the cap
// check: the arrival holds bgArrival from before the insert to after the
// decision, so a check never sees a row whose slot decision (a slot, or an
// recorded over-cap id) has not been made yet -- it would take an over-cap row for a
// slot row and launch it.
//
// Revert-check: dropping the gate from persistBGShellCompletion leaves it free
// between the insert and the decision and turns the assertion red.
func TestBGShellCap_ArrivalHoldsTheGateBetweenInsertAndSlotDecision(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "cap-atomic", attemptFixtureOpts{noIdle: true})
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	var heldBetween, rowVisible bool
	seam := func() {
		heldBetween = len(f.coord.bgArrival.ch) == 1
		rowVisible = f.hasDebt(ctx)
	}
	bgArrivalInsertedSeam.Store(&seam)
	t.Cleanup(func() { bgArrivalInsertedSeam.Store(nil) })

	require.False(t, f.coord.persistBGShellCompletion(f.sessID, "sh", "done", false), "no slot left")

	require.True(t, rowVisible, "the row is durable before the decision")
	require.True(t, heldBetween, "and the gate is held across the insert and the slot decision")
	require.Zero(t, len(f.coord.bgArrival.ch), "released afterwards")
	require.EqualValues(t, 1, f.coord.bgShellOverCapCount(f.sessID))
}

// claimAutoResumeSlot spends a slot only while slots remain, and counts as over-cap
// (by row id) exactly the completions refused because every slot is spent (whatever else
// would have refused them); a refusal with slots left (Stop, auto-resume off)
// is not counted.
//
// Revert-check: dropping the over-cap record turns the count assertions red;
// counting every refusal turns the "slots left" assertions red.
func TestClaimAutoResume_CountsOnlyRefusalsForLackOfASlot(t *testing.T) {
	f, _ := newBGShellCapFixture(t, "claim-over-cap", attemptFixtureOpts{})
	sid := f.sessID

	f.coord.suspendAutoResume(sid)
	require.False(t, f.coord.claimAutoResumeSlot(sid, 1, true), "Stop suspended automatic turns")
	require.Zero(t, f.coord.bgShellOverCapCount(sid), "a refusal with slots left is not over-cap")
	f.coord.resetConsecutiveResume(sid)

	f.coord.cfg.Config().Options.AutoResumeOnJobDone = boolPtr(false)
	require.False(t, f.coord.claimAutoResumeSlot(sid, 2, false), "auto-resume is off")
	require.Zero(t, f.coord.bgShellOverCapCount(sid))
	f.coord.cfg.Config().Options.AutoResumeOnJobDone = boolPtr(true)

	for i := range maxConsecutiveAutoResumes {
		require.True(t, f.coord.claimAutoResumeSlot(sid, int64(10+i), true))
	}
	require.False(t, f.coord.claimAutoResumeSlot(sid, 20, true))
	require.False(t, f.coord.claimAutoResumeSlot(sid, 21, true))
	require.EqualValues(t, 2, f.coord.bgShellOverCapCount(sid))
	require.False(t, f.coord.claimAutoResumeSlot(sid, 21, true), "the same row is one id")
	require.EqualValues(t, 2, f.coord.bgShellOverCapCount(sid))
	require.False(t, f.coord.claimAutoResumeSlot(sid, 0, true), "a completion with no row (insert failed) is refused")
	require.EqualValues(t, 2, f.coord.bgShellOverCapCount(sid), "and records nothing: there is no row to defer")
	require.Equal(t, maxConsecutiveAutoResumes, f.coord.consecutiveResume(sid), "the slot count stops at the cap")

	f.coord.suspendAutoResume(sid)
	require.False(t, f.coord.claimAutoResumeSlot(sid, 22, true))
	require.EqualValues(t, 3, f.coord.bgShellOverCapCount(sid), "a suspended completion after the cap is still over it")
}

// With every slot spent, a re-check whose cap state cannot be read fails CLOSED
// like every policy input: deferred, asking for a re-check tick (the gate
// variant of the error is pinned by
// TestBGShellCapDeferred_BlockedByAnArrivalInFlightFailsClosed).
//
// Revert-check: ignoring the arbiter read error in drainPolicy allows the
// launch and turns this red.
func TestDrainPolicy_CapStateUnreadableFailsClosed(t *testing.T) {
	ctx := context.Background()
	f, _ := newBGShellCapFixture(t, "cap-state-readerr", attemptFixtureOpts{})
	f.seedDebt(ctx, "call-1", false)
	for range maxConsecutiveAutoResumes {
		f.coord.bumpConsecutiveResume(f.sessID)
	}
	f.exec(ctx, `ALTER TABLE session_notices RENAME TO fx_session_notices`)

	v := f.coord.drainPolicy(ctx, f.sessID, false)

	require.Equal(t, drainDeferred, v.kind, "an unreadable cap state must never allow a Drain")
	require.True(t, v.recheck)
	require.Error(t, v.err)
	require.Equal(t, "launch decision input unreadable", v.reason)
}
