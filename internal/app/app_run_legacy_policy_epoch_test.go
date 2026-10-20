package app

// Round-3 SDK review R3-4 regression pin: ExecuteRun no longer arms the
// per-session restricted-run allowlist at call time — the compiled policy
// travels on the call and is armed only when that call becomes the active
// turn. Two legacy queueing (FailIfSessionBusy=false) calls with OPPOSITE
// bash policies on one session each get judged by the policy of the turn
// that is actually running: the owner turn keeps its own policy whole,
// and once promoted the queued turn runs under ITS OWN policy — never a
// stale one or the fallback.
//
// CI-only diagnostic trace (package T). What it records, all NON-SECRET
// metadata with clipped strings and a capped event buffer:
//   - every provider request the mock handler sees: branch (the
//     turn/policy identity the routing markers imply), round counter,
//     emitted tool-call id, and whether the FOLLOW-UP request body
//     carries the denial tool-result text (a supplementary wire proxy
//     only — the real result flags are observed separately below);
//   - every DECIDED permission notification the moment it arrives, via a
//     dedicated recorder goroutine that is independent of the assertion
//     flow (so a failing precondition can no longer suppress decisions
//     that were already received);
//   - on failure: the persisted tool results of the session (per
//     tool_call_id: IsError and a clipped content prefix) read through
//     the app's Messages service, and the owner's batch_nodes rows
//     including the persisted response envelope decoded into its
//     is_error/stop_turn booleans only (fantasy.ToolResponse JSON tags;
//     the envelope content is never logged).
// REAL ADDED WAIT, stated honestly: the recorder goroutine runs for the
// lifetime of the test and is joined in cleanup; the emission precondition
// below can wait up to 60 seconds before the decision loop when a round
// never arrives (it is immediate in a healthy run). Assertions and
// decision-consumption semantics are unchanged: the matrix loop still
// consumes the same decided channel, and the four-decision matrix plus the
// final branch counters are byte-identical to the pre-trace oracle.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
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

	batchFlagOn := config.BatchRuntimeEnabled()
	trace := &legacyPolicyTrace{}

	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Supplementary wire proxy ONLY: whether the follow-up request
		// body carries the denial tool-result text, plus the body SIZE.
		// The real IsError/StopTurn observation lives in the cleanup dump
		// (persisted tool results + response envelopes), not here.
		deniedSeen := strings.Contains(string(body), "User denied permission")
		// Route on the QUEUED marker FIRST: once turn 2 runs, its request
		// body carries the whole session history, including turn 1's OWNER
		// marker; turn 1's requests never contain the QUEUED marker.
		if strings.Contains(string(body), "QUEUED_TURN_MARKER") {
			n := queuedBranch.Add(1)
			switch n {
			case 1:
				trace.recordCall("queued", "call_t2r1")
				trace.recordf("req branch=queued n=1 emit=call_t2r1 deniedSeen=%t bodyBytes=%d flagOn=%t", deniedSeen, len(body), batchFlagOn)
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cy1", "call_t2r1", "bash", `{"command":"probe_cmd_two r1","description":"t2r1"}`),
					admissionSSEStop("cy1", "tool_calls"),
				})
			case 2:
				trace.recordCall("queued", "call_t2r2")
				trace.recordf("req branch=queued n=2 emit=call_t2r2 deniedSeen=%t bodyBytes=%d flagOn=%t", deniedSeen, len(body), batchFlagOn)
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cy2", "call_t2r2", "bash", `{"command":"probe_cmd_one r2","description":"t2r2"}`),
					admissionSSEStop("cy2", "tool_calls"),
				})
			default:
				// Only reachable under the pre-fix bug (an extra grant extends a turn).
				trace.recordf("req branch=queued n=%d emit=CANARY deniedSeen=%t bodyBytes=%d", n, deniedSeen, len(body))
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
				trace.recordCall("owner", "call_t1r1")
				trace.recordf("req branch=owner n=1 emit=call_t1r1 deniedSeen=%t bodyBytes=%d flagOn=%t", deniedSeen, len(body), batchFlagOn)
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cx1", "call_t1r1", "bash", `{"command":"probe_cmd_one r1","description":"t1r1"}`),
					admissionSSEStop("cx1", "tool_calls"),
				})
			case 2:
				trace.recordCall("owner", "call_t1r2")
				trace.recordf("req branch=owner n=2 emit=call_t1r2 deniedSeen=%t bodyBytes=%d flagOn=%t", deniedSeen, len(body), batchFlagOn)
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("cx2", "call_t1r2", "bash", `{"command":"probe_cmd_two r2","description":"t1r2"}`),
					admissionSSEStop("cx2", "tool_calls"),
				})
			default:
				trace.recordf("req branch=owner n=%d emit=CANARY deniedSeen=%t bodyBytes=%d", n, deniedSeen, len(body))
				admissionWriteSSE(w, []string{admissionSSEText("cx", "T1_ROUND3_BUG_CANARY"), admissionSSEStop("cx", "stop")})
			}
			return
		}
		trace.recordf("req branch=UNMATCHED deniedSeen=%t bodyBytes=%d", deniedSeen, len(body))
		admissionWriteSSE(w, []string{admissionSSEText("c0", "IGNORED"), admissionSSEStop("c0", "stop")})
	}

	application, sessionID := newAdmissionRaceApp(t, handler)

	// decided is the matrix loop's input; the recorder goroutine below is
	// its ONLY producer, so the matrix consumes exactly the decisions the
	// trace records (same four in a healthy run — semantics preserved).
	decided := make(chan permission.PermissionNotification, 16)
	notifCtx, cancelNotif := context.WithCancel(context.Background())
	defer cancelNotif()

	// Independent decision recorder: appends every DECIDED notification to
	// the trace the moment it arrives, regardless of what the assertion
	// flow is doing. Started with the subscription, joined in the failure
	// cleanup; runs on its own goroutine and never calls require.
	traceDone := make(chan struct{})
	go func() {
		defer close(traceDone)
		for ev := range application.Permissions.SubscribeNotifications(notifCtx) {
			n := ev.Payload
			if !n.Granted && !n.Denied {
				continue
			}
			branch := "unknown"
			switch n.ToolCallID {
			case "call_t1r1", "call_t1r2":
				branch = "owner"
			case "call_t2r1", "call_t2r2":
				branch = "queued"
			}
			trace.recordDecision(clip120(n.ToolCallID), n.Granted, n.Denied, branch)
			select {
			case decided <- n:
			default:
				trace.recordf("decision channel full; matrix consumer may miss call=%s", clip120(n.ToolCallID))
			}
		}
	}()

	// CI-only diagnostic dump: on failure, print the buffered trace (which
	// by then holds every decision received so far), the branch counters,
	// the mailbox queue depth, the persisted tool results (real IsError
	// per call id) and the owner's batch_nodes rows with their persisted
	// response envelopes decoded to booleans. Metadata only; runs on the
	// test goroutine so no t.Logf races test completion.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		// notifCtx was cancelled by the defer above, so the recorder's
		// range ends promptly; the timeout only guards a stuck broker.
		select {
		case <-traceDone:
		case <-time.After(2 * time.Second):
			trace.recordf("recorder goroutine did not exit within 2s; dump may be incomplete")
		}
		trace.dump(t)
		t.Logf("TRACE flagOn=%t ownerRounds=%d queuedRounds=%d queuedPrompts=%d",
			batchFlagOn, ownerBranch.Load(), queuedBranch.Load(),
			application.AgentCoordinator.QueuedPrompts(sessionID))
		dumpToolResults(t, application, sessionID)
		dumpBatchNodes(t, application, sessionID)
	})

	outcomes := make(chan admissionOutcome, 2)

	// Call 1 (owner): restricted, allows only probe_cmd_one.
	start := make(chan struct{})
	trace.recordf("launch idx=1 role=owner restricted=true allowCount=1 legacyQueueing=true")
	admissionLaunch(t, application, sessionID, outcomes, start, 1, "owner turn OWNER_TURN_MARKER", RunOverrides{
		RestrictedRun: true,
		AllowBash:     []string{"probe_cmd_one"},
	}, false)
	close(start)
	select {
	case <-started:
		trace.recordf("owner call reached the provider")
	case <-time.After(60 * time.Second):
		t.Fatal("owner call never reached the provider; cannot stage the mid-turn owner")
	}

	// Call 2 (queued): restricted, allows only probe_cmd_two.
	start2 := make(chan struct{})
	trace.recordf("launch idx=2 role=queued restricted=true allowCount=1 legacyQueueing=true")
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
	trace.recordf("queued call visible in the mailbox submitted queue")

	// Receive the QUEUED call's outcome while the owner is still parked
	// mid-turn: the queueing caller's front-end ExecuteRun has FULLY
	// returned here — under the pre-fix code its deferred clear would
	// already have fired by now.
	select {
	case o := <-outcomes:
		require.Equal(t, 2, o.idx)
		require.Error(t, o.err)
		require.ErrorIs(t, o.err, ErrRunQueued)
		require.Equal(t, "queued", o.res.ExitReason)
		require.NotNil(t, o.res)
		trace.recordf("outcome idx=2 queued acknowledged")
	case <-time.After(60 * time.Second):
		t.Fatal("queued call did not return while the owner was still mid-turn")
	}

	// Release the owner; the queued turn runs after the owner's turn ends.
	close(gate1)
	trace.recordf("gate1 released; owner follow-up and queued promotion expected")

	// NEW fail-fast precondition, strictly ADDITIVE (non-weakening): the
	// MOCK HANDLER must have EMITTED all four scripted rounds — owner
	// branch exactly call_t1r1 then call_t1r2, queued branch exactly
	// call_t2r1 then call_t2r2 — before the decision matrix is evaluated.
	// This observes the HANDLER, not production dispatch: emission is
	// recorded before the SSE write, so it proves the provider saw the
	// request, not what production did with the call. The decision
	// evidence is immune to this check: the recorder goroutine has
	// already traced every decision received so far by the time this
	// fails. REAL ADDED WAIT: up to 60 seconds when a round never
	// arrives; zero added wait in a healthy run (the condition is
	// already true when the gate opens and both turns run).
	require.Eventually(t, func() bool {
		ownerSeen, queuedSeen := trace.emittedCalls()
		return len(ownerSeen) == 2 && len(queuedSeen) == 2
	}, 60*time.Second, 2*time.Millisecond,
		"the mock handler never emitted all four scripted provider rounds — emission observed at the handler, not production dispatch; see TRACE lines for which branch stalled and what it emitted")
	ownerSeen, queuedSeen := trace.emittedCalls()
	require.Equal(t, []string{"call_t1r1", "call_t1r2"}, ownerSeen,
		"mock handler must emit exactly call_t1r1 then call_t1r2 on the owner branch, in order")
	require.Equal(t, []string{"call_t2r1", "call_t2r2"}, queuedSeen,
		"mock handler must emit exactly call_t2r1 then call_t2r2 on the queued branch, in order")

	// Soft (logged-only) cross-check, no assertion: which emitted call ids
	// already have a persisted tool result at this point. Distinguishes
	// "result never persisted" from "result persisted but decision
	// notification missing".
	if msgs, listErr := application.Messages.List(context.Background(), sessionID); listErr == nil {
		persisted := map[string]bool{}
		for _, m := range msgs {
			for _, r := range m.ToolResults() {
				persisted[r.ToolCallID] = true
			}
		}
		for _, id := range append(append([]string(nil), ownerSeen...), queuedSeen...) {
			trace.recordf("persisted tool result for %s present=%t", id, persisted[id])
		}
	} else {
		trace.recordf("persisted tool result cross-check failed: %s", clip120(listErr.Error()))
	}

	// Collect exactly the four decisions, attributed by unique tool-call id.
	// (A fifth decision would require a fifth provider round, which the
	// branch request-count assertions below make impossible — the negative
	// check is therefore implied by the counters.) Semantics unchanged
	// from the pre-trace oracle: this loop consumes the same decided
	// channel, produced only by the recorder goroutine above.
	decisions := make(map[string]permission.PermissionNotification, 4)
	for i := 0; i < 4; i++ {
		select {
		case n := <-decided:
			decisions[n.ToolCallID] = n
		case <-time.After(60 * time.Second):
			t.Fatalf("only %d permission decisions arrived (expected 4) — see TRACE lines for the decisions received so far and the emission evidence", i)
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
		trace.recordf("outcome idx=1 owner completed")
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

// traceMaxEvents caps the in-memory trace; further events increment the
// overflow counter reported by dump.
const traceMaxEvents = 200

// legacyPolicyTrace collects bounded, non-secret diagnostic metadata for
// the CI-only trace above. Recording is goroutine-safe (the provider
// handler and the recorder goroutine both record); dumping happens only
// on the test goroutine inside t.Cleanup, so no t.Logf call can race test
// completion. Strings are clipped at 120 characters (tool-result content
// at 60) and the event buffer at traceMaxEvents.
type legacyPolicyTrace struct {
	mu          sync.Mutex
	events      []string
	overflow    int
	ownerCalls  []string
	queuedCalls []string
}

func clip120(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func clip60(s string) string {
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

// appendEvent stores one already-clipped event, counting overflow beyond
// the cap instead of growing without bound.
func (tr *legacyPolicyTrace) appendEvent(line string) {
	if len(tr.events) >= traceMaxEvents {
		tr.overflow++
		return
	}
	tr.events = append(tr.events, line)
}

// recordf appends one clipped metadata line to the trace buffer.
func (tr *legacyPolicyTrace) recordf(format string, args ...any) {
	line := clip120(fmt.Sprintf(format, args...))
	tr.mu.Lock()
	tr.appendEvent(line)
	tr.mu.Unlock()
}

// recordDecision appends one decided-permission event. Called from the
// recorder goroutine; the decision evidence therefore never depends on
// the assertion flow reaching the matrix loop.
func (tr *legacyPolicyTrace) recordDecision(callID string, granted, denied bool, branch string) {
	line := clip120(fmt.Sprintf("decision call=%s granted=%t denied=%t branch=%s", callID, granted, denied, branch))
	tr.mu.Lock()
	tr.appendEvent(line)
	tr.mu.Unlock()
}

// recordCall notes a tool-call id emitted on a provider round, keyed by
// the branch (turn/policy identity implied by the routing markers) it
// was dispatched under. Records EMISSION at the handler — not production
// dispatch.
func (tr *legacyPolicyTrace) recordCall(branch, callID string) {
	id := clip120(callID)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	switch branch {
	case "owner":
		tr.ownerCalls = append(tr.ownerCalls, id)
	case "queued":
		tr.queuedCalls = append(tr.queuedCalls, id)
	default:
		tr.appendEvent("recordCall unknown branch=" + clip120(branch) + " id=" + id)
	}
}

// emittedCalls returns copies of the per-branch emitted call-id lists.
func (tr *legacyPolicyTrace) emittedCalls() (owner, queued []string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.ownerCalls...), append([]string(nil), tr.queuedCalls...)
}

// dump prints the buffered trace lines on the test goroutine (cleanup only).
func (tr *legacyPolicyTrace) dump(t *testing.T) {
	t.Helper()
	tr.mu.Lock()
	events := append([]string(nil), tr.events...)
	overflow := tr.overflow
	tr.mu.Unlock()
	for i, line := range events {
		t.Logf("TRACE[%02d] %s", i+1, line)
	}
	if overflow > 0 {
		t.Logf("TRACE event buffer overflowed; %d further events withheld", overflow)
	}
}

// dumpToolResults logs the REAL persisted tool results of the session —
// per tool_call_id: the returned IsError flag and a clipped content
// prefix — read through the app's Messages service. This is the
// authoritative result observation; the handler-side deniedSeen proxy is
// supplementary only.
func dumpToolResults(t *testing.T, application *App, sessionID string) {
	t.Helper()
	msgs, err := application.Messages.List(context.Background(), sessionID)
	if err != nil {
		t.Logf("TRACE tool-results read failed: %s", clip120(err.Error()))
		return
	}
	found := 0
	for order, m := range msgs {
		for _, r := range m.ToolResults() {
			found++
			if found > 16 {
				t.Logf("TRACE tool-results: more than 16 rows, remainder withheld")
				return
			}
			t.Logf("TRACE toolresult order=%d call=%s isError=%t content=%q",
				order, clip120(r.ToolCallID), r.IsError, clip60(r.Content))
		}
	}
	if found == 0 {
		t.Logf("TRACE tool-results: no persisted tool results for the session")
	}
}

// dumpBatchNodes logs bounded terminal-state metadata for the owner's
// batch_nodes rows, including the persisted response envelope (column
// added by migration 20261012000002) decoded into its is_error and
// stop_turn booleans ONLY — the fantasy.ToolResponse JSON tags; the
// envelope content is never logged. With RUSH_BATCH_RUNTIME off there
// are no rows — that empty baseline is itself recorded, so the flag
// modes are comparable.
func dumpBatchNodes(t *testing.T, application *App, owner string) {
	t.Helper()
	const q = `SELECT node_id, root_id, kind, state, delivery, control_state, tool_call_id, response_envelope FROM batch_nodes WHERE owner_session_id = ? ORDER BY created_at, node_id`
	rows, err := application.DB().QueryContext(context.Background(), q, owner)
	if err != nil {
		t.Logf("TRACE batch_nodes read failed: %s", clip120(err.Error()))
		return
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var nodeID, rootID, kind, state, delivery, control, callID, envelope sql.NullString
		if scanErr := rows.Scan(&nodeID, &rootID, &kind, &state, &delivery, &control, &callID, &envelope); scanErr != nil {
			t.Logf("TRACE batch_nodes scan failed: %s", clip120(scanErr.Error()))
			return
		}
		count++
		if count > 16 {
			t.Logf("TRACE batch_nodes: more than 16 rows, remainder withheld")
			return
		}
		envInfo := "absent"
		if envelope.Valid && envelope.String != "" {
			// The column holds the OUTER jobbatch envelope
			// {state, code, summary, details}; the executor's ToolResponse
			// (is_error / stop_turn) is nested under "details"
			// (batch_commit.go batchCommitEnvelope, restored the same way by
			// batchStoreDoneSource.LoadDone).
			var outer struct {
				State   string          `json:"state"`
				Details json.RawMessage `json:"details"`
			}
			var decoded struct {
				IsError  bool `json:"is_error"`
				StopTurn bool `json:"stop_turn"`
			}
			switch {
			case json.Unmarshal([]byte(envelope.String), &outer) != nil:
				envInfo = "undecodable"
			case len(outer.Details) == 0:
				envInfo = fmt.Sprintf("state=%s details=absent", clip120(outer.State))
			case json.Unmarshal(outer.Details, &decoded) != nil:
				envInfo = fmt.Sprintf("state=%s details=undecodable", clip120(outer.State))
			default:
				envInfo = fmt.Sprintf("state=%s is_error=%t stop_turn=%t", clip120(outer.State), decoded.IsError, decoded.StopTurn)
			}
		}
		t.Logf("TRACE batch node=%s root=%s kind=%s state=%s delivery=%s control=%s call=%s envelope=%s",
			clip120(nodeID.String), clip120(rootID.String), clip120(kind.String),
			clip120(state.String), clip120(delivery.String), clip120(control.String),
			clip120(callID.String), envInfo)
	}
	if err := rows.Err(); err != nil {
		t.Logf("TRACE batch_nodes iterate failed: %s", clip120(err.Error()))
		return
	}
	if count == 0 {
		t.Logf("TRACE batch_nodes: no rows for the owner (flag-OFF baseline or no batch dispatch)")
	}
}
