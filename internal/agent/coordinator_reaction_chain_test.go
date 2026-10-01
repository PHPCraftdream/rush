// The reaction chain guard (#1113): N=3 consecutive no-progress links whose
// entire debt is their own idle launches' completions defer the session's
// automatic turns. Counting happens in accountDrainAttempt (the ONE
// accounting point of a leg), the verdict in drainPolicy/chainGuardDeferred,
// and the counter lives in the coordinator -- NOT in the launch gate,
// which a successful reaction (rows.open == 0) resets with exactly the event
// that masks the chain (the observed bug).
package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// chainLink feeds one finished leg through the real accounting path:
// outcome=drainAttempted, no error, the leg's snapshot naming the rows of
// snapClaims (the claims of PREVIOUS legs' launches -- each leg reacts to the
// completions that arrived before it) and the leg's own idle-launch claims.
func (f *attemptFixture) chainLink(ctx context.Context, toolCallID string, snapClaims []string, idle, progress bool) string {
	f.t.Helper()
	f.seedDebt(ctx, toolCallID, true)
	row := f.row(ctx, toolCallID)
	snap := session.DebtSnapshot{}
	for _, c := range snapClaims {
		snap.Jobs = append(snap.Jobs, session.DebtJobRef{ClaimID: c})
	}
	att := &drainAttempt{sessionID: f.sessID, outcome: drainAttempted, snapshot: snap}
	if idle {
		att.chainIdleClaims = []string{row.ClaimID}
	}
	att.chainProgress = progress
	f.coord.accountDrainAttempt(ctx, att, nil)
	return row.ClaimID
}

func (f *attemptFixture) chainCount() int {
	f.t.Helper()
	return f.coord.reactionChainCount(f.sessID)
}

// Three idle legs whose snapshots are only their own completions: the third
// defers (drainPolicy) and the CLI scope reports it through the typed flag.
//
// Revert-checks: dropping the count++ (a link never increments) leaves the
// third verdict Allow; dropping the "every row is a chain claim" comparison
// makes the W-row case below defer too.
func TestReactionChain_ThreeLinksDefer(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "reaction-chain", attemptFixtureOpts{noIdle: true, noDriver: true})
	c1 := f.chainLink(ctx, "chain1", nil, true, false)
	c2 := f.chainLink(ctx, "chain2", []string{c1}, true, false)
	_ = f.chainLink(ctx, "chain3", []string{c1, c2}, true, false)
	require.Equal(t, 3, f.chainCount())

	v := f.coord.drainPermitted(ctx, f.sessID, false)
	require.Equal(t, drainDeferred, v.kind)
	require.Equal(t, reactionChainReason, v.reason)

	state, err := f.coord.CLIScope(ctx, f.sessID)
	require.NoError(t, err)
	require.Equal(t, DrainDeferred, state.Drain)
	require.True(t, state.ChainGuard)

	// A row from another fact in the debt: allow.
	f.seedDebt(ctx, "workrow", true)
	state, err = f.coord.CLIScope(ctx, f.sessID)
	require.NoError(t, err)
	require.Equal(t, DrainOwed, state.Drain)
	require.False(t, state.ChainGuard)
}

// A successful reaction (rows.open == 0 -- the snapshot's rows are gone) does
// NOT reset the count: closing the chain's own completions is the event that
// masks it. Revert-check: moving the counter into the gate reset (or
// resetting it on rows.open == 0) turns this red.
func TestReactionChain_SuccessfulReactionDoesNotReset(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "reaction-chain-reset", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.chainLink(ctx, "chain1", nil, true, false)
	require.Equal(t, 1, f.chainCount())

	att := &drainAttempt{sessionID: f.sessID, outcome: drainAttempted}
	f.coord.accountDrainAttempt(ctx, att, nil) // empty snapshot: rows.open == 0
	require.Equal(t, 1, f.chainCount())
}

