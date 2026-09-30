// The web bg-shell auto-resume cap (R3B-6): exactly maxConsecutiveAutoResumes
// Drains per human message, spent once at admission (claimAutoResume) by the
// completion's own launch. A release or tick re-check of the same bg-shell-only
// debt compares against the cap WITHOUT spending it, so a shell that finishes
// while the last permitted Drain is still running cannot chain further Drains.
package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
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

// A13 (rewritten): six REAL completions through notifyBackgroundJobDone launch
// exactly five Drains; the release/tick re-check of the sixth completion's
// debt does not launch a sixth; a human message re-arms.
//
// Revert-check: dropping the cap comparison for non-fact launches from
// drainPolicy lets wakeSession(fact=false) launch the sixth Drain and this
// test goes red; re-adding the comparison to the fact path (the old "<" after
// the bump) refuses the fifth submission and turns it red too.
func TestBGShellCap_ExactlyFiveAutoResumes(t *testing.T) {
	ctx := context.Background()
	// A plain root session with a real config (drainCallFor builds its call
	// from it); the token is live, so nothing is refreshed.
	f := newAttemptFixture(t, "attempt-bgshell-cap", attemptFixtureOpts{
		noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
	})
	f.rotateCredentials(oauthTestProvider)
	f.coord.cfg.Config().Options.AutoResumeOnJobDone = boolPtr(true)
	f.coord.SetPersistentMode(true)
	t.Cleanup(f.coord.StopRecheckTicker)
	mock := &mockSessionAgent{}
	var launches atomic.Int32
	mock.runFunc = func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		launches.Add(1)
		return nil, nil
	}
	// A plain root session (no delegation driver): every launch runs on
	// currentAgent.
	f.coord.currentAgent = mock

	for range maxConsecutiveAutoResumes + 1 {
		f.coord.notifyBackgroundJobDone(f.sessID, finishedBackgroundShell(t, f.env.workingDir, f.sessID))
	}
	require.Eventually(t, func() bool { return launches.Load() == maxConsecutiveAutoResumes }, 10*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.EqualValues(t, maxConsecutiveAutoResumes, launches.Load(), "exactly five Drains per human message")

	// The release of a Drain and the 60s pass re-check the sixth completion's
	// still-owed debt: over the cap, they must not launch.
	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, false))
	f.pass(ctx)
	require.EqualValues(t, maxConsecutiveAutoResumes, launches.Load(), "a re-check must not spend past the cap")

	f.coord.ResetAutoResumeCounter(f.sessID)
	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, false))
	require.EqualValues(t, maxConsecutiveAutoResumes+1, launches.Load(), "a human message re-arms auto-resume")
}
