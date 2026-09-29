// C16 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN item 3):
// the automatic reviewer-pass gate (app_run.go, around the ScopeOpen call)
// must retry a DB read error with a pause instead of silently skipping the
// reviewer pass on the very first failure -- doc sec.3.5's "a DB read error
// is a retry with a pause, never a silent skip" applies here exactly like
// it does to the CLI loop's own waitForNextCLITurn.
package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// flakyScopeSource's ScopeOpen fails failCount times, then returns
// (wantOpen, nil) forever after -- enough to exercise both "recovers within
// the retry budget" and "persistently failing" cases from one fake.
type flakyScopeSource struct {
	failCount int32
	wantOpen  bool

	calls int32
}

func (f *flakyScopeSource) ClaimExternalDriver(string)          {}
func (f *flakyScopeSource) ReleaseExternalDriver(string)        {}
func (f *flakyScopeSource) RunMaintenanceSweep(context.Context) {}
func (f *flakyScopeSource) ReactionDebtExists(context.Context, string) (bool, error) {
	return false, nil
}
func (f *flakyScopeSource) WaitForHint(context.Context, string) {}
func (f *flakyScopeSource) CaptureDrainSnapshot(context.Context, string) session.DebtSnapshot {
	return session.DebtSnapshot{}
}

func (f *flakyScopeSource) RecordDrainTurnOutcome(context.Context, string, session.DebtSnapshot, error, bool) {
}

func (f *flakyScopeSource) ScopeOpen(context.Context, string) (bool, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n <= f.failCount {
		return false, errors.New("database is locked")
	}
	return f.wantOpen, nil
}

// TestReviewerPassScopeStillOpen_RecoversWithinRetryBudget: a DB error on
// the first attempt must not be treated as "skip the reviewer pass" --
// the SECOND attempt's real answer (scope closed) must win.
//
// Revert-check performed: made reviewerPassScopeStillOpen return true
// (skip) on the FIRST ScopeOpen error instead of retrying -- this test's
// `require.False(t, open)` FAILED (open was true: the transient error alone
// blocked a reviewer pass that should have run). Restored the retry loop;
// re-ran, passed.
func TestReviewerPassScopeStillOpen_RecoversWithinRetryBudget(t *testing.T) {
	orig := cliDBRetryPause
	cliDBRetryPause = time.Millisecond
	t.Cleanup(func() { cliDBRetryPause = orig })

	fake := &flakyScopeSource{failCount: 1, wantOpen: false}
	app := &App{}
	open := app.reviewerPassScopeStillOpen(context.Background(), fake, "sess-1")
	require.False(t, open, "the retry's real answer (scope closed) must be used, not the first attempt's error")
	require.Equal(t, int32(2), atomic.LoadInt32(&fake.calls))
}

// TestReviewerPassScopeStillOpen_PersistentFailureDefaultsToOpen: once every
// retry is exhausted, the conservative default is "open" (skip the reviewer
// pass) rather than guessing "closed" and running an extra phase against
// unknown scope state.
func TestReviewerPassScopeStillOpen_PersistentFailureDefaultsToOpen(t *testing.T) {
	orig := cliDBRetryPause
	cliDBRetryPause = time.Millisecond
	t.Cleanup(func() { cliDBRetryPause = orig })

	fake := &flakyScopeSource{failCount: 100, wantOpen: false}
	app := &App{}
	open := app.reviewerPassScopeStillOpen(context.Background(), fake, "sess-1")
	require.True(t, open, "persistent DB failure must default to skipping the reviewer pass")
	require.Equal(t, int32(reviewerPassScopeOpenRetries), atomic.LoadInt32(&fake.calls),
		"retries must be bounded, not unbounded")
}

// TestReviewerPassScopeStillOpen_NoErrorReturnsRealValueImmediately is the
// common-case regression guard: no retry loop overhead when ScopeOpen just
// answers.
func TestReviewerPassScopeStillOpen_NoErrorReturnsRealValueImmediately(t *testing.T) {
	fake := &flakyScopeSource{failCount: 0, wantOpen: true}
	app := &App{}
	open := app.reviewerPassScopeStillOpen(context.Background(), fake, "sess-1")
	require.True(t, open)
	require.Equal(t, int32(1), atomic.LoadInt32(&fake.calls))
}
