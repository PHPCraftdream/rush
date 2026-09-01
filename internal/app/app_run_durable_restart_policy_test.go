package app

// F2 regression pin (docs/reviews/2026-09-01-sdk-review-fh.md): a queued
// call that gets durably orphaned — the owner's turn loop exits on a
// terminal provider error and runOwned's abandonOwnershipWithHandoff
// finalizer pops the mailbox queue into the session_run_queue table — is
// rebuilt by the pump WITHOUT its in-process restricted-run policy
// (SessionAgentCall.RunAllowlist is json:"-"). Under the pre-fix code the
// restarted turn fell through to the process-wide runAllowlistGate,
// which has no production writer since R2-1, so its zero value applied:
// the still-auto-approved session blanket-approved EVERYTHING, whatever
// restriction the queued caller had declared. The fix arms a session
// BASELINE (permission.SessionRunAllowlistBaselineManager) from each
// ExecuteRun's own compiled policy; the restart must then be judged by
// that baseline, never by an unrestricted fallback.
//
// Scenario (mirrors the review's concrete scenario, tightened): run 1
// (owner, P1 = --allow-bash probe_cmd_one) is parked mid-turn; run 2
// (P2 = --allow-bash probe_cmd_two) legacy-queues behind it. The owner's
// turn then dies on a terminal provider error (HTTP 400 →
// classifyProviderError classTerminal, no retry), the loop exits, and
// the finalizer durably enqueues the queued call. The pump rebuilds it
// policy-less and runs it. Two gated bash probes discriminate:
//   - probe_cmd_two r1 (call_q1): allowed by P2 — the queued caller's
//     OWN policy, which is what its ExecuteRun armed as the baseline.
//     Must be GRANTED: the restart still functions under its caller's
//     declared restriction (pre-fix it was also granted, but for the
//     wrong reason — blanket approval).
//   - probe_cmd_neither r2 (call_q2): allowed by NEITHER policy. Must
//     be DENIED. Pre-fix this exact request was blanket-GRANTED — that
//     grant is the bug.
//
// A third provider round would mean the denial did not stop the turn.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestExecuteRunDurablyOrphanedQueuedCallRunsUnderSessionBaseline(t *testing.T) {
	var (
		gate1         = make(chan struct{})
		started       = make(chan struct{})
		startOnce     sync.Once
		restartRounds atomic.Int64
	)

	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "QUEUED_TURN_MARKER"):
			// The durably restarted queued call: round 1 asks for a
			// command P2 allows; round 2 (recognized by the previous
			// round's tool-call id in the request history) asks for
			// one nobody allows.
			if strings.Contains(string(body), "call_q1") {
				restartRounds.Add(1)
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cq2", "call_q2", "bash", `{"command":"probe_cmd_neither r2","description":"restart round 2"}`),
					admissionSSEStop("cq2", "tool_calls"),
				})
				return
			}
			restartRounds.Add(1)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("cq1", "call_q1", "bash", `{"command":"probe_cmd_two r1","description":"restart round 1"}`),
				admissionSSEStop("cq1", "tool_calls"),
			})
		case strings.Contains(string(body), "OWNER_TURN_MARKER"):
			// The owner: block mid-turn until the queued call has
			// landed in the mailbox, then fail TERMINALLY (HTTP 400 →
			// classifyProviderError classTerminal, no retry) so the
			// turn loop exits on a non-cancel error and the finalizer
			// orphans the queued call into the durable run queue.
			startOnce.Do(func() { close(started) })
			<-gate1
			http.Error(w, "terminal provider failure", http.StatusBadRequest)
		default:
			admissionWriteSSE(w, []string{admissionSSEText("c0", "IGNORED"), admissionSSEStop("c0", "stop")})
		}
	}

	application, sessionID := newAdmissionRaceApp(t, handler)
	require.NotNil(t, application.RunQueuePump,
		"this test drives the durable restart through the production pump; the app must have started one")

	// Collect DECIDED permission notifications, keyed by tool-call id.
	decided := make(chan permission.PermissionNotification, 16)
	notifCtx, cancelNotif := context.WithCancel(context.Background())
	defer cancelNotif()
	go func() {
		for ev := range application.Permissions.SubscribeNotifications(notifCtx) {
			n := ev.Payload
			if n.Granted || n.Denied {
				decided <- n
			}
		}
	}()

	outcomes := make(chan admissionOutcome, 2)

	// Call 1 (owner): restricted, allows only probe_cmd_one.
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start, 1, "owner turn OWNER_TURN_MARKER", RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"probe_cmd_one"},
	}, false)
	close(start)
	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("owner call never reached the provider; cannot stage the mid-turn owner")
	}

	// Call 2 (queued): restricted, allows only probe_cmd_two. Its
	// ExecuteRun preamble arms the session baseline = P2 while the owner
	// is provably still parked mid-turn.
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2, "queued turn QUEUED_TURN_MARKER", RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"probe_cmd_two"},
	}, false)
	close(start2)

	// Wait for the queued call to land in the mailbox's submitted queue
	// BEFORE failing the owner (same anti-orphaning rationale as the
	// legacy-queueing sibling tests): the owner is blocked on gate1, so
	// it cannot reach its own drain; once QueuedPrompts reports the
	// call, the upcoming loop exit is guaranteed to orphan it via the
	// finalizer rather than run it in-loop.
	require.Eventually(t, func() bool {
		return application.AgentCoordinator.QueuedPrompts(sessionID) >= 1
	}, 10*time.Second, time.Millisecond, "the queued call never reached the mailbox's submitted queue")

	// The queueing caller returns as soon as its call is queued.
	select {
	case o := <-outcomes:
		require.Equal(t, 2, o.idx)
		require.NoError(t, o.err, "legacy queueing call must not fail")
		require.NotNil(t, o.res)
	case <-time.After(60 * time.Second):
		t.Fatal("queued call did not return while the owner was still mid-turn")
	}

	// Fail the owner terminally. Its loop must exit with an error, and
	// runOwned's abandonOwnershipWithHandoff finalizer must pop the
	// queued call and durably enqueue it.
	close(gate1)
	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.Error(t, o.err, "the owner's turn must fail on the terminal provider error")
	case <-time.After(60 * time.Second):
		t.Fatal("owner call did not return after the gate opened")
	}

	// The pump (production 3s tick) re-leases the orphaned row, rebuilds
	// the call WITHOUT a policy, and runs it. Collect both restart
	// decisions.
	decisions := make(map[string]permission.PermissionNotification, 2)
	for i := 0; i < 2; i++ {
		select {
		case n := <-decided:
			decisions[n.ToolCallID] = n
		case <-time.After(60 * time.Second):
			t.Fatalf("only %d restart permission decisions arrived (expected 2) — the durable restart never ran", i)
		}
	}

	// THE regression matrix.
	require.True(t, decisions["call_q1"].Granted,
		"call_q1 (probe_cmd_two) must be GRANTED under the session baseline P2 — the queued caller's OWN declared policy (a blanket deny would break the restart; an unrestricted grant would be the pre-fix behavior)")
	require.True(t, decisions["call_q2"].Denied,
		"call_q2 (probe_cmd_neither) must be DENIED: the restarted call carries no policy of its own, so the session baseline must govern it — under the pre-fix code the unrestricted fallback blanket-granted this exact request")

	// The denial must have stopped the restarted turn: exactly 2
	// provider rounds for the restart, never a third (the bug canary).
	require.Equal(t, int64(2), restartRounds.Load(),
		"the restart must see exactly 2 provider rounds (a 3rd means the denied probe did not stop the turn)")
}
