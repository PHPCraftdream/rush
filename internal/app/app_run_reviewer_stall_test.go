package app

// Unit tests for the reviewer pass's stall restarts (app_run_reviewer_stall.go).
//
//   - TestRetryStalledReviewerPassExhaustsAttempts: a review turn that
//     stalls on EVERY attempt must be restarted exactly twice (3 attempts
//     total) and then fail with ErrReviewerUnresponsive.
//
//   - TestRetryStalledReviewerPassRestartsOnceAndSucceeds: a review turn
//     that stalls only on the first attempt must be restarted once, the
//     retry must succeed, and no unresponsive error may surface.
//
//   - TestReviewerUnresponsiveKeepsPrimaryResult: a review turn that
//     exhausted its stall restarts must take the existing review-failed
//     path — the executor's RunResult and final_text stay the run's
//     outcome, with the "review failed: reviewer unresponsive" text on the
//     failure record.

import (
	"context"
	"errors"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

// stalledReviewErr mirrors the error shape the agent's stall abort joins
// (agent_turn_failure.go): the stream failure plus agent.ErrTurnStalled.
func stalledReviewErr() error {
	return errors.Join(errors.New("provider stream stalled"), agent.ErrTurnStalled)
}

func TestRetryStalledReviewerPassExhaustsAttempts(t *testing.T) {
	attempts := 0
	result, err := retryStalledReviewerPass(func() (*RunResult, error) {
		attempts++
		return nil, stalledReviewErr()
	})
	require.ErrorIs(t, err, ErrReviewerUnresponsive,
		"an always-stalling reviewer must surface the unresponsive outcome")
	require.ErrorIs(t, err, agent.ErrTurnStalled,
		"the underlying stall abort must stay joined for diagnostics")
	require.Equal(t, reviewerStallMaxAttempts, attempts,
		"the pass must run exactly 3 attempts: 1 initial + 2 restarts")
	require.Nil(t, result,
		"no attempt ever produced a review result; nothing may be returned")
}

func TestRetryStalledReviewerPassRestartsOnceAndSucceeds(t *testing.T) {
	attempts := 0
	reviewResult := &RunResult{FinalText: "VERDICT: PASS"}
	result, err := retryStalledReviewerPass(func() (*RunResult, error) {
		attempts++
		if attempts == 1 {
			return nil, stalledReviewErr()
		}
		return reviewResult, nil
	})
	require.NoError(t, err,
		"a reviewer that stalls once and then answers must complete the review")
	require.Equal(t, 2, attempts,
		"exactly one restart after the first stalled attempt")
	require.Same(t, reviewResult, result,
		"the successful retry's review result must be the pass's outcome")
}

func TestReviewerUnresponsiveKeepsPrimaryResult(t *testing.T) {
	stalled := retryStalledReviewerPassErr()
	primary := &RunResult{FinalText: "PRIMARY ANSWER"}
	require.True(t, reviewFailureKeepsPrimary(context.Background(), stalled),
		"an unresponsive reviewer is a review failure of its own: the executor's answer must stay the run's outcome")
	recordReviewFailure(primary, errDiscard{}, stalled)
	require.Equal(t, "error", primary.ReviewVerdict)
	require.Contains(t, primary.Warnings[0], "review failed: reviewer unresponsive")
	require.Equal(t, "PRIMARY ANSWER", primary.FinalText,
		"the executor's result text must never be touched by reviewer stalls")
}

// retryStalledReviewerPassErr drives the real retry helper into its
// exhausted state and returns the error it produces.
func retryStalledReviewerPassErr() error {
	_, err := retryStalledReviewerPass(func() (*RunResult, error) {
		return nil, stalledReviewErr()
	})
	return err
}

// errDiscard is a no-op io.Writer for recordReviewFailure's stderr line.
type errDiscard struct{}

func (errDiscard) Write(p []byte) (int, error) { return len(p), nil }
