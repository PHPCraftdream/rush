package server

// R12-EF fix F2: the batch rerun (BatchRerunReconcile) must run the
// session_notices half of the legacy truncation too -- a delivered notice
// whose message landed in the deleted tail is re-pended for the new branch,
// and a wake_failed marker of the deleted branch is voided instead of
// re-pended. Driven through the REAL handleRerunMessage: real App, real DB,
// real truncation transaction; only the replacement turn is the usual mock
// coordinator (p630's cancellableHoldCoordinator).

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestHandleRerunMessage_BatchRependsSessionNotices reruns from the target
// user message while a delivered bg_shell_done completion and a delivered
// wake_failed marker both name the tail message the rerun deletes.
//
// REVERT CHECK (F2): remove the RependSessionNoticesByMessageIDs /
// VoidWakeFailedNoticesByMessageIDs statements from BatchRerunReconcile
// (internal/session/batch_history_rerun.go). The first failing assertion is
// the bg_shell_done delivery check: the notice stays delivery='done' with a
// dead message reference, so the new branch never re-delivers the completion
// -- exactly review finding F2.
func TestHandleRerunMessage_BatchRependsSessionNotices(t *testing.T) {
	f := newRerunHandlerFx(t, "batch-rerun-notices")
	ctx := t.Context()

	seedDelivered := func(kind, text string) int64 {
		t.Helper()
		now := time.Now().Unix()
		var id int64
		attempts, reacted := 0, 0
		if kind == session.NoticeKindBGShellDone {
			attempts, reacted = 2, 1
		}
		require.NoError(t, f.a.DB().QueryRowContext(ctx,
			`INSERT INTO session_notices (owner, kind, text, wake, delivery, wake_attempts, reacted, job_tool_call_id, notice_message_id, created_at, updated_at)
VALUES (?, ?, ?, 1, 'done', ?, ?, NULL, ?, ?, ?) RETURNING id`,
			f.sessionID, kind, text, attempts, reacted, f.tail.ID, now, now).Scan(&id))
		return id
	}
	bgID := seedDelivered(session.NoticeKindBGShellDone, "bg shell finished")
	wfID := seedDelivered(session.NoticeKindWakeFailed, "wake failed in the deleted branch")

	// Establish a non-default baseline so the re-pend reset assertion has a
	// literal, independent oracle before the handler runs.
	var baselineAttempts, baselineReacted int64
	require.NoError(t, f.a.DB().QueryRowContext(ctx,
		`SELECT wake_attempts, reacted FROM session_notices WHERE id = ?`, bgID).
		Scan(&baselineAttempts, &baselineReacted))
	require.EqualValues(t, 2, baselineAttempts)
	require.EqualValues(t, 1, baselineReacted)

	// The batch reconcile path is unconditional; the app itself was built
	// with the plain install so this test stays independent of the runtime
	// install.
	mockCoord := &cancellableHoldCoordinator{}
	f.a.AgentCoordinator = mockCoord

	env := f.run(t)
	require.Equal(t, EventResponse, env.Type, "the batch rerun commits and replies ok")

	var delivery string
	var wakeAttempts, reacted int64
	require.NoError(t, f.a.DB().QueryRowContext(ctx,
		`SELECT delivery, wake_attempts, reacted FROM session_notices WHERE id = ?`, bgID).
		Scan(&delivery, &wakeAttempts, &reacted))
	require.Equal(t, "pending", delivery, "the delivered completion notice of the deleted tail is re-pended for the new branch (F2)")
	require.Zero(t, wakeAttempts, "the re-pend resets the attempt counters")
	require.Zero(t, reacted, "the re-pend resets the reacted marker")

	require.NoError(t, f.a.DB().QueryRowContext(ctx,
		`SELECT delivery FROM session_notices WHERE id = ?`, wfID).Scan(&delivery))
	require.Equal(t, "void", delivery, "a wake_failed marker of the deleted branch is voided, not re-pended")
}
