// R6B-1: a background-shell completion in a delegated CHILD whose delegation
// row still runs must start the child's Drain whatever the auto-resume claim
// says. The claim (web only, AutoResumeOnJobDone, a free slot) bounds a
// session's OWN chain of automatic turns; a child is driven by its delegation,
// so without a wake its debt stayed DrainOwed with nothing to launch it and the
// parent's delegation hung. drainPermitted stays the single launch gate, and a
// ROOT session keeps the cap and the auto-resume policy.
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// childBGFixture is a parent delegating to a child whose first turn ended
// while a background shell of its own still runs (the delegation is parked).
type childBGFixture struct {
	*attemptFixture
	parentID, childID string
	delivered         chan AsyncCompletion
	runs              *countingAgent
	held              *shell.BackgroundShell
	mgr               *shell.BackgroundShellManager
}

type childBGMode struct {
	persistent bool // the web process
	autoResume bool // AutoResumeOnJobDone
	spentSlots int  // slots already spent on the child since its last human message
	noIdle     bool // leave OnSessionIdle unwired (no release hook)
}

// newChildBGFixture must not be parallel (OAuth fixture isolates global paths).
func newChildBGFixture(t *testing.T, m childBGMode) *childBGFixture {
	t.Helper()
	f, parentID, childID, delivered := childDelegationBase(t, attemptFixtureOpts{
		noIdle: m.noIdle, oauthProvider: oauthTestProvider,
	})
	f.rotateCredentials(oauthTestProvider)
	f.coord.cfg.Config().Options.AutoResumeOnJobDone = boolPtr(m.autoResume)
	if m.persistent {
		f.coord.SetPersistentMode(true)
		t.Cleanup(f.coord.StopRecheckTicker)
	}
	for range m.spentSlots {
		f.coord.bumpConsecutiveResume(childID)
		f.coord.bumpConsecutiveResume(parentID)
	}
	runs := &countingAgent{SessionAgent: f.sa}
	f.coord.subAgentDrivers.register(childID, subAgentDriver{
		agent: runs, call: SessionAgentCall{SessionID: childID}, parentSessionID: parentID,
	})

	mgr := shell.NewBackgroundShellManager()
	t.Cleanup(func() { mgr.Close(context.Background()) })
	f.coord.background = mgr
	held, err := mgr.StartOwned(t.Context(), childID, f.env.workingDir, nil, "sleep 30", "held")
	require.NoError(t, err)
	return &childBGFixture{
		attemptFixture: f, parentID: parentID, childID: childID, delivered: delivered,
		runs: runs, held: held, mgr: mgr,
	}
}

// park ends the child's first turn: its own shell still runs, so the
// delegation stays armed.
func (c *childBGFixture) park() {
	c.t.Helper()
	c.ledger.armDelegation(jobOf(c.ledger, c.parentID, "delegate-1"), jobResult{content: "first turn text"})
	require.True(c.t, c.ledger.hasParked(), "the child's running shell keeps the delegation armed")
	require.Empty(c.t, c.delivered)
}

// finishShell is the shell's completion as the bash tool reports it.
func (c *childBGFixture) finishShell() {
	c.t.Helper()
	require.NoError(c.t, c.mgr.Kill(c.t.Context(), c.held.ID))
	require.True(c.t, c.held.WaitContext(c.t.Context()))
	c.coord.notifyBackgroundJobDone(c.childID, c.held)
	c.coord.waitRecheckWakes()
}

func (c *childBGFixture) awaitRelease() AsyncCompletion {
	c.t.Helper()
	select {
	case got := <-c.delivered:
		return got
	case <-time.After(10 * time.Second):
		scope, err := c.coord.CLIScope(context.Background(), c.childID)
		c.t.Fatalf("the delegation never released (child scope %+v, err %v, Drain runs %d)", scope, err, c.runs.runs.Load())
		return AsyncCompletion{}
	}
}