// Progress resets; real bash and neutral-only legs behave per design; a human
// message (ResetAutoResumeCounter) clears the guard entirely.
func TestReactionChain_Resets(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "reaction-chain-resets", attemptFixtureOpts{noIdle: true, noDriver: true})

	// echo + edit in one leg: progress resets (and clears the claim set).
	c1 := f.chainLink(ctx, "chain1", nil, true, false)
	c2 := f.chainLink(ctx, "chain2", []string{c1}, true, false)
	f.chainLink(ctx, "chain3", []string{c2}, true, true)
	require.Zero(t, f.chainCount())
	require.False(t, f.coord.reactionChainHasClaim(f.sessID, c1))

	// bash `cat f`: a real command is progress (same reset path, covered by
	// classification below); text + echo is a LINK: a leg with an idle
	// launch answering another fact starts a fresh chain at 1.
	// Revert-check: counting the leg's text as progress makes this zero.
	c4 := f.chainLink(ctx, "chain4", []string{"foreign-claim"}, true, false)
	require.Equal(t, 1, f.chainCount())
	require.True(t, f.coord.reactionChainHasClaim(f.sessID, c4))

	// Two more links: the guard fires; a human message re-arms.
	c5 := f.chainLink(ctx, "chain5", []string{c4}, true, false)
	f.chainLink(ctx, "chain6", []string{c4, c5}, true, false)
	// The progress leg above really reacted the rows it saw (the model's own
	// step does); model that, or those stale rows (claims outside the chain's
	// set) rightly keep the policy out of the way.
	f.exec(ctx, `UPDATE async_jobs SET reacted=1 WHERE owner_session_id='`+f.sessID+`' AND tool_call_id IN ('chain1','chain2','chain3')`)
	v := f.coord.drainPermitted(ctx, f.sessID, false)
	require.Equal(t, drainDeferred, v.kind)
	f.coord.ResetAutoResumeCounter(f.sessID)
	require.Zero(t, f.chainCount())
	state, err := f.coord.CLIScope(ctx, f.sessID)
	require.NoError(t, err)
	require.Equal(t, DrainOwed, state.Drain)
}

// The idle sweep must not free the guard's state: a session whose ledger
// entry holds nothing is still deferred. Revert-check: keeping the counter in
// sessionJobs (the ledger) makes this an Allow after the sweep.
func TestReactionChain_SurvivesIdleSweep(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "reaction-chain-sweep", attemptFixtureOpts{noIdle: true, noDriver: true})
	c1 := f.chainLink(ctx, "chain1", nil, true, false)
	c2 := f.chainLink(ctx, "chain2", []string{c1}, true, false)
	f.chainLink(ctx, "chain3", []string{c1, c2}, true, false)
	require.Equal(t, drainDeferred, f.coord.drainPermitted(ctx, f.sessID, false).kind)

	// Give the session an expired pause (an otherwise idle arbiter entry)
	// and run the 60s pass's sweeps. The guard's state (chainLinks) must
	// keep the entry alive: an idle sweep frees only idle entries.
	f.coord.seedArbiterState(f.sessID, func(s *arbiterState) {
		s.gate.RetryAt = time.Now().Add(-time.Second)
	})
	f.ledger.sweepIdleSessions()
	f.coord.sweepArbiterEntries()
	require.Equal(t, 3, f.coord.reactionChainCount(f.sessID), "the sweep must not free the guard's state")
	require.Equal(t, drainDeferred, f.coord.drainPermitted(ctx, f.sessID, false).kind)
}

// Web parity: only wakeSession drives the session -- the guard still stops it
// after 3, and exactly one marker notice is inserted.
func TestReactionChain_WebWakeStopsAfterThree(t *testing.T) {
	ctx := context.Background()
	f, runs := newBGShellCapFixture(t, "reaction-chain-web", attemptFixtureOpts{})

	c1 := f.chainLink(ctx, "chain1", nil, true, false)
	c2 := f.chainLink(ctx, "chain2", []string{c1}, true, false)
	f.chainLink(ctx, "chain3", []string{c1, c2}, true, false)

	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, false))
	f.coord.waitRecheckWakes()
	require.Zero(t, runs.runs.Load(), "the guard defers the wake")
	notices, err := f.store.ListSessionNotices(ctx, f.sessID)
	require.NoError(t, err)
	markers := 0
	for _, n := range notices {
		if n.Kind == session.NoticeKindReactionChain {
			markers++
		}
	}
	require.Equal(t, 1, markers, "exactly one marker notice")

	// A human message re-arms (gate cleared with everything else): the debt
	// gets its turn.
	f.coord.ResetAutoResumeCounter(f.sessID)
	require.NoError(t, f.coord.wakeSession(ctx, f.sessID, false))
	require.Eventually(t, func() bool { return runs.runs.Load() == 1 }, 10*time.Second, 5*time.Millisecond)
}

