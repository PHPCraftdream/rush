package app

// C9-16: a Drain that runs AFTER the loop's reviewer pass used to replace
// l.final: the envelope's final_text became the reviewer model's reaction
// text and the review/review_verdict attached at the close disappeared
// (A10: the executor's answer stays final_text, the verdict is additive).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: restoring the unconditional `l.final = result` in
// afterDrain's drainCompleted case makes the post-review Drain the run's
// answer: final_text "reaction text" and an empty review_verdict. This test
// FAILED on both assertions.
func TestRunLoop_DrainAfterTheReviewKeepsTheReviewedAnswer(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, drain bool, _ int) {
		switch {
		case drain: // the reaction Drain: newest user message is the seeded notice
			loopText(w, "d", "reaction text", 30, 10)
		case strings.Contains(string(body), "independent reviewer"):
			loopText(w, "r", "VERDICT: FAIL\n\nthe change does not do what was asked", 11, 3)
		default:
			loopText(w, "a", "first answer", 11, 3)
		}
	})
	h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	// The B9-2 window: the notice is inserted while the reviewer turn runs
	// (its turn-start pull already happened), so it stays Owed and the next
	// decide runs a Drain -- on the reviewer's turn overrides.
	var turns int
	cliLoopTurnDoneSeam = func() {
		turns++
		if turns == 2 { // fires right after the reviewer turn returned
			h.seedDebt()
		}
	}
	t.Cleanup(func() { cliLoopTurnDoneSeam = nil })

	res, _, err := h.run(loopCtx(t), RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "first answer", res.FinalText, "A10: the executor's answer stays final_text")
	require.Equal(t, "fail", res.ReviewVerdict, "the verdict attached at the close survives the later Drain")
	require.EqualValues(t, 3, h.requests.Load(), "first turn, reviewer pass, one reaction Drain")
	require.EqualValues(t, 1, h.drains.Load())
}
