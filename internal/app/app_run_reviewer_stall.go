package app

// REVIEWER PASS stall handling: the turn-stall policy (internal/agent)
// aborts a stalled review turn with agent.ErrTurnStalled, but the reviewer
// pass itself owns the retry decision. This file restarts a stalled review
// turn up to twice (3 attempts total); if the reviewer is still stalling,
// the outcome is ErrReviewerUnresponsive and the EXISTING review-failed
// path keeps the executor's result so the process exits instead of hanging.

import (
	"errors"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/agent"
)

// ErrReviewerUnresponsive is joined into the review error when every
// reviewer attempt stalled. The review gate maps it onto the existing
// review-failed path: the executor's result is kept.
var ErrReviewerUnresponsive = errors.New("review failed: reviewer unresponsive")

// reviewerStallMaxAttempts is the total number of review turn attempts:
// the first run plus two restarts of the reviewer pass.
const reviewerStallMaxAttempts = 3

// retryStalledReviewerPass runs the review turn, restarting it whenever an
// attempt was aborted by the turn-stall policy (agent.ErrTurnStalled), at
// most reviewerStallMaxAttempts-1 restarts, each announced with a WARN.
// Any other failure returns immediately. When the final attempt also
// stalled, ErrReviewerUnresponsive is joined into the returned error; the
// returned result is never the executor's and is discarded by the gate.
func retryStalledReviewerPass(attempt func() (*RunResult, error)) (*RunResult, error) {
	result, err := attempt()
	for restart := 1; err != nil && errors.Is(err, agent.ErrTurnStalled) && restart < reviewerStallMaxAttempts; restart++ {
		slog.Warn("reviewer pass stalled; restarting the review turn",
			"attempt", restart+1, "max_attempts", reviewerStallMaxAttempts)
		result, err = attempt()
	}
	if err != nil && errors.Is(err, agent.ErrTurnStalled) {
		slog.Warn("reviewer pass unresponsive after stall restarts", "max_attempts", reviewerStallMaxAttempts)
		err = errors.Join(err, ErrReviewerUnresponsive)
	}
	return result, err
}
