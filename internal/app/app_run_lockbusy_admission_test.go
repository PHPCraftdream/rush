// #1101 (backlog ox, Q1/Q2a): the first turn's preparatory session writes
// (ClearCancelRequest, SetBudget, SetEndedReason("")) run at ADMISSION —
// runOwned invokes them once the mailbox and the inter-process session lock
// are ours — so a first turn refused by another process's lock leaves the
// session's state untouched, and an admitted turn applies them.
package app

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestRunNonInteractive_FirstTurnLockBusy_LeavesSessionStateUntouched: a
// first turn refused by the session lock of another process must not clear
// the owner's unread cancel request, must not rewrite the budget, and must
// not clear ended_reason (scenario Q1).
//
// Revert-check: moving the three writes back before admission (the old
// pre-handoff block in ExecuteRun) runs them on the refused first turn and
// every assertion below goes red.
func TestRunNonInteractive_FirstTurnLockBusy_LeavesSessionStateUntouched(t *testing.T) {
	application, sessionID, dataDir := newLockBusyCLITestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("provider must never be called: the lock refusal must be caught before any turn runs")
	})
	ctx := context.Background()
	require.NoError(t, application.Sessions.RequestCancel(ctx, sessionID))
	require.NoError(t, application.Sessions.SetBudget(ctx, sessionID, 1.5, 200, 300))
	require.NoError(t, application.Sessions.SetEndedReason(ctx, sessionID, "previous-run"))

	foreignLock, err := session.TryAcquireSessionLock(dataDir, sessionID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreignLock.Release() })

	_, err = application.RunNonInteractiveWithResult(ctx, io.Discard, "hello",
		RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	var lockBusy *session.SessionLockBusyError
	require.ErrorAs(t, err, &lockBusy, "the refusal must surface as the real SessionLockBusyError")

	cancelled, err := application.Sessions.IsCancelRequested(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, cancelled, "the owner's unread cancel request survives the refusal")
	sess, err := application.Sessions.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 1.5, sess.BudgetMaxCost, "the budget is not rewritten by the refused turn")
	require.EqualValues(t, 200, sess.BudgetMaxTokens)
	require.EqualValues(t, 300, sess.BudgetTimeoutSec)
	require.Equal(t, "previous-run", sess.EndedReason, "ended_reason is not cleared by the refused turn")
}

// TestRunNonInteractive_AdmittedRunAppliesSetup: the same writes an ADMITTED
// first turn performs at admission — the stale cancel flag is spent and the
// budget is persisted (CLI path; the web path's flag spend is pinned by
// TestCancelFlag_HumanTurnClearsTheFlagAtAdmission in internal/agent).
//
// Revert-check: deleting the admission callback from ExecuteRun leaves the
// flag set and the budget at zero and the test goes red.
func TestRunNonInteractive_AdmittedRunAppliesSetup(t *testing.T) {
	application, sessionID, _ := newLockBusyCLITestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, sseChunk("ok"))
		_, _ = io.WriteString(w, sseStopChunk)
		_, _ = io.WriteString(w, sseDone)
	})
	ctx := context.Background()
	require.NoError(t, application.Sessions.RequestCancel(ctx, sessionID))

	_, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "hello",
		RunOverrides{Origin: message.OriginCLI, MaxCost: 2.5, MaxTokens: 111, Timeout: time.Minute}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)

	cancelled, err := application.Sessions.IsCancelRequested(ctx, sessionID)
	require.NoError(t, err)
	require.False(t, cancelled, "an admitted first turn spends the stale cancel request")
	sess, err := application.Sessions.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 2.5, sess.BudgetMaxCost, "an admitted first turn persists the budget")
	require.EqualValues(t, 111, sess.BudgetMaxTokens)
}
