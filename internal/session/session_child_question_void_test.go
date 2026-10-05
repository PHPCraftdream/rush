package session

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestPendingChildQuestions_VoidsWhenDelegationNotRunning: the read side of
// the awaiting-answer fix -- a pending child_question notice whose bound
// delegation is no longer running (it finished before the parent ever pulled)
// must NOT surface, while a notice on a still-running delegation survives:
// the filter is per notice. The classifier half: with a wake schedule keeping
// the session between turns, the dropped notice must not produce WaitAnswer.
//
// Revert check (performed): removed the void filter from
// PendingChildQuestions (every parsed pending notice returned again) -- the
// require.Empty below FAILED, the finished delegation's notice was still
// returned. Restored the filter; re-ran, passed.
func TestPendingChildQuestions_VoidsWhenDelegationNotRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	store := NewAsyncJobStore(conn, dataDir, 1, "test")
	t.Cleanup(func() { _ = store.Close(ctx) })

	// session_notices.owner is an FK: real session rows first.
	svc := NewService(db.New(conn), conn)
	root, err := svc.Create(ctx, "void root")
	require.NoError(t, err)

	schedule := []OpenWakeSchedule{{ID: "wake_1", NextRunAt: time.Unix(1700000000, 0).UTC()}}
	classify := func(qs []ChildQuestion) ActivityVerdict {
		return ClassifySessionActivity(ActivityFacts{
			SessionID: root.ID, OpenSchedules: schedule, ChildQuestions: qs,
		})
	}

	// Delegation X runs, its child asks, the notice lands: still pending.
	_, err = store.Claim(ctx, ClaimParams{
		Owner: root.ID, ToolCallID: "call-x", Kind: JobKindAgent,
		Input: "ask x", ToolName: "agent", ChildSessionID: "child-x",
	})
	require.NoError(t, err)
	require.NoError(t, store.InsertSessionNotice(ctx, root.ID, NoticeKindChildQuestion, noticeText("child-x", "go on?"), true, "call-x"))

	qs, err := store.PendingChildQuestions(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, qs, 1)
	require.Equal(t, "call-x", qs[0].DelegationToolCallID)
	require.Contains(t, classify(qs).WaitingOn, WaitAnswer)

	// X finishes before the parent ever pulls its notices: nothing is
	// awaiting an answer anymore, even though the notice row stays pending.
	_, err = store.Transition(ctx, TransitionParams{Owner: root.ID, ToolCallID: "call-x", State: "completed", Wake: true})
	require.NoError(t, err)

	qs, err = store.PendingChildQuestions(ctx, root.ID)
	require.NoError(t, err)
	require.Empty(t, qs, "a notice bound to a finished delegation must not surface as awaiting answer")
	v := classify(qs)
	require.Equal(t, ActivityBetweenTurns, v.Kind)
	require.Equal(t, []string{WaitSchedule}, v.WaitingOn)
	require.NotContains(t, v.WaitingOn, WaitAnswer)

	// Control: the filter is per notice -- a notice on the still-running
	// delegation Y survives X's completion.
	_, err = store.Claim(ctx, ClaimParams{
		Owner: root.ID, ToolCallID: "call-y", Kind: JobKindAgent,
		Input: "ask y", ToolName: "agent", ChildSessionID: "child-y",
	})
	require.NoError(t, err)
	require.NoError(t, store.InsertSessionNotice(ctx, root.ID, NoticeKindChildQuestion, noticeText("child-y", "still here?"), true, "call-y"))

	qs, err = store.PendingChildQuestions(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, qs, 1)
	require.Equal(t, "call-y", qs[0].DelegationToolCallID)
}
