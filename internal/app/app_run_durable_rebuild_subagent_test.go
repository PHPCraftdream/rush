package app

// R4-2 regression (P0): a policy-carrying DURABLE REBUILD whose turn
// delegates to a sub-agent. The parent queued call persists its policy
// spec on the durable row; the pump rebuilds the call, recompiles the
// spec, and runOwned arms the restarted turn's per-call entry keyed by its
// LogicalCallID; RunSessionAgentCall re-arms the session's auto-approve.
// The CHILD session (runSubAgent → InheritSessionAutoApprove +
// InheritSessionRunAllowlist) must then be BOTH:
//   - auto-approved — a permission decision ARRIVES at all. Under
//     pre-R4-3 the rebuilt process's in-memory auto-approve was gone, so
//     the child would hang on the interactive path and produce NO
//     decision (its failure mode is silence, not a wrong verdict). Under
//     pre-R4-2, a child that DID inherit auto-approve with no
//     restriction fell through to the unrestricted process-wide gate and
//     would be GRANTED.
//   - RESTRICTED — the decision is a DENIAL of a command outside the
//     parent's policy. Both halves must be asserted together: arrival
//     alone proves only inheritance of auto-approve, and denial alone
//     would also be produced by a hung-then-timed-out child; only
//     "one decision, and it is a denial, within a bounded time" proves
//     the child inherited BOTH auto-approve and the parent's
//     restriction.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestExecuteRunDurablyRebuiltCallSubAgentInheritsParentPolicy(t *testing.T) {
	var (
		gate1     = make(chan struct{})
		started   = make(chan struct{})
		startOnce sync.Once
	)

	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "CHILD_TURN_MARKER"):
			// The child's turn. Round 1 asks for a command OUTSIDE the
			// parent policy; the inherited restriction must deny it.
			if !strings.Contains(string(body), "ctool") {
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cc1", "ctool", "bash", `{"command":"probe_cmd_child_forbidden r1","description":"child round 1"}`),
					admissionSSEStop("cc1", "tool_calls"),
				})
				return
			}
			// Only reachable for requests whose history already carries the
			// denied tool-call id — a genuine post-denial continuation, or
			// the child session's title-generation request (its history
			// includes the full turn). Neither may produce a SECOND
			// permission decision: title-gen never executes tools, and a
			// real continuation's next gated bash call would surface here
			// as a second decision and fail the count below.
			admissionWriteSSE(w, []string{admissionSSEText("c3", "CHILD_DONE"), admissionSSEStop("c3", "stop")})
		case strings.Contains(string(body), "ptool"):
			// The parent's rebuilt turn, round 2 (after the delegation
			// tool result landed). End the parent turn.
			admissionWriteSSE(w, []string{admissionSSEText("c2", "PARENT_DONE"), admissionSSEStop("c2", "stop")})
		case strings.Contains(string(body), "DELEGATING_MARKER"):
			// The parent's rebuilt turn, round 1: delegate to a
			// sub-agent. The agent tool does not go through
			// permission.Request, so no allowlist entry is needed for it.
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("cp1", "ptool", "agent", `{"prompt":"child turn CHILD_TURN_MARKER"}`),
				admissionSSEStop("cp1", "tool_calls"),
			})
		case strings.Contains(string(body), "OWNER_TURN_MARKER"):
			// The owner: block mid-turn until the delegating call has
			// landed, then fail TERMINALLY so the finalizer orphans the
			// queued call into the durable run queue.
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

	// Collect DECIDED permission notifications.
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

	// Call 1 (owner): unrestricted — its job is only to park and then die.
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start, 1, "owner turn OWNER_TURN_MARKER", RunOverrides{}, false)
	close(start)
	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("owner call never reached the provider; cannot stage the mid-turn owner")
	}

	// Queued call: RESTRICTED, allows only probe_cmd_parent; its turn
	// delegates to a sub-agent.
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2, "delegating turn DELEGATING_MARKER", RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"probe_cmd_parent"},
	}, false)
	close(start2)
	require.Eventually(t, func() bool {
		return application.AgentCoordinator.QueuedPrompts(sessionID) >= 1
	}, 10*time.Second, time.Millisecond, "the delegating call never reached the mailbox's submitted queue")

	// The queueing caller returns as soon as its call is queued.
	select {
	case o := <-outcomes:
		require.Equal(t, 2, o.idx)
		require.NoError(t, o.err, "queued call must not fail")
		require.NotNil(t, o.res)
	case <-time.After(60 * time.Second):
		t.Fatal("queued call did not return while the owner was still mid-turn")
	}

	// Fail the owner terminally; the finalizer orphans the delegating
	// call, whose policy spec is persisted on the durable row.
	close(gate1)
	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.Error(t, o.err, "the owner's turn must fail on the terminal provider error")
	case <-time.After(60 * time.Second):
		t.Fatal("owner call did not return after the gate opened")
	}

	// The pump rebuilds the delegating call with its persisted spec, and
	// its sub-agent's bash request produces EXACTLY ONE decision — a
	// denial, within a bounded time. THE both-halves assertion.
	var decisions []permission.PermissionNotification
	deadline := time.After(60 * time.Second)
	for len(decisions) < 1 {
		select {
		case n := <-decided:
			decisions = append(decisions, n)
		case <-deadline:
			t.Fatal("no permission decision arrived within 60s — the child did not inherit auto-approve (pre-R4-3: the restarted process lost the session's auto-approve, so the child hangs on the interactive path and stays silent)")
		}
	}
	// The extra-decision drain below IS the stop-turn canary: a second
	// gated bash call from a post-denial continuation would arrive as a
	// second decision and fail the count assertion. Drain briefly to
	// catch any EXTRA decision.
	extraDeadline := time.After(5 * time.Second)
	for {
		select {
		case n := <-decided:
			decisions = append(decisions, n)
		case <-extraDeadline:
			goto decided_collected
		}
	}
decided_collected:
	require.Len(t, decisions, 1,
		"exactly one permission decision must arrive for the child's bash call; more means a denial failed to stop a turn or the unrestricted fallback path answered")
	require.Equal(t, "ctool", decisions[0].ToolCallID,
		"the single decision must be the child's bash call")
	require.True(t, decisions[0].Denied,
		"the child's forbidden command must be DENIED: arrival of the decision proves the child inherited auto-approve (a silent turn would be the pre-R4-3 hang), and the DENIAL proves it inherited the parent's RESTRICTION — a grant here is pre-R4-2's fail-open child judged by the unrestricted process-wide gate")
	require.False(t, decisions[0].Granted,
		"the child's forbidden command must not be granted (pre-R4-2's unrestricted child)")

	// The parent's rebuilt turn (on the pump goroutine) produced no
	// ExecuteRun outcome; give it a moment to wind down, then make sure
	// the child's denial is all that happened. Nothing further to assert
	// about outcomes — the owner's err is the only ExecuteRun result this
	// test can observe besides the queued caller's.
}
