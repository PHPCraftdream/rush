package agent

// B1: job_output must never answer a false "not found ... or already
// delivered" for a job that finished and was dropped from the in-memory map
// (deliverLocked) while its durable async_jobs row is still
// delivery='pending' -- the result is delivered as the next session
// message, not lost. ResolveJobShellID's durable-row fallback is pinned
// here against real SQLite.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// dropJobLocked simulates deliverLocked's in-memory half: the terminal
// transition committed and the job left bySession[owner].jobs.
func dropJob(l *workLedger, owner, toolCallID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.bySession[owner]; s != nil {
		delete(s.jobs, toolCallID)
	}
}

// TestWorkLedger_ResolveJobShellID_PendingDeliveryFromDurableRow pins the
// B1 window: the row is terminal with delivery='pending', the map entry is
// gone -- the resolver must answer *JobDeliveryStatusError (not delivered),
// which job_output turns into plain text.
//
// Revert-check: remove the durableDeliveryStatus fallback in
// ResolveJobShellID and this goes red (plain "not found" error instead).
func TestWorkLedger_ResolveJobShellID_PendingDeliveryFromDurableRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session-a", "call-1", "ls", tools.BashToolName, "", false, false, nil, nil)
	require.NoError(t, err)
	job := jobOf(l, "session-a", "call-1")
	l.setShellID(job, "003")

	_, err = l.store.Transition(ctx, session.TransitionParams{
		Owner: "session-a", ToolCallID: "call-1", State: "completed",
		NoticeKind: "completed", ResultSummary: "out", Delivery: "",
		ClaimID: job.claimID,
	})
	require.NoError(t, err)
	dropJob(l, "session-a", "call-1")

	_, err = l.ResolveJobShellID("session-a", "call-1")
	require.ErrorAs(t, err, new(*tools.JobDeliveryStatusError))
	var pend *tools.JobDeliveryStatusError
	require.ErrorAs(t, err, &pend)
	require.False(t, pend.Delivered, "delivery='pending' means the result is NOT yet delivered")
}

// TestWorkLedger_ResolveJobShellID_DoneRowReportsDelivered pins the honest
// "already delivered" answer: the row committed with delivery='done'
// (job_kill's step 6, or the notice was pulled).
func TestWorkLedger_ResolveJobShellID_DoneRowReportsDelivered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session-a", "call-1", "ls", tools.BashToolName, "", false, false, nil, nil)
	require.NoError(t, err)
	job := jobOf(l, "session-a", "call-1")

	_, err = l.store.Transition(ctx, session.TransitionParams{
		Owner: "session-a", ToolCallID: "call-1", State: "completed",
		NoticeKind: "completed", ResultSummary: "out", Delivery: "done",
		ClaimID: job.claimID,
	})
	require.NoError(t, err)
	dropJob(l, "session-a", "call-1")

	_, err = l.ResolveJobShellID("session-a", "call-1")
	var pend *tools.JobDeliveryStatusError
	require.ErrorAs(t, err, &pend)
	require.True(t, pend.Delivered)
}

// TestWorkLedger_ResolveJobShellID_RunningOrVoidRowKeepsNotFound pins that
// the fallback never lies in the other direction: a still-'running' row (or
// a voided one) keeps the plain not-found error -- only a terminal row with
// a decided delivery answers the delivery-status question.
func TestWorkLedger_ResolveJobShellID_RunningOrVoidRowKeepsNotFound(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)
	_, _, err := l.Start("session-a", "call-1", "ls", tools.BashToolName, "", false, false, nil, nil)
	require.NoError(t, err)
	dropJob(l, "session-a", "call-1") // row still 'running'

	_, err = l.ResolveJobShellID("session-a", "call-1")
	require.Error(t, err)
	require.NotErrorAs(t, err, new(*tools.JobDeliveryStatusError))
	require.Contains(t, err.Error(), "not found")
}
