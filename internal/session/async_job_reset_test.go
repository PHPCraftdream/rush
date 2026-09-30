// ResetOwnerHistory coverage (R8A-3): the full wipe behind `rush sessions
// reset` is one transaction that deletes the messages and voids the owner's
// notice rows, and refuses (changing nothing) while a job still runs. Real
// SQLite.
package session

import (
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

const resetOtherOwner = "owner-2"

// resetDebris seeds owner-1 with every shape the wipe must void: a delivered
// unreacted wake job (debt), an announced cancelled job with a pending notice,
// and a pending wake session notice; owner-2 gets one pending job notice that
// must survive.
func resetDebris(t *testing.T) *rerunFx {
	t.Helper()
	f := newRerunFx(t)
	require.NoError(t, seedSession(f.ctx, f.q, resetOtherOwner))

	f.announced("done-1")
	f.complete("done-1")
	f.announced("cancelled-1")
	_, err := f.store.Transition(f.ctx, TransitionParams{Owner: rerunOwner, ToolCallID: "cancelled-1", State: "cancelled", NoticeKind: "session_cancel", Wake: true})
	require.NoError(t, err)
	require.NoError(t, f.store.InsertSessionNotice(f.ctx, rerunOwner, NoticeKindSupervision, "check the soak", true, ""))

	_, err = f.store.Claim(f.ctx, ClaimParams{Owner: resetOtherOwner, ToolCallID: "other-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	_, err = f.messages.Create(f.ctx, resetOtherOwner, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "other"}}})
	require.NoError(t, err)
	_, err = f.store.AnnounceStarted(f.ctx, f.messages, resetOtherOwner, "other-1", message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "other-1", Name: "bash", Content: "started"}},
	})
	require.NoError(t, err)
	_, err = f.store.Transition(f.ctx, TransitionParams{Owner: resetOtherOwner, ToolCallID: "other-1", State: "completed", NoticeKind: "completed", Wake: true})
	require.NoError(t, err)
	return f
}

// Revert-check: dropping the two Void*ForOwner statements from
// ResetOwnerHistory leaves the cancelled row and the notice pending and the
// done row as debt (the pulls below return them, the debt checks are true).
func TestResetOwnerHistory_WipesAndVoidsOnlyThisOwner(t *testing.T) {
	t.Parallel()
	f := resetDebris(t)
	debt, err := f.store.VisibleReactionDebtExists(f.ctx, rerunOwner)
	require.NoError(t, err)
	require.True(t, debt, "precondition: done-but-unreacted debt")

	out, err := f.store.ResetOwnerHistory(f.ctx, f.messages, rerunOwner)
	require.NoError(t, err)
	require.Positive(t, out.MessagesDeleted)
	require.EqualValues(t, 2, out.JobsVoided, "the delivered and the pending job row")
	require.EqualValues(t, 1, out.NoticesVoided)

	left, err := f.messages.List(f.ctx, rerunOwner)
	require.NoError(t, err)
	require.Empty(t, left)
	pulled, err := f.store.PullJobNotices(f.ctx, f.messages, rerunOwner, jobNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled)
	notices, err := f.store.PullSessionNotices(f.ctx, f.messages, rerunOwner, sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, notices)
	for _, visible := range []bool{false, true} {
		var got bool
		if visible {
			got, err = f.store.VisibleReactionDebtExists(f.ctx, rerunOwner)
		} else {
			got, err = f.store.ReactionDebtExists(f.ctx, rerunOwner)
		}
		require.NoError(t, err)
		require.False(t, got, "no debt of the wiped history survives (visible=%v)", visible)
	}
	for _, id := range []string{"done-1", "cancelled-1"} {
		require.Equal(t, "void", f.job(id).Delivery)
	}

	// Another owner is untouched.
	other, err := f.store.PullJobNotices(f.ctx, f.messages, resetOtherOwner, jobNoticeParams)
	require.NoError(t, err)
	require.Len(t, other, 1, "the other session's notice is still pullable")
	msgs, err := f.messages.List(f.ctx, resetOtherOwner)
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
}

// A row voided by the reset must not block a later reuse of its tool_call_id
// (a provider numbering calls per response), and a NEW notice of the session
// is delivered normally: the void is per row, not a sticky session state.
func TestResetOwnerHistory_LaterClaimsAndNoticesStillWork(t *testing.T) {
	t.Parallel()
	f := resetDebris(t)
	_, err := f.store.ResetOwnerHistory(f.ctx, f.messages, rerunOwner)
	require.NoError(t, err)

	res, err := f.store.Claim(f.ctx, ClaimParams{Owner: rerunOwner, ToolCallID: "cancelled-1", Kind: JobKindCommand, Input: "in-cancelled-1", ToolName: "bash"})
	require.NoError(t, err)
	require.False(t, res.Existing, "the voided row's id is free again")
	require.NoError(t, f.store.InsertSessionNotice(f.ctx, rerunOwner, NoticeKindSupervision, "fresh", true, ""))
	notices, err := f.store.PullSessionNotices(f.ctx, f.messages, rerunOwner, sessionNoticeParams)
	require.NoError(t, err)
	require.Len(t, notices, 1)
}

// Revert-check: removing the running-rows guard wipes the messages of a
// session whose job still runs (the assertions below fail).
func TestResetOwnerHistory_RunningJobRefusesAndChangesNothing(t *testing.T) {
	t.Parallel()
	f := resetDebris(t)
	_, err := f.store.Claim(f.ctx, ClaimParams{Owner: rerunOwner, ToolCallID: "running-1", Kind: JobKindCommand, Input: "sleep", ToolName: "bash"})
	require.NoError(t, err)

	_, err = f.store.ResetOwnerHistory(f.ctx, f.messages, rerunOwner)
	require.ErrorIs(t, err, ErrResetJobsRunning)

	left, err := f.messages.List(f.ctx, rerunOwner)
	require.NoError(t, err)
	require.NotEmpty(t, left, "a refused reset keeps the history")
	require.Equal(t, "pending", f.job("cancelled-1").Delivery, "and every notice row")
	visible, err := f.store.VisibleReactionDebtExists(f.ctx, rerunOwner)
	require.NoError(t, err)
	require.True(t, visible)
}

// A failure anywhere inside the transaction rolls the messages back together
// with the voids and publishes nothing. Not parallel: it owns the seam.
//
// Revert-check: delete the messages outside the transaction (before BEGIN) --
// the history is gone after the injected failure.
func TestResetOwnerHistory_InjectedFailureRollsEverythingBack(t *testing.T) {
	f := resetDebris(t)
	before, err := f.messages.List(f.ctx, rerunOwner)
	require.NoError(t, err)
	sub := f.messages.Subscribe(f.ctx)

	for _, step := range []string{"deleted", "voided"} {
		resetStepSeam = func(s string) error {
			if s == step {
				return errors.New("injected failure at " + step)
			}
			return nil
		}
		_, err = f.store.ResetOwnerHistory(f.ctx, f.messages, rerunOwner)
		require.ErrorContains(t, err, "injected failure at "+step)
		resetStepSeam = nil

		after, err := f.messages.List(f.ctx, rerunOwner)
		require.NoError(t, err)
		require.Len(t, after, len(before), "step %s: the history survives a rolled-back reset", step)
		require.Equal(t, "pending", f.job("cancelled-1").Delivery, "step %s: the voids roll back too", step)
	}
	select {
	case ev := <-sub:
		t.Fatalf("a rolled-back reset must publish nothing, got %v", ev)
	default:
	}
}
