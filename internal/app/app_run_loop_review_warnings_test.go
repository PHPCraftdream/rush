// The loop's reviewer pass appends its warnings to the executor's result at the
// scope-closed exit (closePhase -> attachReview / recordReviewFailure), long
// after firstTurnPhase snapshotted that turn into loopTotals, so
// loopTotals.applyTo must keep warnings appended to final beyond its snapshot —
// otherwise the JSON envelope's warnings never mention a failed review.
// Same harness as app_run_loop_test.go (real App and SQLite, httptest provider).
package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// TestLoopReviewWarnings_ReviewerFailVerdictKeepsWarning drives the real loop:
// the reviewer answers VERDICT: FAIL, attachReview appends its warning to the
// executor's result after the loop snapshotted that turn, and the envelope must
// still carry both the verdict and the warning.
//
// Revert-check: restoring the old applyTo (final.Warnings =
// t.warningsFor(final) only) silently drops the late append and the Warnings
// assertion goes red (review_verdict survives; only warnings lose it).
func TestLoopReviewWarnings_ReviewerFailVerdictKeepsWarning(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		_, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "independent reviewer") {
			loopText(w, "r", "VERDICT: FAIL\n\nthe change does not do what was asked", 11, 3)
			return
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})

	res, _, err := h.run(loopCtx(t), RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "first answer", res.FinalText, "A10: final_text stays the executor's answer")
	require.Equal(t, "fail", res.ReviewVerdict)
	require.Contains(t, res.Warnings, "reviewer verdict: FAIL - see review",
		"the envelope's warnings must carry the FAIL verdict even though it was appended after the turn's snapshot, got %v", res.Warnings)
}

// TestLoopReviewWarnings_ReviewerPassFailureKeepsWarning: the review turn dies
// on a provider error (a 4xx is terminal, so it fails once); recordReviewFailure
// appends its warning to the executor's result, and the envelope must keep it.
//
// Revert-check: restoring the old applyTo drops the late append and the
// failed-warning lookup goes red.
func TestLoopReviewWarnings_ReviewerPassFailureKeepsWarning(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		_, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "independent reviewer") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"reviewer model rejected the request","type":"invalid_request_error"}}`))
			return
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})

	res, _, err := h.run(loopCtx(t), RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	require.NoError(t, err, "a failed review keeps the executor's clean answer")
	require.NotNil(t, res)
	require.Equal(t, "error", res.ReviewVerdict)
	var failed []string
	for _, w := range res.Warnings {
		if strings.Contains(w, "reviewer pass failed") {
			failed = append(failed, w)
		}
	}
	require.Len(t, failed, 1, "the envelope's warnings must name the reviewer failure, got %v", res.Warnings)
}

// TestLoopTotals_KeepsWarningsAppendedAfterTheSnapshot is the direct oracle:
// snapshot first, then the append exactly the way attachReview and
// recordReviewFailure do it after closePhase's add.
//
// Revert-check: restoring the old applyTo makes the first assertion fail with
// ["a"] instead of ["a","late"].
func TestLoopTotals_KeepsWarningsAppendedAfterTheSnapshot(t *testing.T) {
	res := &RunResult{Warnings: []string{"a"}}
	var tot loopTotals
	tot.add(res)
	res.Warnings = append(res.Warnings, "late") // what attachReview does after the add

	tot.applyTo(res, time.Now())

	require.Equal(t, []string{"a", "late"}, res.Warnings,
		"warnings appended after the snapshot survive the replacement")
	tot.applyTo(res, time.Now()) // a re-applied totals must not duplicate them
	require.Equal(t, []string{"a", "late"}, res.Warnings)
}

// TestLoopTotals_FinalNotAmongTurnsKeepsNothingExtra pins the guard: a final
// that was never added has no snapshot to be late beyond, so applyTo keeps none
// of its own warnings — the rebuilt list comes from the other turns alone.
func TestLoopTotals_FinalNotAmongTurnsKeepsNothingExtra(t *testing.T) {
	res := &RunResult{Warnings: []string{"a"}}
	var tot loopTotals
	tot.add(&RunResult{Warnings: []string{"turn"}})

	tot.applyTo(res, time.Now())

	require.Equal(t, []string{"turn"}, res.Warnings, "the turn's warnings are rebuilt; final's own warnings are not kept (no snapshot to be late beyond)")
}

// TestLoopTotals_NonFinalTextWarningsStillFiltered pins that the late-warning
// fix does not loosen warningsFor: a superseded turn's final_text warning stays
// dropped while the final turn's late warning is kept.
//
// Revert-check: removing the text-warning filter from warningsFor makes the
// first assertion fail with the stale "final_text is empty" warning.
func TestLoopTotals_NonFinalTextWarningsStillFiltered(t *testing.T) {
	first := buildRunResult("s", "", "", "end_turn", nil, false, nil, 10, 0, 0, "", "", 0, "", "", nil, "")
	require.Contains(t, strings.Join(first.Warnings, "\n"), "final_text is empty")
	second := buildRunResult("s", "the answer", "", "end_turn", nil, false, nil, 5, 0, 0, "", "", 0, "", "", nil, "")
	var tot loopTotals
	tot.add(&first)
	tot.add(&second)
	second.Warnings = append(second.Warnings, "late")

	tot.applyTo(&second, time.Now())

	require.Equal(t, []string{"late"}, second.Warnings,
		"the superseded turn's final_text warning is filtered, the late warning is kept")
}
