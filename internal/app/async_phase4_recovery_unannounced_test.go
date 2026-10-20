// TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck is the step-8 addition
// to doc sec.6's two-App scenario list: work claimed but never reaching the
// ack gate (announced=0 -- the host died between the claim and the "started"
// tool-result actually being persisted/acknowledged, doc sec.3.8's ack-gate
// paragraph) must be recovered by DELETING the row without a trace
// (DUR-6/DUR-7/ASYNC-05: an unannounced job can never produce a notice), not
// by interrupting it -- unlike scenario (а)'s announced=1 case, which gets a
// visible "interrupted" notice.
//
// Dual mode: flag OFF keeps the legacy async_jobs claim/crash/readback
// byte-for-byte. Flag ON (RUSH_BATCH_RUNTIME) seeds the SAME premise as a
// constraint-valid, announced=0 batch root + task child under the App's REAL
// HostStore (batch_nodes is the authority the design retains the deletion
// semantics for -- internal/session/batch_recovery.go's announced==0
// DeleteBatchSubtree branch); no async_jobs row is seeded, because ON never
// recovers that table.
//
// REVERT CHECK (legacy, re-run 2026-09-29, W-DRAIN task D): the deleting
// call is coordinator_reaction_source.go's ScopeOpen body -- app_run_async's
// CLI loop calls nextStep after EVERY turn including the first; nextStep
// finds no reaction debt (an unannounced row produces no notice, ASYNC-05)
// and falls through to source.ScopeOpen, whose body recovers the owner's
// scope (deleting the unannounced row) BEFORE reporting whether anything is
// still open -- so the row is gone by the time the loop decides "no next
// turn". The batch twin is recoverBatchScopeDeadCandidates in the same
// runningWorkOpen preamble (coordinator_reaction_source.go), which runs the
// BatchStore recovery (announced==0 -> DeleteBatchSubtree) before the fresh
// live read answers the scope.
package app

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestTwoAppScenarioD_AnnouncedZeroHostKilledBeforeAck(t *testing.T) {
	var requestsB atomic.Int32
	handlerA := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "A's provider must never be called in this scenario", http.StatusBadRequest)
	}
	handlerB := func(w http.ResponseWriter, r *http.Request) {
		switch requestsB.Add(1) {
		case 1:
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "call-unannounced") {
				// t.Errorf is goroutine-safe; require is not.
				t.Errorf("an unannounced job must leave no trace at all, not even a mention")
				http.Error(w, "unannounced call leaked into B's prompt", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("continue", "still fine"), admissionSSEStop("continue", "stop")})
		default:
			t.Errorf("unexpected model call on B (request %d)", requestsB.Load())
			http.Error(w, "unexpected model call on B", http.StatusBadRequest)
		}
	}

	appA, appB, sessionID := newRecoveryTwoAppHarness(t, handlerA, handlerB)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// rootID is the ON fixture's root id, in scope for the deletion oracle.
	var rootID string
	if !f6BatchOn() {
		// Legacy premise, byte-for-byte: claim directly on A's store,
		// standing in for async_tool.Run's Claim call -- then deliberately
		// skip MarkAnnounced (the tool-result write that would normally
		// follow within the same turn), modeling a host killed in the window
		// between the two.
		_, err := appA.asyncJobStore.Claim(ctx, session.ClaimParams{
			Owner: sessionID, ToolCallID: "call-unannounced", Kind: session.JobKindCommand,
			Input: "echo hi", ToolName: "bash",
		})
		require.NoError(t, err)
		row, err := appA.asyncJobStore.Get(ctx, sessionID, "call-unannounced")
		require.NoError(t, err)
		require.EqualValues(t, 0, row.Announced, "the row must be unannounced -- the crash happens before the ack gate")

		require.NoError(t, appA.asyncJobStore.SimulateCrashForTest())
	} else {
		// Batch premise: reserve an unannounced (announced=0) root + running
		// task child under A's real, lock-holding HostStore, then release
		// the lock through the crash seam -- a dead host owning live rows,
		// exactly the recovery candidate the batch rule deletes (announced
		// roots are interrupted instead; this one is not announced).
		var node string
		rootID, node = pp4ReserveUnannouncedRoot(t, appA, sessionID, "call-unannounced")
		require.NotEmpty(t, rootID)
		require.NotEmpty(t, node)
		pp4CrashSimulate(t, appA)
	}

	resB, err := appB.RunNonInteractiveWithResult(ctx, io.Discard, "continue please", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, resB)
	require.Equal(t, "end_turn", resB.ExitReason, "warnings=%v", resB.Warnings)
	// Exactly one turn: after it, the CLI loop's own nextStep ->
	// ScopeOpen/recovery preamble finds the work gone and reports scope
	// closed, so no second (empty-prompt Drain) turn is needed.
	require.EqualValues(t, 1, requestsB.Load(), "B's post-turn scope recovery must have deleted the unannounced work, closing the scope with no further turn needed")

	if f6BatchOn() {
		// The batch rule: the unannounced ROOT's whole subtree is deleted.
		fact, ok := pp4JobFactRead(appB, sessionID, "call-unannounced")
		require.True(t, ok)
		require.False(t, fact.Found, "an unannounced root from a dead host must be deleted, not interrupted")
		require.Zero(t, pp4BatchRowsForRoot(t, appB, rootID), "the whole unannounced subtree must be deleted, task child included")
	} else {
		_, err = appB.asyncJobStore.Get(ctx, sessionID, "call-unannounced")
		require.True(t, errors.Is(err, sql.ErrNoRows), "an unannounced row from a dead host must be deleted without a trace, not interrupted")
	}

	require.Zero(t, countBackgroundNotices(t, appB, sessionID, ""), "no notice may ever appear for work that never reached the ack gate")
}
