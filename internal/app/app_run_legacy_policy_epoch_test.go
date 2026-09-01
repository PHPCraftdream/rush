package app

// Round-3 SDK review R3-4 regression pin: ExecuteRun no longer arms the
// per-session restricted-run allowlist at call time — the compiled policy
// travels on the call and is armed only when that call becomes the active
// turn. Two legacy queueing (FailIfSessionBusy=false) calls with OPPOSITE
// bash policies on one session each get judged by the policy of the turn
// that is actually running: the owner turn keeps its own policy whole,
// and once promoted the queued turn runs under ITS OWN policy — never a
// stale one or the fallback.

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

// TestExecuteRunLegacyQueueingTurnsRunUnderTheirOwnPolicies: two queued
// legacy ExecuteRun calls on ONE session with opposite restricted
// policies. Each turn's gated bash probe must be judged by the policy of
// the turn actually running — the discriminator rounds are call_t1r2
// (denied: the owner's own policy persists; the queued policy would allow
// it) and call_t2r2 (denied: the queued turn's own policy; a stale policy1
// or the fallback would grant it).
func TestExecuteRunLegacyQueueingTurnsRunUnderTheirOwnPolicies(t *testing.T) {
	var (
		gate1        = make(chan struct{})
		started      = make(chan struct{})
		startOnce    sync.Once
		ownerBranch  atomic.Int64
		queuedBranch atomic.Int64
	)

	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Route on the QUEUED marker FIRST: once turn 2 runs, its request
		// body carries the whole session history, including turn 1's OWNER
		// marker; turn 1's requests never contain the QUEUED marker.
		if strings.Contains(string(body), "QUEUED_TURN_MARKER") {
			n := queuedBranch.Add(1)
			switch n {
			case 1:
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cy1", "call_t2r1", "bash", `{"command":"probe_cmd_two r1","description":"t2r1"}`),
					admissionSSEStop("cy1", "tool_calls"),
				})
			case 2:
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cy2", "call_t2r2", "bash", `{"command":"probe_cmd_one r2","description":"t2r2"}`),
					admissionSSEStop("cy2", "tool_calls"),
				})
			default:
				// Only reachable under the pre-fix bug (an extra grant extends a turn).
				admissionWriteSSE(w, []string{admissionSSEText("cy", "T2_ROUND3_BUG_CANARY"), admissionSSEStop("cy", "stop")})
			}
			return
		}
		if strings.Contains(string(body), "OWNER_TURN_MARKER") {
			n := ownerBranch.Add(1)
			switch n {
			case 1:
				startOnce.Do(func() { close(started) })
				<-gate1
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cx1", "call_t1r1", "bash", `{"command":"probe_cmd_one r1","description":"t1r1"}`),
					admissionSSEStop("cx1", "tool_calls"),
				})
			case 2:
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cx2", "call_t1r2", "bash", `{"command":"probe_cmd_two r2","description":"t1r2"}`),
					admissionSSEStop("cx2", "tool_calls"),
				})
			default:
				admissionWriteSSE(w, []string{admissionSSEText("cx", "T1_ROUND3_BUG_CANARY"), admissionSSEStop("cx", "stop")})
			}
			return
		}
		admissionWriteSSE(w, []string{admissionSSEText("c0", "IGNORED"), admissionSSEStop("c0", "stop")})
	}

	application, sessionID := newAdmissionRaceApp(t, handler)

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

	// Call 2 (queued): restricted, allows only probe_cmd_two.
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2, "queued turn QUEUED_TURN_MARKER", RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"probe_cmd_two"},
	}, false)
	close(start2)

	// Wait for the queued call to land in the mailbox's submitted queue
	// BEFORE releasing the owner. The owner is provably still blocked on
	// gate1, so it cannot have reached its own end-of-turn drain yet —
	// once QueuedPrompts reports the call, the owner's upcoming drain is
	// guaranteed to observe it and hand it the next turn directly (same
	// anti-orphaning rationale as the legacy queueing sibling test).
	require.Eventually(t, func() bool {
		return application.AgentCoordinator.QueuedPrompts(sessionID) >= 1
	}, 10*time.Second, time.Millisecond, "the queued call never reached the mailbox's submitted queue")

	// Receive the QUEUED call's outcome while the owner is still parked
	// mid-turn: the queueing caller's front-end ExecuteRun has FULLY
	// returned here — under the pre-fix code its deferred clear would
	// already have fired by now.
	select {
	case o := <-outcomes:
		require.Equal(t, 2, o.idx)
		require.NoError(t, o.err, "legacy queueing call must not fail")
		require.NotNil(t, o.res)
	case <-time.After(60 * time.Second):
		t.Fatal("queued call did not return while the owner was still mid-turn")
	}

	// Release the owner; the queued turn runs after the owner's turn ends.
	close(gate1)

	// Collect exactly the four decisions, attributed by unique tool-call id.
	// (A fifth decision would require a fifth provider round, which the
	// branch request-count assertions below make impossible — the negative
	// check is therefore implied by the counters.)
	decisions := make(map[string]permission.PermissionNotification, 4)
	for i := 0; i < 4; i++ {
		select {
		case n := <-decided:
			decisions[n.ToolCallID] = n
		case <-time.After(60 * time.Second):
			t.Fatalf("only %d permission decisions arrived (expected 4)", i)
		}
	}

	// THE regression matrix.
	require.True(t, decisions["call_t1r1"].Granted,
		"call_t1r1 (probe_cmd_one r1) must be GRANTED: the owner turn is judged by its own policy1")
	require.True(t, decisions["call_t1r2"].Denied,
		"call_t1r2 (probe_cmd_two r2) must be DENIED: the owner turn keeps ITS OWN policy the whole time — the queued call's opposite policy (which would allow this command) never leaked in, and the entry was not cleared out from under it (fallback would also allow)")
	require.True(t, decisions["call_t2r1"].Granted,
		"call_t2r1 (probe_cmd_two r1) must be GRANTED: once promoted, the queued turn's own policy is armed")
	require.True(t, decisions["call_t2r2"].Denied,
		"call_t2r2 (probe_cmd_one r2) must be DENIED: the queued turn is judged by ITS OWN policy — a stale policy1 or the fallback gate would GRANT this command")

	// Wait for the owner's outcome. FinalText is deliberately not asserted:
	// queueing callers' envelopes reflect whatever the shared session
	// stream had seen.
	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.NoError(t, o.err, "owner call must not fail")
		require.NotNil(t, o.res)
	case <-time.After(60 * time.Second):
		t.Fatal("owner call did not complete after the gate opened")
	}

	// Each turn ends at its denial, so each branch saw exactly 2 requests.
	// A third owner-branch request would mean call_t1r2 was granted (the
	// bug); likewise for the queued branch — and the branch encoding in the
	// handler means the queued turn ran strictly after turn 1 ended.
	require.Equal(t, int64(2), ownerBranch.Load(), "owner branch must see exactly 2 requests (a 3rd means call_t1r2 was granted — the R3-4 bug)")
	require.Equal(t, int64(2), queuedBranch.Load(), "queued branch must see exactly 2 requests (a 3rd means call_t2r2 was granted — the R3-4 bug)")
}
