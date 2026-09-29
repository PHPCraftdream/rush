// TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck is the step-8 addition
// to doc sec.6's two-App scenario list: an async job claimed but never
// reaching the ack gate (announced=0 -- the host died between Claim and the
// "started" tool-result actually being persisted/acknowledged, doc sec.3.8's
// ack-gate paragraph) must be recovered by DELETING the row without a trace
// (DUR-6/DUR-7/ASYNC-05: an unannounced job can never produce a notice), not
// by interrupting it -- unlike scenario (а)'s announced=1 case, which gets a
// visible "interrupted" notice.
//
// REVERT CHECK (re-run 2026-09-29 by the `drain` agent, docs/reviews/2026-
// 09-29-async-phase4-round1.md's W-DRAIN item "TestTwoAppScenarioD ...
// REVERT CHECK note impossible"): the note that used to be here claimed
// disabling coordinator_reaction_source.go's ScopeOpen -> RecoverOwnerScope
// call alone made this test fail. Re-ran that exact probe against the
// current code (ScopeOpen's call commented out, agent_turn.go's own
// turn-preamble call left intact): the test still PASSED. Also tried the
// reverse (ScopeOpen's call intact, agent_turn.go's turn-preamble call
// disabled) and BOTH disabled together: the test PASSED in every
// combination. So neither of the two commonly-cited recovery call sites is
// what actually deletes `call-unannounced` here -- this test does not
// exercise agent_turn.go's turn-start recovery OR ScopeOpen's own recovery
// call at all, and would not catch either one being silently broken. The
// row disappears via a THIRD path this test never named: AsyncJobStore.
// ensureHost's own first-registration sweep (internal/session/
// async_job_store.go, "the process that actually performs registration
// runs ONE sweep over every dead host right after") — internal/session is
// outside this agent's file scope (store agent's), so tracing the exact
// trigger further and re-anchoring this test to the mechanism it actually
// intends to pin is left as follow-up, not done here. The old note's
// specific factual claim is retracted as false for the current code.
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
	require.Equal(t, 1, requestsB, "B's own scope recovery (RecoverOwnerScope, called both from ScopeOpen and from the turn preamble) must have already deleted the row before the prompt was ever built")

	_, err = appB.asyncJobStore.Get(ctx, sessionID, "call-unannounced")
	require.True(t, errors.Is(err, sql.ErrNoRows), "an unannounced row from a dead host must be deleted without a trace, not interrupted")

	require.Zero(t, countBackgroundNotices(t, appB, sessionID, ""), "no notice may ever appear for a job that never reached the ack gate")
}
