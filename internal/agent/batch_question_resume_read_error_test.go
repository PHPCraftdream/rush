package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type failedHeldQuestionFacts struct{ BatchCutoverFacts }

func (failedHeldQuestionFacts) ActionableHeldQuestions(context.Context, string) ([]BatchHeldQuestionRoute, error) {
	return nil, errors.New("question state read failed")
}

func TestHeldQuestionResumeReadFailureIsNotAbsence(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	coord := &coordinator{}
	BindCoordinatorBatchCutoverFacts(coord, failedHeldQuestionFacts{})
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(coord) })
	refusal := coord.batchHeldQuestionResumeRefusal(t.Context(), "owner", "exact-owned-child")
	tl.addf("unreadable question state: refusal=%q", refusal)
	require.NotEmpty(t, refusal, "failed state lookup must not authorize a new child Run")
	require.Contains(t, refusal, "exact-owned-child")
	require.Contains(t, refusal, "unavailable")
}
