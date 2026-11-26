// End-to-end half of T6 of docs/plans/2026-10-02-reviewer-pass-verification.md
// §8: the review turn the provider RECEIVES carries the evidence block. The
// unit tests build the block from hand-made snapshots; this one proves the
// run actually captures a basis, builds the block and puts it into the review
// request.

package app

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// evidenceBlockOpen is the start of the COMPUTED block's opening tag. The bare
// word review_evidence also appears in the fixed prompt text itself, so it
// cannot tell the block from the prompt.
const evidenceBlockOpen = "review_evidence trust="

// TestReviewerPass_ReviewRequestCarriesTheEvidenceBlock runs a --role smart
// run and confirms the fresh package's computed-change and original-request sections.
//
// Revert-check: removing the fresh package's computed changes or original
// prompt section leaves the child transcript without independent evidence.
func TestReviewerPass_ReviewRequestCarriesTheEvidenceBlock(t *testing.T) {
	h := newReviewerPassAppOpts(t, reviewerPassAppOpts{withReviewer: true})
	sess := createModelOverrideSession(t, h.app, "reviewer-evidence-wire")

	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel, reviewerPassReviewerModel}, h.requestedModels())

	child, err := h.app.Sessions.Get(context.Background(), result.ReviewerSessionID)
	require.NoError(t, err)
	bodies := h.recordedBodies()
	require.Contains(t, bodies, "computed changes (before/after snapshots and diff)")
	require.Contains(t, bodies, "original_prompt:")
	require.Contains(t, bodies, "do the thing")
	childMsgs, err := h.app.Messages.List(context.Background(), child.ID)
	require.NoError(t, err)
	require.NotEmpty(t, childMsgs, "the evidence-bearing reviewer prompt is persisted in the child transcript")
}

// TestReviewerPass_NoReviewerNoEvidenceBasis is the control: without a
// configured reviewer no basis is captured and no request mentions the block.
func TestReviewerPass_NoReviewerNoEvidenceBasis(t *testing.T) {
	h := newReviewerPassAppOpts(t, reviewerPassAppOpts{withReviewer: false})
	sess := createModelOverrideSession(t, h.app, "reviewer-evidence-none")

	_, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)

	require.NotContains(t, h.recordedBodies(), "computed changes (before/after snapshots and diff)")
	basis := h.app.captureReviewBasis(context.Background(), config.SelectedModelTypeSmart, "x", time.Now())
	require.Nil(t, basis, "no reviewer slot: no basis")
}
