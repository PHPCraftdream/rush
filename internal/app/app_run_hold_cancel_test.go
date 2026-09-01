package app

// R3-3 admission-hold cancellation barrier: a mailbox-directed
// Cancel(sessionID) landing BETWEEN the mailbox reservation and the turn
// handoff must abort the run BEFORE any pre-handoff session mutation
// (system prompt, reasoning effort, budget, timeout, ended reason, title,
// message count), BEFORE the provider is ever hit, and release the
// reservation exactly once — so a follow-up run on the same session
// succeeds cleanly afterwards.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

// holdWindowCoordinator parks ExecuteRun inside ReserveExclusive: the
// first successful reservation closes reserved and blocks on proceed,
// giving the test a deterministic window in which the mailbox cancel
// lands before any pre-handoff mutation can run.
type holdWindowCoordinator struct {
	agent.Coordinator
	reserved chan struct{} // closed when the first reservation is claimed
	proceed  chan struct{} // closed by the test to release the parked ExecuteRun
	parkOnce sync.Once
	releases atomic.Int32 // coordinator-level ReleaseExclusive call count
}

func (h *holdWindowCoordinator) ReserveExclusive(ctx context.Context, sessionID string) (context.Context, uint64, context.CancelFunc, bool) {
	holdCtx, epoch, cancel, ok := h.Coordinator.ReserveExclusive(ctx, sessionID)
	if !ok {
		return holdCtx, epoch, cancel, ok
	}
	h.parkOnce.Do(func() { close(h.reserved); <-h.proceed })
	return holdCtx, epoch, cancel, ok
}

func (h *holdWindowCoordinator) ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc) {
	h.releases.Add(1)
	h.Coordinator.ReleaseExclusive(sessionID, epoch, cancel)
}

// TestExecuteRunMailboxCancelDuringAdmissionHoldAborts proves that a
// Cancel(sessionID) delivered while ExecuteRun holds its reservation
// aborts the run before any mutation and before the turn starts.
func TestExecuteRunMailboxCancelDuringAdmissionHoldAborts(t *testing.T) {
	var seenMu sync.Mutex
	var seen []string
	record := func(label string) {
		seenMu.Lock()
		seen = append(seen, label)
		seenMu.Unlock()
	}
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "R33_FOLLOWUP"):
			record("followup")
		case strings.Contains(string(body), "R33_CANCELLED"):
			record("cancelled")
		default:
			record("unexpected")
		}
		admissionWriteSSE(w, []string{
			admissionSSEText("c1", "R33_FOLLOWUP_DONE"),
			admissionSSEStop("c1", "stop"),
		})
	})
	h := &holdWindowCoordinator{
		Coordinator: application.AgentCoordinator,
		reserved:    make(chan struct{}),
		proceed:     make(chan struct{}),
	}
	application.AgentCoordinator = h

	// Make the Rename mutation observable: ExecuteRun's auto-title only
	// fires when the title is "", "Untitled Session", or the session id —
	// so a non-gated run WOULD rewrite it to the prompt.
	require.NoError(t, application.Sessions.Rename(context.Background(), sessionID, sessionID))

	before, err := application.Sessions.Get(context.Background(), sessionID)
	require.NoError(t, err)

	outcomes := make(chan admissionOutcome, 1)
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start, 1,
		"R33_CANCELLED this must never run", RunOverrides{
			SystemPrompt:    "R33_SYSTEM_PROMPT_MUST_NOT_LAND",
			ReasoningEffort: "high",
			RoleSmart:       true,
			MaxCost:         42.5,
			MaxTokens:       4242,
			Timeout:         424 * time.Second,
		}, true)
	close(start)

	select {
	case <-h.reserved:
	case <-time.After(60 * time.Second):
		t.Fatal("the victim never claimed its reservation; cannot stage the hold window")
	}

	// Fire the mailbox-directed cancellation while the victim is parked
	// inside the reservation window. This goes through the embedded real
	// coordinator — sessionAgent.Cancel fires the hold's placeholder
	// cancel; mailbox state/epoch stay ours.
	application.AgentCoordinator.Cancel(sessionID)

	// Release the park and collect the victim's outcome.
	close(h.proceed)
	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.Error(t, o.err, "the parked victim must fail once the hold is canceled")
		require.ErrorIs(t, o.err, context.Canceled, "the bail-out must wrap context.Canceled")
		require.ErrorContains(t, o.err, "canceled during run admission")
		require.Nil(t, o.res, "a canceled admission hold must produce no run result")
	case <-time.After(60 * time.Second):
		t.Fatal("the victim neither failed nor returned after the hold was canceled")
	}

	// No-mutation assertions: the bail-out happens before every
	// pre-handoff mutation, so the session must be byte-identical.
	after, err := application.Sessions.Get(context.Background(), sessionID)
	require.NoError(t, err)
	require.Equal(t, before.ID, after.ID)
	require.Equal(t, before.SystemPrompt, after.SystemPrompt,
		"canceled hold must not rewrite the system prompt")
	require.Equal(t, before.SmartModelReasoningEffort, after.SmartModelReasoningEffort,
		"canceled hold must not rewrite reasoning effort")
	require.Equal(t, before.BudgetMaxCost, after.BudgetMaxCost,
		"canceled hold must not rewrite the cost budget")
	require.Equal(t, before.BudgetMaxTokens, after.BudgetMaxTokens,
		"canceled hold must not rewrite the token budget")
	require.Equal(t, before.BudgetTimeoutSec, after.BudgetTimeoutSec,
		"canceled hold must not rewrite the timeout")
	require.Equal(t, before.EndedReason, after.EndedReason,
		"canceled hold must not rewrite ended_reason")
	require.Equal(t, before.Title, after.Title,
		"canceled hold must not rename the session")
	require.Equal(t, before.MessageCount, after.MessageCount,
		"canceled hold must not add a user message")

	// Exactly-once release, and the turn must never have started.
	require.Equal(t, int32(1), h.releases.Load(),
		"the bail-out must release the reservation exactly once")
	seenMu.Lock()
	require.Empty(t, seen,
		"no provider request may arrive for a canceled admission hold — the turn must not start")
	seenMu.Unlock()

	// The session must not be stuck: a follow-up run on the same session
	// (FailIfSessionBusy) succeeds and genuinely reaches the provider.
	res, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "R33_FOLLOWUP answer me",
		Mode:              RunModeJSON,
		ContinueSessionID: sessionID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
		FailIfSessionBusy: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, "R33_FOLLOWUP_DONE", res.FinalText)
	seenMu.Lock()
	require.Equal(t, []string{"followup"}, seen,
		"the only provider request must be the follow-up's")
	seenMu.Unlock()
}
