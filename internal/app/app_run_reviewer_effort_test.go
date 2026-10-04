// T12 of docs/plans/2026-10-02-reviewer-pass-verification.md §8 (#1166): the
// reasoning_effort recorded on the review turn's assistant messages is the
// REVIEWER slot's effort, not the session's smart-model effort.

package app

import (
	"context"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestReviewerPass_RecordsReviewerEffort runs a --role smart run whose reviewer
// slot is configured with effort "max". The assistant message of the review
// turn (the one after the reviewer-pass marker) must carry "max"; the primary
// turn's assistant message must carry the session's own effort.
//
// Revert-check: put `ts.currentSession.SmartModelReasoningEffort` back in
// agent_turn_step.go (the label of the assistant message) instead of
// turnSmartReasoningEffort(callContext, ts.currentSession): the review turn is
// then labelled with the session's effort and this test goes red.
func TestReviewerPass_RecordsReviewerEffort(t *testing.T) {
	h := newReviewerPassAppOpts(t, reviewerPassAppOpts{withReviewer: true, reviewerEffort: "max"})
	sess := createModelOverrideSession(t, h.app, "reviewer-effort-label")

	before, err := h.app.Sessions.Get(context.Background(), sess.ID)
	require.NoError(t, err)
	require.NotEqual(t, "max", before.SmartModelReasoningEffort, "control: the session itself is not at max")

	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel}, h.requestedModels())

	msgs, err := h.app.Messages.List(context.Background(), sess.ID)
	require.NoError(t, err)

	markerAt := -1
	for i, m := range msgs {
		if m.Role == message.User && strings.Contains(m.Content().Text, reviewerPassMarker) {
			markerAt = i
		}
	}
	require.GreaterOrEqual(t, markerAt, 0, "the reviewer-pass prompt must be in the transcript")

	var primaryEffort, reviewEffort string
	var sawPrimary, sawReview bool
	for i, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		if i > markerAt {
			reviewEffort, sawReview = m.ReasoningEffort, true
		} else {
			primaryEffort, sawPrimary = m.ReasoningEffort, true
		}
	}
	require.True(t, sawPrimary && sawReview, "both an executor and a reviewer assistant message are expected")
	require.Equal(t, "max", reviewEffort, "the review turn must be labelled with the reviewer slot's effort")
	require.Equal(t, before.SmartModelReasoningEffort, primaryEffort, "the primary turn keeps the session's own effort")
}
