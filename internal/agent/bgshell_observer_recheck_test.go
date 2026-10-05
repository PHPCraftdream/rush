package agent

// The claim-time observer in claimBackgroundShellRow (bgshell_claim.go) is the
// ONLY completion callback a sync-bash background shell has when
// NotifyOnBackgroundJobDone is false. When such a shell is owned by a
// delegated child and exits AFTER the child's turn ended, only the observer
// can end the shell's completion hold (MarkCompletionRecorded) and drive the
// delegation re-check (noteSubAgentChildRunEnded); without them the parent's
// parked delegation waits for the 60s RecheckPass. This file proves the
// release without any RecheckPass or manual trigger, mirroring
// session_idle_trigger_test.go's real-driver fixture.

import (
	"context"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

func TestAsyncTool_BGShellObserverReleasesParkedDelegationWithoutNotifier(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	const parentSession = "bgshell-observer-parent"
	const parentCall = "call-bgshell-observer"

	child, err := env.sessions.Create(t.Context(), "bgshell-observer-child")
	require.NoError(t, err)
	childSession := child.ID

	delivered := make(chan AsyncCompletion, 4)
	coord := &coordinator{}
	coord.cfg = config.NewLibraryStore(&config.Config{Options: &config.Options{NotifyOnBackgroundJobDone: boolPtr(false)}}, t.TempDir())
	coord.asyncJobs = newWorkLedger(func(c AsyncCompletion) { delivered <- c })
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord
	mgr := shell.NewBackgroundShellManager()
	t.Cleanup(func() { mgr.KillAll(context.Background()) })
	coord.background = mgr
	coord.subAgentDrivers = newSubAgentDriverRegistry()

	// A real driver holds the child's mailbox busy while the shell starts and
	// the delegation arms (the session_idle_trigger_test.go fixture shape).
	model := newBlockUntilReleasedModel()
	driver := NewSessionAgent(SessionAgentOptions{
		SmartModel: Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:  Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		IsYolo:     true,
		Sessions:   env.sessions,
		Messages:   env.messages,
		AsyncJobs:  coord.asyncJobs,
		// The exact wiring buildAgent does for every driver it builds.
		OnSessionIdle: coord.noteSubAgentChildRunEnded,
	})
	coord.subAgentDrivers.register(childSession, subAgentDriver{agent: driver, parentSessionID: parentSession})

	runDone := make(chan struct{})
	var runErr error
	go func() {
		defer close(runDone)
		_, runErr = driver.Run(context.Background(), SessionAgentCall{
			SessionID:           childSession,
			Prompt:              "resume",
			AutoResumed:         true,
			BackgroundJobNotice: true,
		})
	}()
	require.Eventually(t, func() bool {
		select {
		case <-runDone:
			t.Fatalf("driver.Run returned early, err=%v", runErr)
		default:
		}
		return driver.IsSessionBusy(childSession)
	}, 2*time.Second, 5*time.Millisecond, "the child's real turn must be mid-flight before the shell starts")

	// The child owns a still-running background shell, claimed through the
	// sync bash escape so the claim-time observer is registered. No notifier
	// callback exists: NotifyOnBackgroundJobDone is false.
	sh, err := mgr.StartOwned(t.Context(), childSession, t.TempDir(), nil, "sleep 30", "child-owned shell")
	require.NoError(t, err)
	bgShellEscapeFor(t, coord, childSession, sh.ID)

	// Park the delegation while the child is busy: trigger (i) must not
	// release it.
	_, _, err = coord.asyncJobs.Start(parentSession, parentCall, "", AgentToolName, childSession, false, false, nil, func() {})
	require.NoError(t, err)
	coord.asyncJobs.acknowledged(jobOf(coord.asyncJobs, parentSession, parentCall))
	coord.asyncJobs.armDelegation(jobOf(coord.asyncJobs, parentSession, parentCall), jobResult{content: "child yielded: shell still running"})
	require.Empty(t, drainCompletions(delivered), "the delegation must not be delivered while the real driver is busy")

	// End the child's turn. Its turn-end recheck (onSessionIdle) must NOT
	// release the delegation: the shell is still running.
	model.unblock()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the driver's real turn did not finish")
	}
	require.Empty(t, drainCompletions(delivered), "the delegation must stay parked while the child's shell runs")

	// The shell exits only now, after the child's turn ended. The observer is
	// the ONLY completion callback, so the release must come from it: no
	// RecheckPass is driven anywhere in this test after this point.
	require.NoError(t, mgr.KillOwned(context.Background(), childSession, sh.ID))
	sh.Wait()

	// REVERT CHECK: removing MarkCompletionRecorded + noteSubAgentChildRunEnded
	// from claimBackgroundShellRow's OnDone observer leaves the delegation
	// parked here (row committed, but no recheck fires) -- observed failure:
	// this select hits its timeout with "the delegation stayed parked".
	select {
	case got := <-delivered:
		require.Equal(t, parentCall, got.ToolCallID)
	case <-time.After(5 * time.Second):
		t.Fatal("the delegation stayed parked after the shell exited; the claim-time observer must release it (hold + recheck) without any RecheckPass")
	}
	require.False(t, coord.asyncJobs.hasParked())
	row, err := coord.asyncJobs.store.Get(context.Background(), parentSession, parentCall)
	require.NoError(t, err)
	require.NotEqual(t, "running", row.State, "the delegation row must have left running")
	shellRow, err := coord.asyncJobs.store.Get(context.Background(), childSession, sh.ID)
	require.NoError(t, err)
	// KillOwned ends the process non-zero, so the observer records failed.
	require.Equal(t, "failed", shellRow.State, "the observer must have committed the shell's row")
	require.Zero(t, mgr.PendingCompletionsOwned(childSession), "the observer must have ended the shell's completion hold")
}

