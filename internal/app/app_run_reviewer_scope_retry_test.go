// C16 / R2C-6 (docs/reviews/2026-09-29-async-phase4-round1.md, 2026-09-30-
// async-phase4-round2.md): the automatic reviewer-pass gate (app_run.go)
// decides from CLIScope: running work and an owed/paced/stuck reaction block
// the pass (one stderr line each), debt the policy defers does not, and a DB
// read error retries with a pause instead of silently skipping the pass on the
// very first failure. The real end-to-end case (deferred debt, reviewer runs)
// is TestRunNonInteractive_ReviewerRunsWithDeferredDebt.
package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

// scopeAnswer is one scripted CLIScope answer.
type scopeAnswer struct {
	state agent.CLIScopeState
	err   error
}

// scriptedScopeSource answers CLIScope from a script (the last entry repeats)
// and counts its calls and WaitForHint waits.
type scriptedScopeSource struct {
	script []scopeAnswer
	calls  atomic.Int32
	waits  atomic.Int32
}

func (f *scriptedScopeSource) ClaimExternalDriver(context.Context, string) error { return nil }
func (f *scriptedScopeSource) ReleaseExternalDriver(context.Context, string)     {}
func (f *scriptedScopeSource) RunMaintenanceSweep(context.Context)               {}
func (f *scriptedScopeSource) WaitForHint(context.Context, string, time.Time)    { f.waits.Add(1) }
func (f *scriptedScopeSource) CLIScope(context.Context, string) (agent.CLIScopeState, error) {
	i := int(f.calls.Add(1)) - 1
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	return f.script[i].state, f.script[i].err
}

func TestReviewerPassBlocked_ScopeDecides(t *testing.T) {
	cases := []struct {
		name    string
		state   agent.CLIScopeState
		blocked bool
	}{
		{"nothing outstanding", agent.CLIScopeState{}, false},
		{"debt the policy defers does not block", agent.CLIScopeState{Drain: agent.DrainDeferred}, false},
		{"running work blocks", agent.CLIScopeState{WorkOpen: true}, true},
		{"an owed reaction blocks", agent.CLIScopeState{Drain: agent.DrainOwed}, true},
		{"a retried reaction blocks", agent.CLIScopeState{Drain: agent.DrainPaced}, true},
		{"a stuck reaction blocks", agent.CLIScopeState{Drain: agent.DrainStuck}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &scriptedScopeSource{script: []scopeAnswer{{state: tc.state}}}
			require.Equal(t, tc.blocked, (&App{}).reviewerPassBlocked(context.Background(), fake, "sess-1", io.Discard))
			require.EqualValues(t, 1, fake.calls.Load(), "no retry overhead when the scope just answers")
		})
	}
}

// TestReviewerPassBlocked_RecoversWithinRetryBudget: a DB error on the first
// attempt must not be treated as "skip the reviewer pass" -- the second
// attempt's real answer wins.
//
// Revert-check: returning "blocked" on the first read error turns this red.
func TestReviewerPassBlocked_RecoversWithinRetryBudget(t *testing.T) {
	orig := cliDBRetryPause
	cliDBRetryPause = time.Millisecond
	t.Cleanup(func() { cliDBRetryPause = orig })

	fake := &scriptedScopeSource{script: []scopeAnswer{{err: errors.New("database is locked")}, {}}}
	require.False(t, (&App{}).reviewerPassBlocked(context.Background(), fake, "sess-1", io.Discard))
	require.EqualValues(t, 2, fake.calls.Load())
}

// TestReviewerPassBlocked_PersistentFailureDefaultsToBlocked: once every retry
// is exhausted the conservative default is "blocked" rather than running an
// extra phase against unknown scope state.
func TestReviewerPassBlocked_PersistentFailureDefaultsToBlocked(t *testing.T) {
	orig := cliDBRetryPause
	cliDBRetryPause = time.Millisecond
	t.Cleanup(func() { cliDBRetryPause = orig })

	fake := &scriptedScopeSource{script: []scopeAnswer{{err: errors.New("database is locked")}}}
	require.True(t, (&App{}).reviewerPassBlocked(context.Background(), fake, "sess-1", io.Discard))
	require.EqualValues(t, reviewerPassScopeOpenRetries, fake.calls.Load(), "retries are bounded")
}

// TestReviewerPassBlocked_WritesToTheRunsStderr is R3C-9: the skip line goes
// to the writer the run was given (SDK callers pass RunRequest.Stderr), never
// to the process's os.Stderr.
//
// Revert-check: writing the lines to os.Stderr again leaves both buffers empty
// and this test red.
func TestReviewerPassBlocked_WritesToTheRunsStderr(t *testing.T) {
	orig := cliDBRetryPause
	cliDBRetryPause = time.Millisecond
	t.Cleanup(func() { cliDBRetryPause = orig })

	var blocked bytes.Buffer
	fake := &scriptedScopeSource{script: []scopeAnswer{{state: agent.CLIScopeState{WorkOpen: true}}}}
	require.True(t, (&App{}).reviewerPassBlocked(context.Background(), fake, "sess-1", &blocked))
	require.Contains(t, blocked.String(), `reviewer pass skipped for session "sess-1": it still has running work`)

	var unreadable bytes.Buffer
	fake = &scriptedScopeSource{script: []scopeAnswer{{err: errors.New("database is locked")}}}
	require.True(t, (&App{}).reviewerPassBlocked(context.Background(), fake, "sess-1", &unreadable))
	require.Contains(t, unreadable.String(), "its scope could not be read (database is locked)")

	var open bytes.Buffer
	fake = &scriptedScopeSource{script: []scopeAnswer{{}}}
	require.False(t, (&App{}).reviewerPassBlocked(context.Background(), fake, "sess-1", &open))
	require.Empty(t, open.String(), "an open scope prints nothing")
}
