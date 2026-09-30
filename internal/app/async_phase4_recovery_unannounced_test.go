// TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck is the step-8 addition
// to doc sec.6's two-App scenario list: an async job claimed but never
// reaching the ack gate (announced=0 -- the host died between Claim and the
// "started" tool-result actually being persisted/acknowledged, doc sec.3.8's
// ack-gate paragraph) must be recovered by DELETING the row without a trace
// (DUR-6/DUR-7/ASYNC-05: an unannounced job can never produce a notice), not
// by interrupting it -- unlike scenario (а)'s announced=1 case, which gets a
// visible "interrupted" notice.
//
// REVERT CHECK (re-run 2026-09-29, W-DRAIN task D, docs/reviews/2026-09-29-
// async-phase4-round1.md: "TestTwoAppScenarioD ... REVERT CHECK note
// impossible"): the two previously-cited call sites -- agent_turn.go's
// turn-preamble RecoverOwnerScope call, and coordinator.RunMaintenanceSweep's
// SweepDeadHosts call -- were BOTH disabled (`if false &&`/`if false {}`)
// simultaneously; this test still PASSED. AsyncJobStore.ensureHost's
// first-registration sweep (internal/session/async_job_store.go) cannot be
// it either: App B never calls Claim in this test at all, so ensureHost is
// never reached on B's side. Isolated by ALSO disabling coordinator_reaction_
// source.go's ScopeOpen method's own `RecoverOwnerScope` call (its line
// ~204, distinct from the two above) with the other two still disabled --
// THIS made the test FAIL (`errors.Is(err, sql.ErrNoRows)` false: the row
// still existed). Re-enabling ONLY that one call (the other two still
// disabled) made it PASS again. The mechanism is: app_run_async.go's CLI
// loop calls nextStep after EVERY turn, including the first;
// nextStep finds no reaction debt (an unannounced row produces no
// notice at all, matching ASYNC-05) and falls through to source.ScopeOpen,
// whose own body (coordinator_reaction_source.go's ScopeOpen method)
// recovers the owner's scope (deleting this unannounced row) BEFORE
// reporting whether anything is still open -- so the row is gone by the time
// the loop decides "no next turn", never before the FIRST turn's own prompt
// was built (confirmed live: the row still existed, via a direct Get, at the
// moment the first and only HTTP request reached the provider). The old
// note's specific factual claims (both the original "ScopeOpen call alone"
// and the retraction's "ensureHost's sweep") are retracted; this is the
// real, proven, isolated mechanism as of the current code.
package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck(t *testing.T) {
	var requestsB int
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "A's provider must never be called in this scenario", http.StatusBadRequest)
	}
	handlerB := func(w http.ResponseWriter, r *http.Request) {
		requestsB++
		switch requestsB {
		case 1:
			body, _ := io.ReadAll(r.Body)
			require.NotContains(t, string(body), "call-unannounced",
				"an unannounced job must leave no trace at all, not even a mention")
			admissionWriteSSE(w, []string{admissionSSEText("continue", "still fine"), admissionSSEStop("continue", "stop")})
		default:
			http.Error(w, "unexpected model call on B", http.StatusBadRequest)
		}
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Claim directly on A's store, standing in for async_tool.Run's Claim
	// call -- then deliberately skip MarkAnnounced (the tool-result write
	// that would normally follow within the same turn), modeling a host
	// killed in the window between the two.
	_, err := appA.asyncJobStore.Claim(ctx, session.ClaimParams{
		Owner: sessionID, ToolCallID: "call-unannounced", Kind: session.JobKindCommand,
		Input: "echo hi", ToolName: "bash",
	})
	require.NoError(t, err)
	row, err := appA.asyncJobStore.Get(ctx, sessionID, "call-unannounced")
	require.NoError(t, err)
	require.EqualValues(t, 0, row.Announced, "the row must be unannounced -- the crash happens before the ack gate")

	require.NoError(t, appA.asyncJobStore.SimulateCrashForTest())

	resB, err := appB.RunNonInteractiveWithResult(ctx, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resB)
	require.Equal(t, "end_turn", resB.ExitReason, "warnings=%v", resB.Warnings)
	// Exactly one turn: after it, the CLI loop's own nextStep ->
	// ScopeOpen recovery call (see REVERT CHECK above) finds the row gone and
	// reports scope closed, so no second (empty-prompt Drain) turn is needed.
	require.Equal(t, 1, requestsB, "B's post-turn scope recovery (ScopeOpen's own RecoverOwnerScope call) must have deleted the row, closing the scope with no further turn needed")

	_, err = appB.asyncJobStore.Get(ctx, sessionID, "call-unannounced")
	require.True(t, errors.Is(err, sql.ErrNoRows), "an unannounced row from a dead host must be deleted without a trace, not interrupted")

	require.Zero(t, countBackgroundNotices(t, appB, sessionID, ""), "no notice may ever appear for a job that never reached the ack gate")
}