// A supervision tick's drain pulling a chain link must not reset the
// supervision backoff (else the no-progress pause after 6 ticks never
// arrives). Revert-check: removing the chain-claim exclusion in
// pullPendingNotices resets tickCount to 0 and fails the first assertion.
func TestReactionChain_PulledLinkDoesNotResetSupervision(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "reaction-chain-supervision", attemptFixtureOpts{noIdle: true, noDriver: true})
	l := f.ledger
	l.supervision = newSupervisionRegistry()
	l.noteWorkStarted(ctx, f.sessID)

	claim := f.chainLink(ctx, "chain1", nil, true, false)
	sr := l.supervision
	sr.mu.Lock()
	sr.byRoot[f.sessID].tickCount = 5
	sr.mu.Unlock()

	f.sa.pullPendingNotices(ctx, f.sessID) // the chain link's completion
	sr.mu.Lock()
	require.Equal(t, 5, sr.byRoot[f.sessID].tickCount, "a chain link is not supervision progress")
	sr.mu.Unlock()

	// A completion outside the chain still resets.
	f.seedDebt(ctx, "other", false)
	f.sa.pullPendingNotices(ctx, f.sessID)
	sr.mu.Lock()
	require.Zero(t, sr.byRoot[f.sessID].tickCount, "a real completion is progress")
	sr.mu.Unlock()
	_ = claim
}

// Step classification: a bash sleep/echo launch is an idle claim, a real
// command is progress, run_command sleep is idle, job_output/todos are
// neutral, text-only is nothing. Revert-check: counting a redirect-bearing or
// substituted command as idle turns the `echo x > f` case red; counting the
// leg's text as progress turns the text-only case red.
func TestRecordChainEvidence_ClassifiesSteps(t *testing.T) {
	ts := &turnStream{att: &drainAttempt{}}
	startedTag := `{"async":true,"job_id":"j1","status":"running","claim_id":"claim-A"}`
	mk := func(calls []fantasy.ToolCallContent, results []fantasy.ToolResultContent) fantasy.StepResult {
		content := fantasy.ResponseContent{}
		for _, c := range calls {
			content = append(content, c)
		}
		for _, r := range results {
			content = append(content, r)
		}
		return fantasy.StepResult{Response: fantasy.Response{Content: content}}
	}
	call := func(id, name, input string) fantasy.ToolCallContent {
		return fantasy.ToolCallContent{ToolCallID: id, ToolName: name, Input: input}
	}
	res := func(id, meta string) fantasy.ToolResultContent {
		return fantasy.ToolResultContent{ToolCallID: id, ClientMetadata: meta}
	}

	// A bash idle launch and a real edit: one claim, progress.
	ts.recordChainEvidence(mk(
		[]fantasy.ToolCallContent{call("c1", "bash", `{"command":"echo tick","run_in_background":true}`), call("c2", "edit", `{}`)},
		[]fantasy.ToolResultContent{res("c1", startedTag), res("c2", "")},
	))
	require.Equal(t, []string{"claim-A"}, ts.att.chainIdleClaims)
	require.True(t, ts.att.chainProgress)

	// A real bash command's claim is NOT idle.
	ts.att = &drainAttempt{}
	ts.recordChainEvidence(mk(
		[]fantasy.ToolCallContent{call("c1", "bash", `{"command":"sleep 90; gh run view"}`)},
		[]fantasy.ToolResultContent{res("c1", startedTag)},
	))
	require.Empty(t, ts.att.chainIdleClaims)
	require.True(t, ts.att.chainProgress)

	// sleep 5 && echo done IS idle; run_command sleep is; job_output and
	// todos are neutral; a text-only step is nothing.
	ts.att = &drainAttempt{}
	ts.recordChainEvidence(mk(
		[]fantasy.ToolCallContent{call("c1", "bash", `{"command":"sleep 5 && echo done"}`), call("c2", "run_command", `{"program":"sleep","args":["90"]}`), call("c3", "job_output", `{}`), call("c4", "todos", `{}`)},
		[]fantasy.ToolResultContent{res("c1", startedTag), res("c2", startedTag)},
	))
	require.Equal(t, []string{"claim-A", "claim-A"}, ts.att.chainIdleClaims)
	require.False(t, ts.att.chainProgress)

	ts.att = &drainAttempt{}
	ts.recordChainEvidence(mk(nil, nil))
	require.Empty(t, ts.att.chainIdleClaims)
	require.False(t, ts.att.chainProgress)
}
