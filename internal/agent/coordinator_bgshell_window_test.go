// R7B-1: a delegated child's background shell that has finished but whose
// bg_shell_done notice has not committed yet is still open work. The window
// runs from the shell's own "finished" observation to the durable insert in
// notifyBackgroundJobDone (which may wait on the process-wide bgArrival gate
// and the single writer connection); a delegation re-check landing in it saw
// no ledger job, no running shell and no debt, released the delegation with
// the first turn's stale text, and the shell's result reached nobody.
package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// windowCallback registers the completion callback the way the bash tool does
// (sh.OnDone) but lets the test freeze it before it enters
// notifyBackgroundJobDone; ended closes when the callback has returned.
func (c *childBGFixture) windowCallback(sh *shell.BackgroundShell, enter <-chan struct{}) (ended <-chan struct{}) {
	done := make(chan struct{})
	sh.OnDone(func() {
		defer close(done)
		<-enter
		c.coord.notifyBackgroundJobDone(c.childID, sh)
	})
	return done
}

// requireStillParked runs the re-check that used to release the delegation.
func (c *childBGFixture) requireStillParked(why string) {
	c.t.Helper()
	c.ledger.recheckChild(c.childID)
	require.True(c.t, c.ledger.hasParked(), why)
	require.Empty(c.t, c.delivered, why)
}

// TestChildScope_FinishedShellStaysOpenUntilItsNoticeCommits: the shell of a
// parked child finishes while its callback is (a) not yet entered, (b) waiting
// on the bgArrival gate. A re-check in either window keeps the delegation
// parked and delivers nothing; once the notice commits the child's Drain
// reacts and the parent gets "reacted", not "first turn text". A callback that
// already ran to completion holds nothing (the control).
//
// Revert-check: dropping the completion hold from childScopeDrained turns all
// three cases red (the two windows where the notice is not durable yet, and the
// natural completion in place).
func TestChildScope_FinishedShellStaysOpenUntilItsNoticeCommits(t *testing.T) {
	cases := []struct {
		name   string
		gate   bool // hold bgArrival so the entered callback blocks on it
		remove bool // the shell left the manager (job_kill) before finishing
	}{
		{"callback not entered yet", false, true},
		{"callback waiting on the gate", true, true},
		{"shell still in the manager (natural completion)", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newChildBGFixture(t, childBGMode{})
			c.park()

			sh := c.held
			if !tc.remove {
				// The held shell is dropped; a second one completes in place.
				require.NoError(t, c.mgr.Kill(ctx, c.held.ID))
				var err error
				sh, err = c.mgr.StartOwned(ctx, c.childID, c.env.workingDir, nil, "echo done", "quick")
				require.NoError(t, err)
			}
			enter := make(chan struct{})
			ended := c.windowCallback(sh, enter)
			if tc.gate {
				require.NoError(t, c.coord.bgArrival.lock(ctx))
			}
			if tc.remove {
				require.NoError(t, c.mgr.Kill(ctx, sh.ID))
			}
			require.True(t, sh.WaitContext(ctx))
			if !tc.remove {
				require.NotNil(t, mustGet(t, c.mgr, sh.ID), "a naturally finished shell stays in the manager")
			}
			require.Zero(t, c.mgr.ActiveOwned(c.childID), "the shell reports finished: the window is open")

			c.requireStillParked("the shell finished, its callback has not entered yet")
			entered := make(chan struct{}, 1)
			seam := func(id string) {
				if id == c.childID {
					select {
					case entered <- struct{}{}:
					default:
					}
				}
			}
			bgArrivalEnteredSeam.Store(&seam)
			t.Cleanup(func() { bgArrivalEnteredSeam.Store(nil) })
			close(enter)
			if tc.gate {
				<-entered // the callback is in persistBGShellCompletion; the gate is held, so nothing is written
				c.requireStillParked("the callback entered and waits on the gate: the notice is not written")
				c.coord.bgArrival.unlock()
			}
			<-ended
			c.coord.waitRecheckWakes()

			got := c.awaitRelease()
			require.Equal(t, "reacted", got.Content, "the parent gets the child's reaction, not its stale first-turn text")
			require.False(t, got.IsError)
			require.EqualValues(t, 1, c.runs.runs.Load(), "one Drain")
			require.False(t, c.ledger.hasParked())
			require.Zero(t, c.mgr.PendingCompletionsOwned(c.childID), "the hold is gone once the callback returned")
		})
	}
}

func mustGet(t *testing.T, m *shell.BackgroundShellManager, id string) *shell.BackgroundShell {
	t.Helper()
	sh, ok := m.Get(id)
	require.True(t, ok)
	return sh
}

// TestBGShellDone_OwnRecheckRunsAfterTheHoldDrops: the callback's own re-check
// is the only trigger left for a child whose delegation was stopped while its
// shell ran (nothing is parked, no Drain launches); it must run after the
// completion hold ends, or it still sees the finished shell as open work and
// the child's driver record lives on.
//
// Revert-check: dropping MarkCompletionRecorded from notifyBackgroundJobDone
// leaves the hold on during that re-check and the driver stays registered.
func TestBGShellDone_OwnRecheckRunsAfterTheHoldDrops(t *testing.T) {
	ctx := context.Background()
	c := newChildBGFixture(t, childBGMode{noIdle: true})
	c.coord.currentAgent = &mockSessionAgent{}
	c.park()
	c.coord.Cancel(c.parentID) // the delegation is terminal; the running shell keeps the child's scope
	_, registered := c.coord.subAgentDrivers.get(c.childID)
	require.True(t, registered)

	enter := make(chan struct{})
	ended := c.windowCallback(c.held, enter)
	close(enter)
	require.NoError(t, c.mgr.Kill(ctx, c.held.ID))
	<-ended
	c.coord.waitRecheckWakes()

	_, registered = c.coord.subAgentDrivers.get(c.childID)
	require.False(t, registered, "the callback's own re-check released the child's driver")
	require.Zero(t, c.runs.runs.Load(), "no delegation drives the child: no Drain")
}