// TestBGShellDone_DelegatedChildIsWokenWhateverTheClaimSays: the child's shell
// finishes while the delegation is parked; the claim is refused (web with
// auto-resume off; web with every slot spent; CLI, where persistentMode is
// false) yet ONE Drain reaches the provider and the delegation releases.
// Before the fix: no request, CLIScope(child).Drain stayed DrainOwed and the
// delegation never released.
//
// Revert-check: dropping the delegated-child wake from notifyBackgroundJobDone
// (launching on the claim alone) turns every case red on the release timeout.
func TestBGShellDone_DelegatedChildIsWokenWhateverTheClaimSays(t *testing.T) {
	cases := []struct {
		name string
		mode childBGMode
	}{
		{"web, auto-resume off", childBGMode{persistent: true}},
		{"web, every slot spent", childBGMode{persistent: true, autoResume: true, spentSlots: maxConsecutiveAutoResumes}},
		{"CLI, persistentMode false", childBGMode{}},
		{"CLI, no release hook", childBGMode{noIdle: true}},
		{"web, a free slot (the claim launches, once)", childBGMode{persistent: true, autoResume: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newChildBGFixture(t, tc.mode)
			c.park()

			c.finishShell()

			got := c.awaitRelease()
			require.Equal(t, "delegate-1", got.ToolCallID)
			require.False(t, got.IsError)
			require.Equal(t, "reacted", got.Content, "the parent gets the child's reaction, not its stale first-turn text")
			require.EqualValues(t, 1, c.runs.runs.Load(), "one Drain: the wake, the release hook and the re-check never launch a second")
			require.EqualValues(t, 1, c.requests.Load())
			scope, err := c.coord.CLIScope(context.Background(), c.childID)
			require.NoError(t, err)
			require.NotEqual(t, DrainOwed, scope.Drain)
			require.False(t, c.ledger.hasParked())
		})
	}
}

// TestBGShellDone_ParentOfARunningDelegationKeepsTheCap: the wake exception is
// for the CHILD only. A session that merely OWNS a running delegation is a
// root: once its slots are spent (or auto-resume is off) its completion
// launches nothing, and a re-check does not launch it either.
//
// Revert-check: making the child check true for any session with a running
// delegation row (owner or child) launches a Drain for the parent and turns
// this red.
func TestBGShellDone_ParentOfARunningDelegationKeepsTheCap(t *testing.T) {
	cases := []struct {
		name string
		mode childBGMode
	}{
		{"every slot spent", childBGMode{persistent: true, autoResume: true, spentSlots: maxConsecutiveAutoResumes}},
		{"auto-resume off", childBGMode{persistent: true}},
		{"CLI", childBGMode{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newChildBGFixture(t, tc.mode)
			rootRuns := &countingAgent{SessionAgent: c.sa}
			c.coord.currentAgent = rootRuns
			running, err := c.ledger.hasRunningDelegationFor(ctx, c.childID)
			require.NoError(t, err)
			require.True(t, running, "the parent owns a running delegation")

			sh, err := c.mgr.StartOwned(ctx, c.parentID, c.env.workingDir, nil, "echo done", "root shell")
			require.NoError(t, err)
			require.True(t, sh.WaitContext(ctx))
			c.coord.notifyBackgroundJobDone(c.parentID, sh)
			c.coord.waitRecheckWakes()

			require.Zero(t, rootRuns.runs.Load(), "a root's completion over the cap or with auto-resume off launches no Drain")
			require.NoError(t, c.coord.wakeSession(ctx, c.parentID, false))
			c.pass(ctx)
			require.Zero(t, rootRuns.runs.Load(), "nor does a re-check")
		})
	}
}

// TestDelegatedChildDriven_ReadsTheDurableRow: "a child whose delegation runs"
// is the durable row, not this process's memory: a row another process claimed
// counts with no ledger job and no driver here; without a running row a
// registered driver alone does not (the released child's Drain would be
// refused by the policy anyway), and an unreadable row falls back to the
// driver, so the wake goes through the policy instead of being lost.
//
// Revert-check: answering from the in-memory byChild index (or the driver
// registry) instead of HasRunningDelegationFor turns the first case red.
func TestDelegatedChildDriven_ReadsTheDurableRow(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "durable-child", attemptFixtureOpts{noDriver: true})
	child, err := f.env.sessions.Create(ctx, "child")
	require.NoError(t, err)

	driven, err := f.coord.delegatedChildDriven(ctx, child.ID)
	require.NoError(t, err)
	require.False(t, driven, "no delegation names the session")

	// A delegation another process claimed: only the row exists.
	_, err = f.store.Claim(ctx, session.ClaimParams{
		Owner: f.sessID, ToolCallID: "remote-delegate", Kind: session.JobKindAgent, Input: "x", ToolName: AgentToolName, ChildSessionID: child.ID,
	})
	require.NoError(t, err)
	driven, err = f.coord.delegatedChildDriven(ctx, child.ID)
	require.NoError(t, err)
	require.True(t, driven, "a running row on the store is enough")

	// An unreadable row: fall back to the driver registration.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	driven, err = f.coord.delegatedChildDriven(cancelled, "other-session")
	require.Error(t, err)
	require.False(t, driven, "no row readable and no driver: not driven")
	f.coord.subAgentDrivers.register("other-session", subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: "other-session"}})
	driven, err = f.coord.delegatedChildDriven(cancelled, "other-session")
	require.Error(t, err)
	require.True(t, driven, "a registered driver stands in for an unreadable row")
}
