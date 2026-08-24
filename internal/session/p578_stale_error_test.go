package session_test

// Regression coverage for task #578 (P2-1 of the 2026-08-19 release-readiness
// review): DrainSessionNow must not keep asserting a retryable local
// failure's error once that row's fate has been superseded -- either by a
// later retry AT THE SAME ROW succeeding in this same process, or by a
// genuinely different process's pump instance taking the row over before
// this loop's own next attempt.
//
// See the CONTRACT DECISION comment atop DrainSessionNow
// (run_queue_drain_session.go) for the explicit (a)-vs-(b) choice this
// closes: a later resolution supersedes an earlier retryable failure at the
// same row (option a), not "stop at the first error" (option b).
//
// The coordinator's review of an earlier version of this fix found the
// re-check collapsed err to nil whenever the row was simply gone, treating
// that as a confirmed success. It is not: AckRunQueueEntry and
// TerminalFailRunQueueEntry are both a plain DELETE ... RETURNING id query
// against session_run_queue (see sql/run_queue.sql) -- a row gone after a
// DIFFERENT process touched it is exactly as ambiguous, after the fact, as
// one still leased by a different process, and nothing in this schema
// survives to tell a genuine commit apart from a permanent terminal
// failure. Treating "gone" as "succeeded" reopened exactly the
// false-success class task #575 (commit 638bc777) closed, through this new
// re-check path instead of the admission-wait path #575 fixed --
// app_run.go's RunNonInteractive reads a completed drain as "a durable
// continuation completed here" and converts it directly into exit code 0.
// The re-check therefore NEVER clears a row's failure entry for the
// cross-process case; each reachable outcome below is pinned separately,
// mirroring p575_background_outcome_handoff_test.go's per-outcome pattern,
// so this class cannot come back silently through the re-check path either.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// retryThenSucceedCoordinator fails the first N calls with an ordinary
// (non-terminal, non-busy) retryable error, then succeeds.
type retryThenSucceedCoordinator struct {
	calls        atomic.Int64
	failuresLeft atomic.Int32
}

func (c *retryThenSucceedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	if c.failuresLeft.Add(-1) >= 0 {
		return nil, errors.New("p578: simulated transient retryable failure")
	}
	return nil, nil
}

// TestDrainSessionNow_SameRowRetrySucceedsClearsErr covers the literal
// scenario named in task #578: a retryable attempt is nacked, the SAME row
// is retried by this same process's own DrainSessionNow loop, and that
// retry commits cleanly. The final returned result must be a completed
// drain, not the earlier attempt's stale failure.
func TestDrainSessionNow_SameRowRetrySucceedsClearsErr(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p578-same-row-retry")
	coord := &retryThenSucceedCoordinator{}
	coord.failuresLeft.Store(1) // fail once, then succeed
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p578-same-row",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p578-same-row-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.Equal(t, int64(2), coord.calls.Load(), "must have retried after the transient failure and succeeded on the second attempt")
	require.NoError(t, drainErr, "the LATEST outcome for the row was a commit -- a stale error from the earlier nacked attempt at the SAME row must not survive")
	require.Equal(t, session.DrainComplete, result)
}

// stealOnNackSessions wraps a real session.Service and, the instant
// NackRunQueueEntry is called (returning a row to pending after a local
// retryable failure), synchronously leases that same row under a DIFFERENT
// leasedBy -- deterministically simulating a genuinely different process's
// pump instance winning the DB-level lease race for the same row before
// THIS process's DrainSessionNow loop gets back around to it. No sleep/poll
// timing dependency: the steal happens inline, in the same call that would
// otherwise return control to DrainSessionNow's loop.
type stealOnNackSessions struct {
	session.Service
	sessionID string
	stolen    atomic.Bool
}

func (s *stealOnNackSessions) NackRunQueueEntry(ctx context.Context, id, leasedBy, lastError string) error {
	if err := s.Service.NackRunQueueEntry(ctx, id, leasedBy, lastError); err != nil {
		return err
	}
	leased, err := s.LeaseRunQueueEntry(context.Background(), s.sessionID, "external-process-pump", time.Minute)
	if err == nil && leased != nil {
		s.stolen.Store(true)
	}
	return nil
}

type failOnceCoordinator struct {
	calls atomic.Int64
}

func (c *failOnceCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, errors.New("p578: simulated transient failure, row stolen by another process right after nack")
}

