package app

// R4-1 regression (P0): TWO queued calls with OPPOSITE restricted policies
// on the SAME session, both durably orphaned by the owner's terminal
// failure. Each rebuilt turn must run under ITS OWN persisted policy spec
// — independent of the order in which the orphaned rows were enqueued or
// rebuilt. Under the pre-fix code the pump-rebuilt calls carried no policy
// of their own and fell back to the per-session baseline, a
// last-writer-wins value armed by whichever ExecuteRun ran last (here
// always Q3's P3): Q2's restart was then judged by P3 and its own
// probe_cmd_two was DENIED — the cross-call identity loss this test pins.
//
// Enqueue order is deliberately NOT assumed: restartOrphanedWithRetry
// enqueues each orphaned call from its own goroutine, so the durable row
// order is racy; the pump only serializes per-session execution. The
// handler therefore routes by the LAST occurrence of each turn's marker
// in the request body — the messages array ends with the CURRENT turn's
// user message, so the last marker names the turn being executed (mere
// presence cannot: the sibling's prompt and tool-call ids sit in the
// history once its rebuild has run). Each rebuilt turn gets an own-policy
// round followed by a cross-policy round; under the pre-fix code Q2's own
// probe is denied by the last-writer-wins P3 baseline — that mismatch is
// the regression signal.

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

// decisionBoard records decided permission notifications so the test
// goroutine can wait for each restart round's verdict by tool-call id.
type decisionBoard struct {
	mu  sync.Mutex
	got map[string]permission.PermissionNotification
}

func newDecisionBoard() *decisionBoard {
	return &decisionBoard{got: make(map[string]permission.PermissionNotification)}
}

func (b *decisionBoard) add(n permission.PermissionNotification) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got[n.ToolCallID] = n
}

