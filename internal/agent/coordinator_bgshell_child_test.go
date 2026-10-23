// R6B-1: a background-shell completion in a delegated CHILD whose delegation
// row still runs must start the child's Drain whatever the auto-resume claim
// says. The claim (web only, AutoResumeOnJobDone, a free slot) bounds a
// session's OWN chain of automatic turns; a child is driven by its delegation,
// so without a wake its debt stayed DrainOwed with nothing to launch it and the
// parent's delegation hung. drainPermitted stays the single launch gate, and a
// ROOT session keeps the cap and the auto-resume policy.
//
// Ledger-era halves deleted with the workLedger park: the hasParked armed-
// result assertion and the delivered-channel content match ("the parent gets
// the child's reaction") -- batch delegation delivery is the batch executor's
// (see coordinator_child_question_test.go's file doc). What remains pinned
// here is the WAKE contract: one Drain whatever the claim says, and the cap
// for a root that merely owns a running delegation.
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// childBGFixture is a parent delegating to a child whose live delegation link
// is seeded in batch_nodes and whose first turn ended while a background shell
// of its own still runs.
type childBGFixture struct {
	*attemptFixture
	parentID, childID string
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
	f, parentID, childID := childDelegationBase(t, attemptFixtureOpts{
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
	held, err := mgr.StartOwned(t.Context(), childID, f.env.workingDir, nil, bgHoldCommand(bgGateName), "held")
	require.NoError(t, err)
	return &childBGFixture{
		attemptFixture: f, parentID: parentID, childID: childID,
		runs: runs, held: held, mgr: mgr,
	}
}

// park ends the child's first turn: its own pending wake-armed debt is seeded
// and the delegation row is live in the batch store. (The deleted ledger half
// asserted hasParked; the durable batch rows are the only representation now.)
func (c *childBGFixture) park() {
	c.t.Helper()
	seedFixtureJobDebt(c.t, c.env.conn, c.childID, "bg-call", false)
	view := readFixtureDelegation(c.t, c.env.conn, c.parentID, "delegate-1")
	require.Equal(c.t, "running", view.State, "the delegation runs in the batch store")
	require.Equal(c.t, c.childID, view.ChildSessionID)
}

// finishShell is the shell's completion as the bash tool reports it. The
// shell ends NATURALLY (the gate file appears); a manager Kill would take the
// shell out of the manager, and absence-means-cancelled (bgShellRowCancelled)
// would then swallow the completion notice — cancelled shells never wake.
func (c *childBGFixture) finishShell() {
	c.t.Helper()
	releaseBGShellGate(c.t, c.env.workingDir)
	require.True(c.t, c.held.WaitContext(c.t.Context()))
	c.coord.notifyBackgroundJobDone(c.childID, c.held)
	c.coord.waitRecheckWakes()
}

// awaitRelease waits for the release's observable effect (the child's one
// Drain ran). The deleted ledger-era implementation read the onWebDone
// delivery channel; batch delivery is the batch executor's.
func (c *childBGFixture) awaitRelease() AsyncCompletion {
	c.t.Helper()
	require.Eventually(c.t, func() bool { return c.runs.runs.Load() >= 1 },
		10*time.Second, 10*time.Millisecond,
		"the delegation never released (the child's Drain never ran)")
	return AsyncCompletion{ToolCallID: "delegate-1"}
}

// TestBGShellDone_DelegatedChildIsWokenWhateverTheClaimSays: the child's shell
// finishes while its debt is pending; the claim is refused (web with
// auto-resume off; web with every slot spent; CLI, where persistentMode is
// false) yet ONE Drain reaches the provider. Before the fix: no request and
// CLIScope(child).Drain stayed DrainOwed. (The deleted ledger-era content
// assertion on the parent's delivery channel is replaced by the drain count
// and the post-release scope read.)
//
// Revert-check: dropping the delegated-child wake from notifyBackgroundJobDone
// (launching on the claim alone) turns the four refused-claim cases red on the
// drain count; "a free slot" launches through the claim and stays green (the
// control that the claim path itself still works).
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

			require.Eventually(c.t, func() bool { return c.runs.runs.Load() == 1 },
				10*time.Second, 10*time.Millisecond,
				"one Drain: the wake, the release hook and the re-check never launch a second")
			require.Equal(c.t, int32(1), c.requests.Load(),
				"the one Drain reached the provider exactly once")
			scope, err := c.coord.CLIScope(context.Background(), c.childID)
			require.NoError(c.t, err)
			require.NotEqual(c.t, DrainOwed, scope.Drain)
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
			running, err := coordinatorBatchCutoverFacts(c.coord).LiveDelegationChildOf(ctx, c.parentID, c.childID)
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
// is the durable batch row, not this process's memory: a live delegation
// seeded by another process counts with no driver here; without a running
// delegation a registered driver alone does not (the released child's Drain
// would be refused by the policy anyway), and an unreadable read falls back to
// the driver, so the wake goes through the policy instead of being lost.
//
// Revert-check: answering from the in-memory byChild index (or the driver
// registry) instead of LiveDelegationChildOf turns the first case red.
func TestDelegatedChildDriven_ReadsTheDurableRow(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "durable-child", attemptFixtureOpts{noDriver: true})
	// A delegated child is a task session durably linked to its parent: the
	// durable read resolves the parent from the child's own row.
	child, err := f.env.sessions.CreateTaskSession(ctx, "remote-delegate", f.sessID, "child")
	require.NoError(t, err)

	driven, err := f.coord.delegatedChildDriven(ctx, child.ID)
	require.NoError(t, err)
	require.False(t, driven, "no delegation names the session")

	// A live delegation another process seeded: only the batch rows exist.
	seedBatchDelegation(t, f.env.conn, f.sessID, "remote-delegate", child.ID, "agent", "running", false)
	driven, err = f.coord.delegatedChildDriven(ctx, child.ID)
	require.NoError(t, err)
	require.True(t, driven, "a live batch delegation is enough")

	// An unreadable read: fall back to the driver registration.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	driven, err = f.coord.delegatedChildDriven(cancelled, "other-session")
	require.Error(t, err)
	require.False(t, driven, "no delegation readable and no driver: not driven")
	f.coord.subAgentDrivers.register("other-session", subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: "other-session"}})
	driven, err = f.coord.delegatedChildDriven(cancelled, "other-session")
	require.Error(t, err)
	require.True(t, driven, "a registered driver stands in for an unreadable read")
}
