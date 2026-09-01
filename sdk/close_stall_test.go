package sdk

// R3-2 (P1) — Close-vs-stuck-run tests. Round-2's Close waited for its
// admission drain to complete BEFORE running the shutdown, and the
// shutdown's CancelAll is the ONLY code path that cancels in-flight
// agent work — so an admitted Run stuck on a non-cancellable provider or
// tool call kept inflight above zero forever, drained never closed,
// CancelAll was never reached, and Close blocked forever. The reordered
// Close gives admitted calls one grace period to finish against the
// fully live App, then cancels agent work while every resource is still
// open, and releases resources only once the drain completed or the
// shutdown went forced.
//
// Both tests below are channel-gated end to end — no sleeps. The only
// real time in them is the documented grace policy itself
// (agent.DefaultCancelAllGrace), which is the thing under test.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// stallGraceSlack is the scheduling slack on top of the documented grace
// bound in the tests' never-hang guards. Under -race on a loaded box the
// cooperative paths take a few seconds; the slack exists only so a
// REGRESSION (Close hanging forever) fails with a clear message instead
// of the package-level timeout.
const stallGraceSlack = 20 * time.Second

// writeMarkerSSE serves one openai-compat SSE chunk stream finishing a
// turn with the given marker text — the chunk body of
// close_admission_test.go's newMarkerProvider server, usable for
// follow-up requests inside a custom handler.
func writeMarkerSSE(w http.ResponseWriter, marker string) {
	w.Header().Set("Content-Type", "text/event-stream")
	writeChunk := func(chunk map[string]any) {
		b, err := json.Marshal(chunk)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	writeChunk(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "library-model",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": marker},
			"finish_reason": nil,
		}},
	})
	writeChunk(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "library-model",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 3, "total_tokens": 6},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestCloseCancelsStuckAdmittedRun is the core R3-2 regression test: a
// Run admitted before Close blocks INSIDE its provider call, on the
// request context's Done channel — the only way it can ever unblock is
// the coordinator's own cancellation reaching the in-flight generation.
// The test hands Run a context.Background() and never cancels anything
// test-side; Close must unblock the run by cancelling agent work while
// the App is still fully live, and the run's unwinding keeps the
// shutdown graceful.
func TestCloseCancelsStuckAdmittedRun(t *testing.T) {
	seq := &seqRecorder{}
	entered := make(chan struct{})
	canceled := make(chan struct{})
	var requests atomic.Int32

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			// The turn's provider call: block until the REQUEST
			// context is done — which happens only when the
			// coordinator's cancellation reaches the in-flight
			// generation. Nothing test-side can unblock this.
			seq.record("provider:stuck")
			close(entered)
			<-r.Context().Done()
			seq.record("provider:canceled")
			close(canceled)
			return
		}
		// Any later request (e.g. background title generation for an
		// unrelated turn) completes immediately so it can never pin
		// the coordinator's join past the assertions.
		writeMarkerSSE(w, "CLOSE_STALL_LATER_OK")
	}))
	t.Cleanup(provider.Close)

	client := newAdmissionTestClient(t, provider.URL)
	client.app.AgentCoordinator = &recordingCoordinator{
		Coordinator: client.app.AgentCoordinator,
		seq:         seq,
	}

	type runOutcome struct {
		res *RunResult
		err error
	}
	runDone := make(chan runOutcome, 1)
	go func() {
		// context.Background(), and the test never cancels it: any
		// cancellation the run observes must have come from Close.
		res, err := client.Run(context.Background(), RunRequest{
			Prompt:      "reply with exactly the marker text and nothing else",
			Mode:        RunModeJSON,
			HideSpinner: true,
		})
		runDone <- runOutcome{res: res, err: err}
	}()
	<-entered // the run is stuck inside agent work at the provider

	closeStart := time.Now()
	closeDone := make(chan CloseResult, 1)
	go func() { closeDone <- client.Close() }()

	select {
	case <-canceled:
		// Close's cancellation reached the provider call.
	case <-time.After(2*agent.DefaultCancelAllGrace + stallGraceSlack):
		t.Fatal("Close never unblocked the stuck provider call via agent cancellation — the R3-2 hang is back")
	}

	outcome := <-runDone
	seq.record("run:returned")
	// Deliberately NO assertion on the envelope's error/exit reason: the
	// run's presentation of its own cancellation is ExecuteRun's concern
	// and is not deterministic — the message-event subscription is bound
	// to the run's ctx, which the cancellation kills, so the
	// FinishReasonCanceled event can be dropped with it and the envelope
	// may be either a canceled error or a bare success. What Close owes
	// this test is that the stuck run RETURNED (asserted by the receive
	// above), unblocked by the coordinator's cancellation (asserted by
	// the provider:canceled ordering below), with a graceful shutdown.
	if outcome.err == nil {
		require.NotNil(t, outcome.res, "a finished run must always carry a result envelope")
	}

	res := <-closeDone
	require.False(t, res.Forced,
		"the run unwound once cancelled, so the shutdown must stay graceful")
	require.Empty(t, res.CleanupErrors)
	require.Less(t, time.Since(closeStart), 2*agent.DefaultCancelAllGrace+stallGraceSlack,
		"Close must return within the documented grace bound once the run unwinds")

	// Ordering: Close cancelled agent work WHILE the run was genuinely
	// stuck in the provider, the provider unblocked BECAUSE of that
	// cancellation, and the run returned only afterwards.
	require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("provider:stuck"),
		"Close must fire cancellation while the run is still stuck in agent work")
	require.Greater(t, seq.indexOf("provider:canceled"), seq.indexOf("shutdown:CancelAll"),
		"the provider call must have been unblocked by the shutdown's cancellation")
	require.Greater(t, seq.indexOf("run:returned"), seq.indexOf("shutdown:CancelAll"))
}