// TestDrainSessionNow_ExternalProcessStealAfterLocalNackReportsUnknown
// covers the cross-process variant the literal same-row-retry-in-this-
// process case does not: this process's own retryable failure Nacks the
// row back to pending, but a DIFFERENT process's pump instance leases it
// before this loop's own next attempt (this process's in-memory admission
// map cannot see a different process's DB-level lease). The row's ultimate
// fate is then UNKNOWN to this call -- it must neither report its own
// stale local failure as the final answer, nor fabricate a completed drain
// for an outcome it cannot confirm. It must report DrainFailed with a
// distinct "unknown" error (errLeaseLost's own meaning), matching this
// repo's existing vocabulary for "someone else has it, nothing further can
// be learned here" rather than misusing success for "unknown".
//
// Deterministic: the steal happens synchronously inside the wrapped
// NackRunQueueEntry call, not via a background goroutine racing on timing.
//
// Revert-check: change the `case current.Status == "leased":` branch in
// DrainSessionNow's re-check back to clearing the ledger entry and this
// fails on the require.Error below -- exactly the false success this test
// exists to catch.
func TestDrainSessionNow_ExternalProcessStealAfterLocalNackReportsUnknown(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p578-external-steal")
	wrapped := &stealOnNackSessions{Service: svc, sessionID: sess.ID}
	coord := &failOnceCoordinator{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       wrapped,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p578-external",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p578-external-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.True(t, wrapped.stolen.Load(), "the external steal must have succeeded (deterministic, not timing-dependent) -- otherwise this test proves nothing")
	require.Equal(t, int64(1), coord.calls.Load(), "only the one local failing attempt should have run in this process; a second local run would itself be a duplicate-execution bug")
	require.Equal(t, session.DrainFailed, result, "an execution genuinely happened in this process (the failed attempt) -- the row is now owned by a genuinely different process whose outcome is unknown to this call")
	require.Error(t, drainErr, "a completed drain here would be app_run.go's exact false-success contract violation")
	require.NotErrorIs(t, drainErr, context.DeadlineExceeded, "must not be conflated with a mere lookup failure")
	// errLeaseLost is unexported; pin its observable message the same way
	// p575_background_outcome_handoff_test.go's own lease-loss test does.
	require.ErrorContains(t, drainErr, "lost lease ownership", "an UNKNOWN outcome (someone else holds the row) must be reported through the same 'nothing further can be learned' vocabulary this package already uses for that meaning, not a fabricated success")
}

// stealAndTerminalFailOnNackSessions wraps a real session.Service and, the
// instant NackRunQueueEntry is called, synchronously leases the same row
// under a DIFFERENT leasedBy and immediately terminal-fails it --
// deterministically simulating a genuinely different process's pump
// instance not merely taking the row over, but exhausting ITS OWN
// RunQueueMaxAttempts (or hitting an AlreadyAttempted failure) and
// terminal-failing it before this loop's own next attempt.
type stealAndTerminalFailOnNackSessions struct {
	session.Service
	sessionID      string
	terminalFailed atomic.Bool
}

func (s *stealAndTerminalFailOnNackSessions) NackRunQueueEntry(ctx context.Context, id, leasedBy, lastError string) error {
	if err := s.Service.NackRunQueueEntry(ctx, id, leasedBy, lastError); err != nil {
		return err
	}
	leased, err := s.LeaseRunQueueEntry(context.Background(), s.sessionID, "external-process-pump", time.Minute)
	if err != nil || leased == nil {
		return nil
	}
	if termErr := s.TerminalFailRunQueueEntry(context.Background(), leased.ID, "external-process-pump"); termErr == nil {
		s.terminalFailed.Store(true)
	}
	return nil
}

// TestDrainSessionNow_ExternalProcessTerminalFailAfterLocalNackReportsFailure
// covers the branch the coordinator's review found collapsing into a false
// success: a DIFFERENT process's pump instance takes the row over after
// this process's own Nack, and TERMINAL-FAILS it (not merely holds it).
// The row is now permanently deleted and will NEVER be retried by anyone
// -- reporting a completed drain here would tell the operator their run
// finished when their accepted work was in fact permanently dropped,
// which is worse than surfacing the stale retryable error it replaces.
// From DrainSessionNow's own perspective this is indistinguishable from a
// genuine Ack (both are current == nil on re-check -- see this file's
// header and TestDrainSessionNow_ExternalProcessAckAfterLocalNackReportsUnconfirmed's
// own doc for why), which is exactly why the fix reports the conservative
// ErrRowOutcomeUnconfirmed for BOTH rather than guessing.
//
// Revert-check: change the `case current == nil:` branch in
// DrainSessionNow's re-check back to clearing the ledger entry and this
// fails on the require.Error below.
func TestDrainSessionNow_ExternalProcessTerminalFailAfterLocalNackReportsFailure(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p578-external-terminal-fail")
	wrapped := &stealAndTerminalFailOnNackSessions{Service: svc, sessionID: sess.ID}
	coord := &failOnceCoordinator{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       wrapped,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p578-external-terminal",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p578-external-terminal-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.True(t, wrapped.terminalFailed.Load(), "the external terminal-fail must have succeeded (deterministic, not timing-dependent) -- otherwise this test proves nothing")
	require.Equal(t, int64(1), coord.calls.Load(), "only the one local failing attempt should have run in this process")
	require.Equal(t, session.DrainFailed, result, "an execution genuinely happened in this process (the failed attempt) -- the row was terminal-failed by another process and will NEVER be retried")
	require.Error(t, drainErr, "a completed drain here is a false success over permanently lost work, worse than the stale error it replaces")
	require.ErrorIs(t, drainErr, session.ErrRowOutcomeUnconfirmed)
}

// TestDrainSessionNow_ExternalProcessAckAfterLocalNackReportsUnconfirmed is
// the control that proves the design decision is deliberate, not merely
// untested: even a GENUINE Ack by another process (a real, successful
// commit) reports DrainFailed/ErrRowOutcomeUnconfirmed through this call's
// re-check rather than a completed drain -- because AckRunQueueEntry and
// TerminalFailRunQueueEntry are both a plain DELETE ... RETURNING id query
// (see sql/run_queue.sql), and nothing survives in session_run_queue to
// tell DrainSessionNow which of the two actually happened once the row is
// gone. This call genuinely cannot confirm success here, so it must not
// report one -- app_run.go's RunNonInteractive would otherwise report exit
// code 0 for an outcome this call never actually observed.
//
// This is the operationally-conservative direction and its own cost: a
// real success gets reported as "could not confirm" rather than "success".
// That is accepted deliberately (see ErrRowOutcomeUnconfirmed's own doc) --
// the alternative direction (defaulting "gone" to success) would silently
// misreport a genuine terminal failure as success instead, which is worse.
//
// Deterministic: the ack happens synchronously inside the wrapped
// NackRunQueueEntry call, not via a background goroutine racing on timing.
func TestDrainSessionNow_ExternalProcessAckAfterLocalNackReportsUnconfirmed(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "p578-external-ack")
	wrapped := &stealAndAckOnNackSessions{Service: svc, sessionID: sess.ID}
	coord := &failOnceCoordinator{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       wrapped,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-p578-external-ack",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "p578-external-ack-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(ctx, sess.ID)

	require.True(t, wrapped.acked.Load(), "the external ack must have succeeded (deterministic, not timing-dependent) -- otherwise this test proves nothing")
	require.Equal(t, int64(1), coord.calls.Load(), "only the one local failing attempt should have run in this process")
	require.Equal(t, session.DrainFailed, result, "an execution genuinely happened in this process -- even a genuine external Ack cannot be confirmed as success from this call's own re-check")
	require.Error(t, drainErr, "Ack and TerminalFail are indistinguishable once the row is gone, and this call must not guess")
	require.ErrorIs(t, drainErr, session.ErrRowOutcomeUnconfirmed)
}

// stealAndAckOnNackSessions wraps a real session.Service and, the instant
// NackRunQueueEntry is called, synchronously leases the same row under a
// DIFFERENT leasedBy and immediately Acks it -- deterministically
// simulating a genuinely different process's pump instance not merely
// taking the row over, but running it to a genuine committed success
// before this loop's own next attempt.
type stealAndAckOnNackSessions struct {
	session.Service
	sessionID string
	acked     atomic.Bool
}

func (s *stealAndAckOnNackSessions) NackRunQueueEntry(ctx context.Context, id, leasedBy, lastError string) error {
	if err := s.Service.NackRunQueueEntry(ctx, id, leasedBy, lastError); err != nil {
		return err
	}
	leased, err := s.LeaseRunQueueEntry(context.Background(), s.sessionID, "external-process-pump", time.Minute)
	if err != nil || leased == nil {
		return nil
	}
	if _, ackErr := s.AckRunQueueEntry(context.Background(), leased.ID, "external-process-pump"); ackErr == nil {
		s.acked.Store(true)
	}
	return nil
}