// bgShellEscapeFor runs a SYNC-branch bash call for sessionID whose inner tool
// answers with the background-escape metadata of an already started shell
// (bgShellEscape's parameterized twin: the escape must be claimable for a
// delegated child session, not only the fixed "session" fixture id).
func bgShellEscapeFor(t *testing.T, coord *coordinator, sessionID, shellID string) {
	t.Helper()
	wrapped := &asyncTool{inner: bgShellInnerTool(shellID), coordinator: coord, name: tools.BashToolName}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, sessionID)
	ctx = WithCallOrigin(ctx, message.OriginSDK)
	resp, err := wrapped.Run(ctx, fantasy.ToolCall{ID: "call-" + shellID, Name: tools.BashToolName, Input: `{"command":"x","run_in_background":true}`})
	require.NoError(t, err)
	require.False(t, resp.IsError)
}

// With notifications ON the observer must NOT end the hold: the notifier owns
// the notice, and a recheck that ran before it is saved would release the
// delegation too early (R7B-1). The notifier is parked at the arrival seam
// while the observer finishes the row; the hold has to stay until the
// notifier returns. "On" is the default (no config, no options, an unset
// pointer) as well as an explicit true.
//
// REVERT CHECK: releasing the hold in the observer regardless of the
// notification setting makes PendingCompletionsOwned drop to 0 while the
// notifier is still parked; a helper that read the default as "off" does the
// same for the config cases.
func TestAsyncTool_BGShellObserverKeepsTheHoldWhileTheNotifierIsPending(t *testing.T) {
	// bgArrivalEnteredSeam is a package-level pointer: no t.Parallel.
	cases := []struct {
		name string
		opts *config.Options // nil: no config store at all
	}{
		{name: "no config", opts: nil},
		{name: "unset option", opts: &config.Options{}},
		{name: "explicit true", opts: &config.Options{NotifyOnBackgroundJobDone: boolPtr(true)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coord, store, mgr, dir := newBGShellFixture(t)
			if tc.opts != nil {
				coord.cfg = config.NewLibraryStore(&config.Config{Options: tc.opts}, t.TempDir())
			}
			sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "echo hold", "notifier pending")
			require.NoError(t, err)

			entered := make(chan struct{})
			release := make(chan struct{})
			seam := func(id string) {
				if id != "session" {
					return
				}
				close(entered)
				<-release
			}
			bgArrivalEnteredSeam.Store(&seam)
			t.Cleanup(func() { bgArrivalEnteredSeam.Store(nil) })
			sh.OnDone(func() { coord.persistBGShellCompletion("session", sh.ID, "notifier summary") })

			bgShellEscape(t, coord, sh.ID)
			sh.Wait()

			require.Eventually(t, func() bool {
				select {
				case <-entered:
				default:
					return false
				}
				row, err := store.Get(context.Background(), "session", sh.ID)
				return err == nil && row.State == "completed"
			}, 10*time.Second, 10*time.Millisecond, "the notifier is parked and the observer closed the row")
			require.Positive(t, mgr.PendingCompletionsOwned("session"),
				"the observer must leave the hold to the pending notifier")

			close(release)
			require.Eventually(t, func() bool { return mgr.PendingCompletionsOwned("session") == 0 },
				10*time.Second, 10*time.Millisecond, "the hold ends once the notifier returned")
		})
	}
}