// stonewallCoordinator stands in for agent work that IGNORES
// cancellation: Run parks on a test channel — a controlled stand-in for
// a tool or provider call that never checks its ctx — and CancelAll
// reports the verdict a real CancelAll would after ITS grace period:
// still busy, because the parked run never unwinds. The real
// coordinator's join behavior is covered by internal/agent's own tests
// (TestP343_CancelAllTrueJoinWaitsForRealBlockedRun and friends); this
// test pins the SDK contract on top: Close returns within the documented
// bound with Forced=true, the in-memory handles stay open, and
// CloseEphemeralConnsForced reclaims them afterwards.
type stonewallCoordinator struct {
	agent.Coordinator
	seq       *seqRecorder
	entered   chan struct{}
	unblock   chan struct{}
	exited    chan struct{}
	joinGrace time.Duration
}

func (c *stonewallCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	c.seq.record("run:stuck")
	close(c.entered)
	<-c.unblock // ignores ctx deliberately — the uncooperative case
	c.seq.record("run:unblocked")
	close(c.exited)
	return nil, errors.New("stonewall: run abandoned")
}

func (c *stonewallCoordinator) CancelAll() bool {
	c.seq.record("shutdown:CancelAll")
	// Latch the real coordinator underneath too, exactly as production
	// cancellation would.
	_ = c.Coordinator.CancelAll()
	select {
	case <-c.exited:
		return false
	case <-time.After(c.joinGrace):
		return true // still busy: the run ignored cancellation
	}
}

// TestCloseForcedWhenRunIgnoresCancellation pins the uncooperative
// counterpart: a run that ignores cancellation entirely must not hang
// Close — the documented bound is two grace windows (one for the drain
// to stall, one for cancellation's join), after which Close returns a
// forced result and leaves the in-memory handles open under the
// still-live writer, for the host to reclaim with
// CloseEphemeralConnsForced once the writers are done.
func TestCloseForcedWhenRunIgnoresCancellation(t *testing.T) {
	provider := newMarkerProvider(t, "CLOSE_STALL_FORCED_OK")
	client := newAdmissionTestClient(t, provider.URL)
	require.NotEmpty(t, client.closeConns, "an ephemeral library-mode client owns in-memory handles")

	seq := &seqRecorder{}
	coord := &stonewallCoordinator{
		Coordinator: client.app.AgentCoordinator,
		seq:         seq,
		entered:     make(chan struct{}),
		unblock:     make(chan struct{}),
		exited:      make(chan struct{}),
		joinGrace:   300 * time.Millisecond,
	}
	client.app.AgentCoordinator = coord

	runDone := make(chan error, 1)
	go func() {
		_, err := client.Run(context.Background(), RunRequest{
			Prompt:      "reply with exactly the marker text and nothing else",
			Mode:        RunModeJSON,
			HideSpinner: true,
		})
		runDone <- err
	}()
	<-coord.entered // parked in "agent work", ignoring cancellation

	closeDone := make(chan CloseResult, 1)
	go func() { closeDone <- client.Close() }()

	var res CloseResult
	select {
	case res = <-closeDone:
	case <-time.After(2*agent.DefaultCancelAllGrace + stallGraceSlack):
		t.Fatal("Close did not return within the documented bound — a forced shutdown must not wait on a run that ignores cancellation")
	}
	require.True(t, res.Forced, "a run still busy after cancellation's join must force the shutdown")
	require.Empty(t, res.CleanupErrors)
	require.Greater(t, seq.indexOf("shutdown:CancelAll"), seq.indexOf("run:stuck"),
		"Close must have fired cancellation while the run was stuck")

	// Forced shutdown leaves the in-memory handles open (live writers),
	// and the bookkeeping agrees.
	for _, conn := range client.closeConns {
		require.NoError(t, conn.PingContext(t.Context()), "forced Close must leave the in-memory handles open")
	}
	require.False(t, client.connsClosed)

	// Now let the stuck run finish — the host's writers are done — and
	// reclaim the handles explicitly, as the forced contract documents.
	close(coord.unblock)
	require.Error(t, <-runDone)
	seq.record("run:returned")
	require.Greater(t, seq.indexOf("run:returned"), seq.indexOf("shutdown:CancelAll"))

	require.NoError(t, client.CloseEphemeralConnsForced())
	require.True(t, client.connsClosed)
	for _, conn := range client.closeConns {
		require.Error(t, conn.PingContext(t.Context()), "the explicit reclaim must actually close the handles")
	}
	// The reclaim is idempotent.
	require.NoError(t, client.CloseEphemeralConnsForced())
}
