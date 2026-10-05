package app

// C9-1 (ox round 9): when the review turn FAILS and the run was not asked to
// capture a structured result (terse and stream mode -- the SDK default),
// the primary phase's result is nil. ExecuteRun's "keep the executor's answer"
// branch dereferenced it unconditionally and took the host process down.

import (
	"bytes"
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: restoring the inline `primaryResult.ReviewVerdict = "error"` in
// ExecuteRun's reviewFailureKeepsPrimary branch makes this test FAILED with a
// nil-pointer panic (require.NotPanics reports it).
func TestReviewerPass_FailureInTerseModeKeepsPrimaryAnswerWithoutPanic(t *testing.T) {
	app := newReviewerPassFailureApp(t)
	sess := createModelOverrideSession(t, app, "reviewer-pass-failure-terse")

	var stdout, stderr bytes.Buffer
	var res *RunResult
	var err error
	require.NotPanics(t, func() {
		res, err = app.ExecuteRun(context.Background(), RunRequest{
			Prompt:            "do the thing",
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeSmart},
			Mode:              RunModeTerse,
			Stdout:            &stdout,
			Stderr:            &stderr,
			HideSpinner:       true,
		})
	})
	require.NoError(t, err, "a failed review must not fail the run")
	require.Nil(t, res, "terse mode without captureResult carries no structured result")
	require.Contains(t, stderr.String(), "rush run: reviewer pass failed",
		"the operator still gets the one stderr line naming the reviewer failure")
	require.Contains(t, stdout.String(), reviewerPassPrimaryText,
		"the executor's answer is what terse mode prints")
}
