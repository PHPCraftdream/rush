package agent_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestP0_338_FinalizerReachableDespiteHungCleanup empirically proves that the
// ownership finalizer (abandonOwnershipWithHandoff) in sessionAgent.Run is
// ALWAYS reachable, even if SessionLock.Release() hangs on diagnostic cleanup.
//
// This test proves BOTH reachability AND correct execution:
//  1. Reachability: Run() returns quickly despite hung cleanup, proving Release()
//     defer doesn't block the finalizer from being reached.
//  2. Execution: orphaned call (queued before Run) is actually executed by the
//     finalizer's detached run, proven by provider call count and message history.
//
// The test:
//  1. Injects a blocking version of clearHolderMetadataFn via SessionAgentOptions.LockOptions
//     (using session.WithClearHolderMetadataFn).
//  2. Starts a Run() that acquires the OS lock.
//  3. The first call to the provider returns an error via a custom context,
//     causing Run() to error out quickly and trigger the finalizer.
//  4. The run wait is a 30-second HANG DETECTOR, not a latency bound: a
//     reverted fix makes Release() block forever on the injected cleanup, so
//     Run() never returns and the detector fires.
//  5. Verifies that the cleanup goroutine actually started (proves it was spawned).
//  6. Verifies that the OS lock is available even while cleanup is blocked.
//  7. Verifies that the finalizer abandonOwnershipWithHandoff actually ran
//     and correctly handed off the orphaned call via two proofs:
//     a. Provider was called at least twice (once for firstCall, once for orphanedCall)
//     b. orphanedCall.Prompt appears in the session's message history
//
// Since task #340 ROUND 3, the finalizer's detached run durably enqueues the
// orphaned call instead of executing it inline, so this test runs one cold
// synchronous DrainSessionNow on the queue pump to prove the queue is actually
// drained, not just written to.
//
// This is the SAME execution proof pattern as p0_2_regression_test.go
// (TestP0_2_RetryExhaustion_QueuesCall), using httptest.Server with SSE responses
// and atomic counters inside the HTTP handler.
//
// NOTE: This test is NOT parallel because it mutates package-local state that
// would create a data race with other parallel tests doing the same.
//
// REVERT CHECK PROCEDURE:
//  1. In lock.go Release(), change "go cleanupFn(path)" back to "cleanupFn(path)"
//     (remove the "go " keyword) in the background goroutine launch.
//  2. Run: go test ./internal/agent -run TestP0_338_FinalizerReachableDespiteHungCleanup -v
//  3. The test will FAIL via the 30-second hang detector: Release() blocks
//     forever on the injected cleanup, so Run() never returns.
//  4. Restore the fix (add "go " back) and the test will PASS.
func TestP0_338_FinalizerReachableDespiteHungCleanup(t *testing.T) {
	tmpDir := t.TempDir()

	// Track provider calls to prove actual execution (not just queuing).
	var providerCalls atomic.Int64

	// Set up a mock provider that:
	// 1. Returns error on first call to trigger finalizer quickly
	// 2. Returns normal SSE responses on subsequent calls
	providerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callNum := providerCalls.Add(1)

		if callNum == 1 {
			// First call: return error immediately to trigger finalizer
			t.Logf("Provider call %d: returning error to trigger finalizer", callNum)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"message":"synthetic provider failure","type":"invalid_request_error"}}`)
			return
		}

		t.Logf("Provider call %d: returning normal SSE response", callNum)
		// Subsequent calls: return normal SSE response
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"response"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(providerSrv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(providerSrv.URL),
		openaicompat.WithAPIKey("test-key"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := agent.Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
	}

	// Create DB and services.
	// Don't close DB immediately — we need it for detached runs.
	conn, err := db.Connect(t.Context(), tmpDir)
	require.NoError(t, err)
	q := db.New(conn)
	// t.Cleanup runs even on failure paths, so rush.db can't block t.TempDir's
	// RemoveAll if an assertion above bails out early.
	t.Cleanup(func() { require.NoError(t, db.Release(tmpDir)) })
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	// Create the session.
	sess, err := sessions.Create(t.Context(), "test session for p0-338")
	require.NoError(t, err)
	sessionID := sess.ID

	// Add a message so the session isn't empty.
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test message"}},
	})
	require.NoError(t, err)

	// Inject the hung cleanup BEFORE starting Run, so it's active for the Release() defer.
	// We use SessionAgentOptions.LockOptions to inject the hung cleanup function.
	cleanupStarted := atomic.Bool{}
	cleanupUnblock := make(chan struct{})
	// Release() is called at least twice over this test's life (once for
	// firstCall's failing turn, once for the orphaned call's turn), so
	// this callback fires more than once. testDone guards every t.Logf
	// call inside it: closing cleanupUnblock only wakes a blocked
	// invocation, it doesn't wait for its remaining log calls to run, and
	// a goroutine calling t.Logf after the test has been marked done
	// panics the whole binary ("Log in goroutine after Test has
	// completed"). Checking testDone right before each call is safe under
	// ANY number of invocations and ANY interleaving -- unlike waiting on
	// a WaitGroup Added-to from inside the goroutine itself, which would
	// need "Add happens before Wait" to hold for every invocation to
	// avoid missing one; the worst case here is silently dropping a log
	// line already racing test completion, never a panic.
	var testDone atomic.Bool
	safeLogf := func(format string, args ...any) {
		if !testDone.Load() {
			t.Logf(format, args...)
		}
	}

	// Create a sessionAgent with dataDir so it acquires the OS lock, and inject
	// the hung cleanup via LockOptions.
	sa := agent.NewSessionAgent(agent.SessionAgentOptions{
		DataDirectory: tmpDir,
		SmartModel:    model,
		FastModel:     model,
		Sessions:      sessions,
		Messages:      messages,
		IsYolo:        true,
		SystemPrompt:  "You are a test assistant.",
		LockOptions: []session.LockOption{
			session.WithClearHolderMetadataFn(func(path string, expectedGeneration string) {
				safeLogf("cleanup goroutine started for path: %s", path)
				cleanupStarted.Store(true)
				// Hung until the test's defer: a synchronous Release can never return.
				<-cleanupUnblock
				safeLogf("cleanup goroutine unblocking via explicit unblock")
				safeLogf("cleanup goroutine completed")
			}),
		},
	})
	defer func() {
		// Unblock cleanup before test completes. The brief sleep is a
		// best-effort courtesy so normal runs still see the goroutine's
		// final log lines (it wakes and finishes in microseconds); the
		// actual safety net is testDone, set right after -- so a slow or
		// unexpectedly-late invocation degrades to a dropped log line,
		// never a post-completion t.Logf panic.
		close(cleanupUnblock)
		time.Sleep(50 * time.Millisecond)
		testDone.Store(true)
	}()

	// Queue an orphaned call BEFORE starting Run — it will become orphaned when Run fails.
	orphanedCall := agent.SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "orphaned call",
	}
	sa.QueueMessage(orphanedCall)

	// Start the first Run that will acquire the lock and then fail.
	firstCall := agent.SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "first call",
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := sa.Run(t.Context(), firstCall)
		runErrCh <- err
	}()

	// Wait for Run() to return — CRITICAL: it should return quickly despite hung cleanup.
	// If the #337 fix is broken (Release() not running cleanup in background), this will hang forever.
	runStart := time.Now()
	select {
	case runErr := <-runErrCh:
		runDuration := time.Since(runStart)
		currentCalls := providerCalls.Load()
		t.Logf("Run() returned in %v with error: %v, provider calls: %d", runDuration, runErr, currentCalls)
		// With the fix present, Run() returns as soon as the HTTP round trip
		// completes; with the fix reverted, Release() blocks forever on
		// cleanupUnblock, so the 30s wait is a hang detector, not a latency bound.
		require.Error(t, runErr, "Run should fail")
		// The error message indicates Run() failed (expected since our provider returns HTTP 400).
		// What matters is that Run() returned quickly, proving Release() defer didn't block.
	case <-time.After(30 * time.Second):
		currentCalls := providerCalls.Load()
		t.Logf("Provider call count: %d", currentCalls)
		t.Logf("Cleanup started: %v", cleanupStarted.Load())
		require.Fail(t, "Run() never returned - Release() is blocking on cleanup, i.e. fix #337 is broken")
	}

	// Wait for the cleanup goroutine to start (proves Release() reached it).
	// We wait AFTER Run() returns because cleanup goroutine is spawned in Release() defer,
	// which runs after Run() returns.
	deadline := time.After(2 * time.Second)
	for !cleanupStarted.Load() {
		select {
		case <-deadline:
			// If cleanup never started, Release() was never called.
			// This could happen if Run() failed before acquiring the OS lock.
			t.Logf("Provider call count: %d", providerCalls.Load())
			require.Fail(t, "cleanup goroutine did not start within 2 seconds - "+
				"Release() was never called (Run() may have failed before OS lock acquisition)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// CRITICAL VERIFICATION 1: The cleanup goroutine started and is blocked.
	// This proves Release() was called and the background cleanup goroutine was spawned.
	require.True(t, cleanupStarted.Load(),
		"Cleanup goroutine should have started (this proves Release() was called)")

	// CRITICAL VERIFICATION 2: The OS lock should be available even though cleanup is blocked.
	// This proves Release() returned after unlock/close, not after cleanup.
	//
	// Release() unlocks and closes the file before spawning the cleanup goroutine,
	// and the acquire path waits for any pending handoff, so a single TryAcquire is enough.
	lk2, err := session.TryAcquireSessionLock(tmpDir, sessionID)
	require.NoError(t, err, "OS lock should be acquirable after Run returned: Release unlocks before spawning cleanup (lock.go) and acquire waits for any pending handoff")
	require.NotNil(t, lk2)
	_ = lk2.Release()

	// CRITICAL VERIFICATION 3: The finalizer's detached run actually executed the orphaned call.
	// Since task #340 ROUND 3, restartOrphanedWithRetry durably enqueues the call
	// synchronously inside Run's defer, so the queue row already exists when Run returns.
	//
	// The finalizer enqueued synchronously before Run returned, so one cold synchronous
	// drain proves the queue is actually drained; the drain runs the entry to completion,
	// so its DB writes finish before db.Release in t.Cleanup.
	pumpCoord := &p0338PumpCoordinator{sessionAgent: sa}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       sessions,
		Coordinator:    pumpCoord,
		PumpInstanceID: "test-pump-p0-338",
	})
	drainCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, drainErr := pump.DrainSessionNow(drainCtx, sessionID)
	require.NoError(t, drainErr, "synchronous drain of the session queue should succeed")
	require.Equal(t, session.DrainComplete, result, "synchronous drain should execute the orphaned call end to end")

	require.GreaterOrEqual(t, providerCalls.Load(), int64(2),
		"Finalizer's detached run should have executed orphaned call "+
			"(provider call count >= 2, got %d)", providerCalls.Load())

	// CRITICAL VERIFICATION 4: The orphaned call's prompt appears in message history.
	// This proves the call was actually executed, not just queued.
	msgs, err := messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var found bool
	for _, m := range msgs {
		for _, part := range m.Parts {
			if tc, ok := part.(message.TextContent); ok && tc.Text == "orphaned call" {
				found = true
			}
		}
	}
	require.True(t, found, "Orphaned call prompt should appear in message history "+
		"(proving the call was actually executed, not just queued)")
}

// p0338PumpCoordinator adapts session.SessionAgentCallData to agent.SessionAgentCall
// and executes it through the exported agent.SessionAgent interface, mirroring
// production's coordinatorAdapterImpl (internal/app/app.go) without needing the
// unexported *coordinator/*sessionAgent types this external test package can't see.
type p0338PumpCoordinator struct {
	sessionAgent agent.SessionAgent
}

func (p *p0338PumpCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	call, err := agent.FromSessionAgentCallData(callData)
	if err != nil {
		return nil, err
	}
	// Mirror production's coordinator.RebuildSessionAgentCall (coordinator.go):
	// mark this call as originating from the durable queue so mailbox.submit
	// skips mb.submitted for it (P0-1, closing-review round).
	call.FromDurableQueue = true
	result, err := p.sessionAgent.Run(ctx, call)
	if err != nil {
		return nil, err
	}
	var anyResult any
	if result != nil {
		anyResult = result
	}
	return &anyResult, nil
}