// waitDecision polls for the decision on id until the deadline.
func (b *decisionBoard) waitDecision(id string, timeout time.Duration) (permission.PermissionNotification, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		n, ok := b.got[id]
		b.mu.Unlock()
		if ok {
			return n, true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return permission.PermissionNotification{}, false
}

func TestExecuteRunDurablyOrphanedQueuedCallsEachRunUnderTheirOwnPolicy(t *testing.T) {
	var (
		gate1     = make(chan struct{})
		started   = make(chan struct{})
		startOnce sync.Once

		// restartRounds counts restart provider rounds: exactly 4 across
		// both rebuilt turns (2 each). A 5th means a denial failed to
		// stop a turn.
		restartRounds atomic.Int64
	)

	board := newDecisionBoard()

	// toolRound emits one bash tool-call round for id/command and counts
	// it as a restart round.
	toolRound := func(w http.ResponseWriter, id, command, description string) {
		restartRounds.Add(1)
		admissionWriteSSE(w, []string{
			admissionSSEToolCall(id, id, "bash",
				`{"command":"`+command+`","description":"`+description+`"}`),
			admissionSSEStop(id, "tool_calls"),
		})
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Routing by LAST occurrence, not mere presence: the request's
		// messages array ends with the CURRENT turn's user message, so the
		// marker that appears last in the body identifies the turn being
		// executed. Mere presence cannot: once the other queued turn has
		// already run (row order is racy), its prompt and tool-call ids sit
		// in this turn's history too, and a contains() check misroutes the
		// turn — the observed failure mode had Q2's rebuild re-served by
		// the Q3 branch, repeating an identical granted bash call until
		// loop detection stopped it.
		posTwo := strings.LastIndex(string(body), "TWO_TURN_MARKER")
		posThree := strings.LastIndex(string(body), "THREE_TURN_MARKER")
		switch {
		case posTwo > posThree:
			// Q2's rebuilt turn is executing (its own marker is the
			// current, final user message).
			if !strings.Contains(string(body), "t2a") {
				toolRound(w, "t2a", "probe_cmd_two r1", "Q2 own round")
				return
			}
			toolRound(w, "t2b", "probe_cmd_three r2", "Q2 cross round")
		case posThree > posTwo:
			// Q3's rebuilt turn is executing.
			if !strings.Contains(string(body), "t3a") {
				toolRound(w, "t3a", "probe_cmd_three r1", "Q3 own round")
				return
			}
			toolRound(w, "t3b", "probe_cmd_two r2", "Q3 cross round")
		case strings.Contains(string(body), "OWNER_TURN_MARKER"):
			// Neither queued marker is present anywhere: the owner's own
			// round (it runs before either queued user message exists).
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

	// Feed every DECIDED permission notification into the board.
	notifCtx, cancelNotif := context.WithCancel(context.Background())
	defer cancelNotif()
	go func() {
		for ev := range application.Permissions.SubscribeNotifications(notifCtx) {
			if ev.Payload.Granted || ev.Payload.Denied {
				board.add(ev.Payload)
			}
		}
	}()

	outcomes := make(chan admissionOutcome, 3)

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

	// Queued call Q2: restricted, allows only probe_cmd_two.
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2, "second queued turn TWO_TURN_MARKER", RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"probe_cmd_two"},
	}, false)
	close(start2)
	require.Eventually(t, func() bool {
		return application.AgentCoordinator.QueuedPrompts(sessionID) >= 1
	}, 10*time.Second, time.Millisecond, "Q2 never reached the mailbox's submitted queue")

	// Queued call Q3: restricted, allows only probe_cmd_three — the
	// OPPOSITE policy, queued behind Q2 on the SAME session.
	start3 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start3, 3, "third queued turn THREE_TURN_MARKER", RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"probe_cmd_three"},
	}, false)
	close(start3)
	require.Eventually(t, func() bool {
		return application.AgentCoordinator.QueuedPrompts(sessionID) >= 2
	}, 10*time.Second, time.Millisecond, "Q3 never reached the mailbox's submitted queue")

	// Both queueing callers must return cleanly (collected tolerantly of
	// arrival order).
	seen := make(map[int]bool)
	for len(seen) < 2 {
		select {
		case o := <-outcomes:
			seen[o.idx] = true
			require.NoError(t, o.err, "queued call %d must not fail", o.idx)
			require.NotNil(t, o.res)
		case <-time.After(60 * time.Second):
			t.Fatalf("queued calls did not return (seen=%v)", seen)
		}
	}
	require.True(t, seen[2], "Q2's outcome missing")
	require.True(t, seen[3], "Q3's outcome missing")

	// Fail the owner terminally; the finalizer must orphan BOTH queued
	// calls into the durable run queue (row order racy by design).
	close(gate1)
	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.Error(t, o.err, "the owner's turn must fail on the terminal provider error")
	case <-time.After(60 * time.Second):
		t.Fatal("owner call did not return after the gate opened")
	}

	// THE R4-1 matrix: all four restart rounds must be decided, each
	// under the rebuilt turn's OWN persisted policy.
	t2a, ok := board.waitDecision("t2a", 60*time.Second)
	require.True(t, ok, "t2a never produced a decision — the pump never rebuilt the orphaned rows")
	require.True(t, t2a.Granted,
		"t2a (Q2's own probe_cmd_two) must be GRANTED under Q2's OWN persisted policy P2 — a denial here means Q2's restart was judged by P3, the LAST queued policy: the R4-1 cross-call identity loss")

	t2b, ok := board.waitDecision("t2b", 60*time.Second)
	require.True(t, ok, "t2b never produced a decision — the pump never rebuilt the orphaned rows")
	require.True(t, t2b.Denied,
		"t2b (probe_cmd_three) must be DENIED under Q2's own P2 — a grant here means Q2 ran under P3")

	t3a, ok := board.waitDecision("t3a", 60*time.Second)
	require.True(t, ok, "t3a never produced a decision — the pump never rebuilt the orphaned rows")
	require.True(t, t3a.Granted,
		"t3a (Q3's own probe_cmd_three) must be GRANTED under Q3's OWN persisted policy P3 — a denial here means Q3's restart was judged by P2")

	t3b, ok := board.waitDecision("t3b", 60*time.Second)
	require.True(t, ok, "t3b never produced a decision — the pump never rebuilt the orphaned rows")
	require.True(t, t3b.Denied,
		"t3b (probe_cmd_two) must be DENIED under Q3's own P3")

	require.Equal(t, int64(4), restartRounds.Load(),
		"both rebuilt turns must see exactly 4 restart rounds total (a 5th means a denial failed to stop a turn)")
}
