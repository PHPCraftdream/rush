package mailbox

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	"github.com/stretchr/testify/require"
)

// TestMailbox_Invariant_NoStaleCancelHandleSurvivesAnyMutatorReturn is the
// mechanical, table-driven check rounds 12 and 13's independent reviews
// both recommended in place of further ad-hoc reading. Four rounds of
// human review (BLOCKER-2, round 9; MEDIUM-1, round 11's legacy-reclaim
// branch; finding A, round 12's mb.Submitted branch; the fourth instance,
// round 13's DrainAfterCancel branches) each found ONE more copy of the
// exact same postcondition violation, one branch at a time, because each
// round re-audited whichever function had most recently changed rather
// than enumerating every branch that can leave the Mailbox owned.
//
// The invariant, stated once and checked everywhere: for every Mailbox
// operation that hands "hasNext == true" (or the equivalent "still
// running, ownership continues") back to a turn loop, the postcondition
// MUST be state == MbOwned && current.cancel == nil && dispatcherCancel
// != nil. For every operation that ends an ownership era, the
// postcondition MUST be state == MbIdle && current.cancel == nil &&
// dispatcherCancel == nil. current.cancel must NEVER be left holding a
// handle from a Generation that already ended — that's the shape every
// one of the four found bugs shared.
//
// Each table row starts from a Mailbox that looks exactly like the state
// right after a turn's own genCtx cancel has already fired once (the real
// production shape at every one of these call sites: runTurn calls its
// own `cancel()` before reaching the final drain, and DrainAfterCancel is
// only reached from the isCancelErr branch, i.e. after a cancellation
// already happened) — current.cancel is a closure that records if it's
// ever invoked AGAIN, which would mean a stale handle survived and got
// called by a later Cancel()/InterruptAndReplace() instead of the correct
// dispatcherCancel fallback.
func TestMailbox_Invariant_NoStaleCancelHandleSurvivesAnyMutatorReturn(t *testing.T) {
	type expectation struct {
		stillOwned bool // true: era continues (MbOwned); false: era ends (MbIdle)
	}

	newFixture := func() *Mailbox {
		return &Mailbox{
			State:            MbOwned,
			Epoch:            1,
			DispatcherCancel: func() {},
			Current:          Generation{ID: 1, Cancel: func() {}},
		}
	}

	cases := []struct {
		name string
		run  func(t *testing.T, mb *Mailbox)
		want expectation
	}{
		{
			name: "DrainOrRelease/submitted branch keeps ownership",
			run: func(t *testing.T, mb *Mailbox) {
				mb.Submitted = []agentcall.SessionAgentCall{{SessionID: "s1"}}
				_, hasNext := mb.DrainOrRelease(1)
				require.True(t, hasNext)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "DrainOrRelease/empty ends the era",
			run: func(t *testing.T, mb *Mailbox) {
				_, hasNext := mb.DrainOrRelease(1)
				require.False(t, hasNext)
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "DrainOrReleaseFinal/submitted branch keeps ownership",
			run: func(t *testing.T, mb *Mailbox) {
				mb.Submitted = []agentcall.SessionAgentCall{{SessionID: "s1"}}
				_, hasNext, orphaned, err := mb.DrainOrReleaseFinal(1, nil)
				require.NoError(t, err)
				require.True(t, hasNext)
				require.Empty(t, orphaned)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "DrainOrReleaseFinal/Replacement branch keeps ownership",
			run: func(t *testing.T, mb *Mailbox) {
				repl := agentcall.SessionAgentCall{SessionID: "s1"}
				mb.Replacement = &repl
				_, hasNext, orphaned, err := mb.DrainOrReleaseFinal(1, nil)
				require.NoError(t, err)
				require.True(t, hasNext)
				require.Empty(t, orphaned)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "DrainOrReleaseFinal/both empty releases and ends the era",
			run: func(t *testing.T, mb *Mailbox) {
				_, hasNext, orphaned, err := mb.DrainOrReleaseFinal(1, func() error { return nil })
				require.NoError(t, err)
				require.False(t, hasNext)
				require.Empty(t, orphaned)
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "DrainAfterCancel/Replacement branch keeps ownership",
			run: func(t *testing.T, mb *Mailbox) {
				repl := agentcall.SessionAgentCall{SessionID: "s1"}
				mb.Replacement = &repl
				_, ok := mb.DrainAfterCancel()
				require.True(t, ok)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "DrainAfterCancel/submitted branch keeps ownership",
			run: func(t *testing.T, mb *Mailbox) {
				mb.Submitted = []agentcall.SessionAgentCall{{SessionID: "s1"}}
				_, ok := mb.DrainAfterCancel()
				require.True(t, ok)
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "DrainAfterCancel/empty returns no work and clears the handle",
			run: func(t *testing.T, mb *Mailbox) {
				_, ok := mb.DrainAfterCancel()
				require.False(t, ok, "empty Mailbox must return no work")
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "AbandonOwnership/empty always ends the era",
			run: func(t *testing.T, mb *Mailbox) {
				hadWork := mb.AbandonOwnership(1)
				require.False(t, hadWork)
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "AbandonOwnership/with queued work still always ends the era",
			run: func(t *testing.T, mb *Mailbox) {
				mb.Submitted = []agentcall.SessionAgentCall{{SessionID: "s1"}}
				hadWork := mb.AbandonOwnership(1)
				require.True(t, hadWork)
			},
			want: expectation{stillOwned: false},
		},
		// The shutdown branches added by the closing-review round. The
		// previous review pointed out this table had Stopped covering the
		// branches it promises to cover — these close that gap. All of them
		// return "no more work" while state stays MbOwned (Run's
		// AbandonOwnership defer ends the era shortly after), so they must
		// still satisfy the no-stale-handle half of the invariant.
		{
			name: "DrainAfterCancel/hard-Stopped refuses and clears the handle",
			run: func(t *testing.T, mb *Mailbox) {
				repl := agentcall.SessionAgentCall{SessionID: "s1"}
				mb.Replacement = &repl
				mb.HardStop()
				_, ok := mb.DrainAfterCancel()
				require.False(t, ok, "a hard-Stopped Mailbox must not hand the turn loop more work")
			},
			want: expectation{stillOwned: true},
		},
		{
			name: "DrainOrReleaseFinal/hard-Stopped ends the era and still releases",
			run: func(t *testing.T, mb *Mailbox) {
				mb.Submitted = []agentcall.SessionAgentCall{{SessionID: "s1"}}
				mb.HardStop()
				released := false
				_, hasNext, orphaned, err := mb.DrainOrReleaseFinal(1, func() error {
					released = true
					return nil
				})
				require.NoError(t, err)
				require.False(t, hasNext, "a hard-Stopped Mailbox must not continue into another turn")
				require.True(t, released,
					"shutdown must still release the OS lock — refusing to continue is not a reason to leak it")
				require.NotEmpty(t, orphaned, "queued work must be handed out as orphaned on a Stopped Mailbox — "+
					"since #646/#340 it is durably enqueued by the caller, not discarded")
			},
			want: expectation{stillOwned: false},
		},
		// InterruptAndReplace is deliberately NOT in this table. The
		// invariant here is about operations that hand work back to a turn
		// loop or end an era; InterruptAndReplace's refusal path is a pure
		// early return that mutates nothing, so the blanket "current.cancel
		// must be nil" post-condition does not apply to it (and asserting it
		// would just force a pointless write). Its Stopped behaviour is
		// covered directly by TestInterruptAndReplace_RefusesOnStoppedMailbox.
		//
		// #307 (P1-2 follow-up) update: InterruptAndReplace's OWNED branch
		// (state == MbOwned && !Stopped) is also not added here, even though
		// it is no longer a pure early return — it records
		// mb.Replacement and returns mb.Current.Cancel as-is (nil or
		// non-nil, whichever it already was). It still never WRITES to
		// state/current.cancel/dispatcherCancel, the three fields this
		// table's postcondition governs, so there is nothing for the
		// blanket "current.cancel must be nil" (or the MbOwned/MbIdle
		// dichotomy) to assert about it: it neither hands work back to a
		// turn loop (that's ReclaimReplacementOrKeep's job, below) nor ends
		// an era. Covered directly instead by
		// TestMailbox_InterruptAndReplace_OwnedButNoLiveGeneration_RecordsReplacementWithoutDispatcherFallback
		// and TestMailbox_InterruptAndReplace_OwnedRecordsReplacementAndReturnsCurrentCancel
		// (mailbox_test.go), which assert its actual postcondition:
		// mb.Replacement is set, and the returned cancel is exactly
		// mb.Current.Cancel (never a dispatcherCancel fallback — that
		// fallback is precisely what #307 removed, since dispatcherCancel
		// is runCancel and firing it poisons every future turn's context,
		// not just the possibly-nonexistent current Generation).
		//
		// ReclaimReplacementOrKeep is likewise NOT in this table, for the
		// same reason InjectIfBusy already isn't (see that method's own
		// doc): it does not change ownership state at all — state, current,
		// and dispatcherCancel are all left untouched. Its job is choosing
		// WHICH agentcall.SessionAgentCall the next Generation runs (the loop's stale
		// `call` vs. a same-window mb.Replacement), not deciding whether
		// the era continues or ends — that decision was already made by
		// whichever drain call produced `call` in the first place. On the
		// Replacement-hit branch it also pushes the pre-empted `call` back
		// onto the FRONT of mb.Submitted (closing review: an earlier draft
		// discarded it instead, the #283/P0-2 class of bug — "interrupt
		// deletes the very message it's supposed to Queue behind"), but
		// mb.Submitted's contents are not part of this table's ownership-
		// state postcondition either. Covered directly by
		// TestMailbox_ReclaimReplacementOrKeep_* (mailbox_test.go), including
		// the multi-element-Queue case that specifically catches the discard
		// defect.

		// #296/P1-C, corrected per #297 review: DrainOrReleaseFinal passes
		// THROUGH MbReleasing on its way to MbIdle — it NEVER re-emerges as
		// MbOwned once release() has been invoked, no matter what lands in
		// mb.Submitted/mb.Replacement during the window (see this function's
		// own doc: release() has already run, so there is no OS lock left to
		// keep a turn loop going under — an earlier draft of this fix handed
		// such work back as hasNext==true/MbOwned, which meant the turn loop
		// kept running turns with the session's OS lock already released,
		// letting a second process acquire the same lock concurrently).
		//
		// These three rows assert the CORRECTED contract: work that races
		// into the Mailbox during the release() window always ends up in the
		// `orphaned` return value with the era ending at MbIdle, EXCEPT when
		// HardStop also landed in that same window, in which case it is
		// discarded (not orphaned) per HardStop's existing contract.
		{
			name: "DrainOrReleaseFinal/submitted lands during the release() window: orphaned, era still ends",
			run: func(t *testing.T, mb *Mailbox) {
				release := func() error {
					mb.Mu.Lock()
					mb.Submitted = append(mb.Submitted, agentcall.SessionAgentCall{SessionID: "s1", Prompt: "raced in"})
					mb.Mu.Unlock()
					return nil
				}
				_, hasNext, orphaned, err := mb.DrainOrReleaseFinal(1, release)
				require.NoError(t, err)
				require.False(t, hasNext, "work that lands in mb.Submitted while release() is running must NOT "+
					"be handed back to the turn loop — the OS lock is already gone by the time it is noticed")
				require.Equal(t, []agentcall.SessionAgentCall{{SessionID: "s1", Prompt: "raced in"}}, orphaned,
					"the racing call must be returned as orphaned so the caller can restart it under a fresh lock")
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "DrainOrReleaseFinal/Replacement lands during the release() window: orphaned, era still ends",
			run: func(t *testing.T, mb *Mailbox) {
				release := func() error {
					mb.Mu.Lock()
					repl := agentcall.SessionAgentCall{SessionID: "s1", Prompt: "raced in as Replacement"}
					mb.Replacement = &repl
					mb.Mu.Unlock()
					return nil
				}
				_, hasNext, orphaned, err := mb.DrainOrReleaseFinal(1, release)
				require.NoError(t, err)
				require.False(t, hasNext, "a Replacement recorded while release() is running must NOT be handed "+
					"back to the turn loop — the OS lock is already gone by the time it is noticed")
				require.Equal(t, []agentcall.SessionAgentCall{{SessionID: "s1", Prompt: "raced in as Replacement"}}, orphaned,
					"the racing Replacement must be returned as orphaned so the caller can restart it under a fresh lock")
			},
			want: expectation{stillOwned: false},
		},
		{
			name: "DrainOrReleaseFinal/hard-Stopped during the release() window hands the race out as orphaned",
			run: func(t *testing.T, mb *Mailbox) {
				release := func() error {
					mb.HardStop()
					mb.Mu.Lock()
					mb.Submitted = append(mb.Submitted, agentcall.SessionAgentCall{SessionID: "s1"})
					mb.Mu.Unlock()
					return nil
				}
				_, hasNext, orphaned, err := mb.DrainOrReleaseFinal(1, release)
				require.NoError(t, err)
				require.False(t, hasNext, "HardStop landing during the release() window must override any work "+
					"that also landed there — a shutdown must not hand the turn loop a fresh call")
				require.Equal(t, []agentcall.SessionAgentCall{{SessionID: "s1"}}, orphaned, "work that races in alongside a shutdown must be "+
					"returned as orphaned so the caller durably enqueues it (#646) — a DB row, not a fresh provider turn")
			},
			want: expectation{stillOwned: false},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mb := newFixture()
			c.run(t, mb)

			if c.want.stillOwned {
				require.Equal(t, MbOwned, mb.State, "expected ownership to continue (state == MbOwned)")
				require.NotNil(t, mb.DispatcherCancel, "dispatcherCancel must stay live while ownership continues "+
					"— Cancel()'s fallback depends on it")
			} else {
				require.Equal(t, MbIdle, mb.State, "expected the ownership era to end (state == MbIdle)")
				require.Nil(t, mb.DispatcherCancel, "dispatcherCancel must be cleared once the era ends")
			}

			// THE invariant every one of the four found bugs violated, on one
			// branch at a time: current.cancel must never survive this call
			// still pointing at the fixture's original (already-spent, in
			// production terms) cancel func. A future branch added to any of
			// these methods that forgets this clear will fail HERE, without
			// needing a fifth review round to notice it by hand.
			require.Nil(t, mb.Current.Cancel, "current.cancel must never survive a Mailbox mutator call as a stale "+
				"handle — this is the exact postcondition rounds 9-13 kept finding violated one branch at a time; "+
				"a non-nil value here means Cancel()/InterruptAndReplace() would silently invoke a spent Generation's "+
				"cancel func instead of ever reaching the dispatcherCancel fallback")
		})
	}
}

// TestMailbox_BeginCompact_Postconditions verifies the postconditions of
// BeginCompact — the atomic acquisition method added by #268/P0-4 for
// compaction ownership. Unlike the drain/release/abandon operations in the
// main invariant table (whose postcondition is current.cancel == nil),
// BeginCompact STARTS an era: its postcondition is the opposite —
// current.cancel must be freshly set (non-nil), state must be MbOwned, and
// dispatcherCancel must be set. It is deliberately in a separate test
// function rather than the stale-handle table because the assertion is
// different.
func TestMailbox_BeginCompact_Postconditions(t *testing.T) {
	t.Run("acquires idle Mailbox", func(t *testing.T) {
		mb := &Mailbox{
			State: MbIdle,
		}
		myCancel := func() {}
		Epoch, ok := mb.BeginCompact(myCancel)
		require.True(t, ok, "BeginCompact on an idle Mailbox must succeed")
		require.NotZero(t, Epoch, "Epoch must be non-zero after acquisition")
		require.Equal(t, MbOwned, mb.State, "state must be MbOwned after acquisition")
		require.NotNil(t, mb.Current.Cancel, "current.cancel must be set after acquisition "+
			"— Cancel(sessionID) targets it instead of the old synthetic key")
		require.NotNil(t, mb.DispatcherCancel, "dispatcherCancel must be set after acquisition "+
			"— Cancel()'s fallback depends on it")
	})

	t.Run("refuses owned Mailbox", func(t *testing.T) {
		existingCancel := func() {}
		mb := &Mailbox{
			State:            MbOwned,
			Epoch:            5,
			DispatcherCancel: existingCancel,
			Current:          Generation{ID: 5, Cancel: existingCancel},
		}
		_, ok := mb.BeginCompact(func() {})
		require.False(t, ok, "BeginCompact on an owned Mailbox must fail")
		require.Equal(t, MbOwned, mb.State, "state must be unchanged")
		require.Equal(t, uint64(5), mb.Epoch, "Epoch must be unchanged")
		require.NotNil(t, mb.Current.Cancel, "current.cancel must be unchanged (non-nil)")
		require.NotNil(t, mb.DispatcherCancel, "dispatcherCancel must be unchanged (non-nil)")
	})

	t.Run("refuses Stopped Mailbox", func(t *testing.T) {
		mb := &Mailbox{
			State:   MbIdle,
			Stopped: true,
		}
		_, ok := mb.BeginCompact(func() {})
		require.False(t, ok, "BeginCompact on a Stopped Mailbox must fail")
		require.Equal(t, MbIdle, mb.State, "state must be unchanged")
		require.Nil(t, mb.Current.Cancel, "current.cancel must be unchanged (nil)")
	})

	t.Run("two BeginCompact calls are mutually exclusive", func(t *testing.T) {
		mb := &Mailbox{State: MbIdle}
		_, ok1 := mb.BeginCompact(func() {})
		require.True(t, ok1, "first BeginCompact must succeed")
		_, ok2 := mb.BeginCompact(func() {})
		require.False(t, ok2, "second BeginCompact on the same Mailbox must fail "+
			"— this is the core P0-4 fix: two concurrent compactions are impossible")
	})
}
