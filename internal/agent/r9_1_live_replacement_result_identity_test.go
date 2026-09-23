package agent

// R9-1 (round-9 audit) regression coverage.
//
// The bug: runInternal's OnAssistantMessageCreated (attemptEvidence,
// R3-1/R7-1, and through it the R8-3 CallResultRecorder) is armed on the
// ORIGINAL SessionAgentCall only. sessionAgent.runOwned is a dispatcher
// loop: when a legitimate in-process InterruptAndReplace lands while the
// original turn is streaming, runTurn's isCancelErr branch drains the
// replacement via drainAfterCancel and returns hasNext=true, and runOwned
// continues its OWN loop with that replacement as the next call --
// still the SAME dispatcher invocation the original caller is waiting on.
// But the replacement's SessionAgentCall was built by buildCall
// (coordinator_run.go, shared by InterruptAndSend and
// RunWithReservedOwnership), which has no way to know about the caller's
// identity-capture callback, so it carries no OnAssistantMessageCreated of
// its own. Without runOwned carrying the original's callback over, the
// caller's own identity capture silently stops observing anything from
// the point of replacement onward -- exactly the shape a genuinely
// independent LATER caller's turn would have, which this is not: the
// session was never released, the replacement was explicitly accepted by
// THIS dispatcher.
//
// This test proves runOwned's carry-over fixes that gap directly, using
// the same real end-to-end technique as
// TestRun_LateInterruptReplacement_SurvivesNormalCompletion_P0A (a real
// sessionAgent, a real mailbox, the real production interruptAndReplace)
// but exercising the OPPOSITE code path deliberately: here the cancel
// func IS invoked, so the original's stream genuinely ends in
// context.Canceled and runTurn's isCancelErr/drainAfterCancel branch is
// what recovers the replacement -- R9-1's own scenario, not P0-A's
// late-cancel-loses-the-race one.
//
// Verified by revert: removing the `next.OnAssistantMessageCreated =
// call.OnAssistantMessageCreated` carry-over in runOwned (agent_run.go)
// makes this test fail: recordedIDs never contains the replacement's own
// assistant row ID, because the replacement's call has a nil callback.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunOwned_ReplacementInheritsOriginalCallsIdentityCallback_R9_1(t *testing.T) {
	env := testEnv(t)
	dataDir := t.TempDir()
	var calls atomic.Int64

	sess, err := env.sessions.Create(t.Context(), "r9-1 replacement inherits identity callback")
	require.NoError(t, err)

	requestStarted := make(chan struct{}, 1)
	proceed := make(chan struct{})
	srv := requestStartedSSEServer(&calls, requestStarted, proceed)
	t.Cleanup(srv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(srv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	titleSrv := singleTurnSSEServer(nil)
	t.Cleanup(titleSrv.Close)
	titleProvider, err := openaicompat.New(
		openaicompat.WithBaseURL(titleSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	titleLM, err := titleProvider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:           Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		FastModel:            Model{Model: titleLM, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000}},
		SystemPrompt:         "you are a probe",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
		DataDirectory:        dataDir,
	})
	sa := agentIface.(*sessionAgent)

	var mu sync.Mutex
	var recordedIDs []string
	recordCallback := func(id string) {
		mu.Lock()
		recordedIDs = append(recordedIDs, id)
		mu.Unlock()
	}

	runDone := make(chan error, 1)
	go func() {
		_, runErr := agentIface.Run(t.Context(), SessionAgentCall{
			SessionID:                 sess.ID,
			Prompt:                    "original turn, about to be genuinely canceled by a live replacement",
			MaxOutputTokens:           1000,
			OnAssistantMessageCreated: recordCallback,
		})
		runDone <- runErr
	}()

	select {
	case <-requestStarted:
	case r := <-runDone:
		t.Fatalf("Run returned before the original turn reached the provider (err=%v)", r)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the original turn to reach the provider")
	}

	// Land the replacement via the REAL production mailbox method, exactly
	// like coordinator.InterruptAndSend does for a live turn -- and unlike
	// the P0-A test, actually invoke the returned cancel: this test targets
	// runTurn's isCancelErr/drainAfterCancel path, R9-1's own scenario.
	mb := sa.getMailbox(sess.ID)
	cancel, hadOwner := mb.interruptAndReplace(SessionAgentCall{
		SessionID:       sess.ID,
		Prompt:          "the accepted replacement -- deliberately has NO OnAssistantMessageCreated of its own, matching buildCall's real shape",
		MaxOutputTokens: 1000,
	})
	require.True(t, hadOwner, "interruptAndReplace must report a live turn to interrupt")
	cancel()

	// requestStartedSSEServer's handler blocks on proceed regardless of the
	// client's own cancellation, so unblock it to let the first request's
	// goroutine actually exit (its write after cancellation is simply
	// discarded by the already-torn-down connection).
	close(proceed)

	select {
	case <-requestStarted:
		// Second signal: the replacement reached the provider.
	case r := <-runDone:
		t.Fatalf("Run returned (err=%v) before the replacement ever reached the provider", r)
	case <-time.After(10 * time.Second):
		t.Fatalf("replacement never reached the provider within 10s (calls=%d)", calls.Load())
	}

	select {
	case runErr := <-runDone:
		require.NoError(t, runErr, "Run must complete cleanly once the accepted replacement finishes")
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return within 15s after the replacement started")
	}

	assert.Equal(t, int64(2), calls.Load(), "expected exactly 2 provider calls: the canceled original plus the recovered replacement")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var replacementRowID string
	for _, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		if fp := m.FinishPart(); fp != nil && fp.Reason == message.FinishReasonEndTurn {
			replacementRowID = m.ID
		}
	}
	require.NotEmpty(t, replacementRowID, "the replacement must have committed a successful (end_turn) assistant row")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, recordedIDs, "the original call's OnAssistantMessageCreated must have fired at least once")
	assert.Contains(t, recordedIDs, replacementRowID,
		"R9-1: the original call's identity-capture callback must observe the accepted replacement's own row, "+
			"not just the superseded original's -- otherwise a call-result recorder built from this callback "+
			"would report the CANCELED original as the outcome instead of the successful replacement")
}
