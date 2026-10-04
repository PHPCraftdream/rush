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
// run with a reviewer configured and reads the two provider requests: the
// primary turn must not carry the block, the review turn must, with the
// original request in it.
//
// Revert-check: make captureReviewBasis return nil (the basis is never
// captured): reviewerTurnPrompt then sends the bare prompt and this test goes
// red on the review request.
func TestReviewerPass_ReviewRequestCarriesTheEvidenceBlock(t *testing.T) {
	h := newReviewerPassAppOpts(t, reviewerPassAppOpts{withReviewer: true})
	sess := createModelOverrideSession(t, h.app, "reviewer-evidence-wire")

	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel}, h.requestedModels())

	h.mu.Lock()
	bodies := append([]string(nil), h.bodies...)
	h.mu.Unlock()
	require.Len(t, bodies, 2)

	require.NotContains(t, bodies[0], evidenceBlockOpen, "the primary turn is not a review")
	require.Contains(t, bodies[1], evidenceBlockOpen, "the review turn must carry the computed evidence block")
	require.Contains(t, bodies[1], "do the thing", "the block must carry the original request of the run")
	require.Contains(t, bodies[1], "independent reviewer", "the fixed reviewer prompt stays in front of the block")
}

// TestReviewerPass_NoReviewerNoEvidenceBasis is the control: without a
// configured reviewer no basis is captured and no request mentions the block.
func TestReviewerPass_NoReviewerNoEvidenceBasis(t *testing.T) {
	h := newReviewerPassAppOpts(t, reviewerPassAppOpts{withReviewer: false})
	sess := createModelOverrideSession(t, h.app, "reviewer-evidence-none")

	_, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)

	require.NotContains(t, h.recordedBodies(), evidenceBlockOpen)
	basis := h.app.captureReviewBasis(context.Background(), config.SelectedModelTypeSmart, "x", time.Now())
	require.Nil(t, basis, "no reviewer slot: no basis")
}
