// Release gate tests for the durable run queue and its autonomous pump:
// orphaned-retry enqueue (gate 2), cross-process interrupt inject (gate 3),
// and double-failure no-duplicate execution (gate 9).

package agent

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestReleaseGate_2_OSLockHeldPastRetryWindow proves that calls accepted through
// detached path (restartOrphanedWithRetry/abandonOwnershipWithHandoff) execute
// autonomously via REAL pump after OS lock becomes available.
//
// CRITERION: Hold OS lock past retry window, then RELEASE → already-accepted
//
//	call executes autonomously by REAL pump (NO manual startDetachedRun/Run)
//
// NO EXTERNAL POKE: Test holds OS lock, then RELEASES it. The pump autonomously
// detects the queued entry and executes it. We do NOT call startDetachedRun manually.
//
// REVERT CHECK PROCEDURE:
//  1. In agent.go restartOrphanedWithRetry (~line 1057), comment out EnqueueRunQueueEntry
//  2. Run: go test -run TestReleaseGate_2_OSLockHeldPastRetryWindow -v
//  3. FAIL: Queue empty, no execution, provider never called
//  4. Restore EnqueueRunQueueEntry and PASS
func TestReleaseGate_2_OSLockHeldPastRetryWindow(t *testing.T) {
	t.Parallel()

	// Track provider calls to prove actual execution.
	var providerCalls atomic.Int64

	// Set up a mock provider that responds normally.
	busyLockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
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
	t.Cleanup(busyLockSrv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(busyLockSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel:           model,
		FastModel:            model,
		SystemPrompt:         "you are a probe",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()

	// Create the session.
	sess, err := env.sessions.Create(ctx, "release-gate-2")
	require.NoError(t, err)
	sessionID := sess.ID

	// Add a seed message.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test"}},
	})
	require.NoError(t, err)

	// Hold the OS lock to force restartOrphanedWithRetry to fail.
	lock, err := session.TryAcquireSessionLock(env.workingDir, sessionID)
	require.NoError(t, err, "should acquire OS lock to force retry exhaustion")

	// Call that will be orphaned and retried.
	orphanedCall := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "orphaned call",
	}

	// Start a detached run that will exhaust retries and enqueue the call.
	sessionAgent.restartOrphanedWithRetry([]SessionAgentCall{orphanedCall})

	// Wait for durable enqueue to complete.
	time.Sleep(100 * time.Millisecond)

	// PHASE 1: PROVE PERSISTENCE
	pendingEntries, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingEntries, 1, "orphaned call should be queued in durable run queue")
	require.Equal(t, sessionID, pendingEntries[0].SessionID)

	// PHASE 2: PROVE AUTONOMOUS EXECUTION
	// Start pump BEFORE releasing lock - pump will poll but can't acquire lock yet.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       env.sessions,
		Coordinator:    &p0338PumpCoordinator{sessionAgent: sa},
		PumpInstanceID: "release-gate-2-pump",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// CRITICAL: Release the OS lock - this is the ONLY "action" we take.
	// The pump AUTONOMOUSLY detects the queued entry and executes it.
	lock.Release()

	// Wait for pump to autonomously process the queued call.
	require.Eventually(t, func() bool {
		pending, checkErr := env.sessions.ListPendingRunQueueEntries(ctx)
		if checkErr != nil {
			return false
		}
		return len(pending) == 0 && providerCalls.Load() >= 1
	}, 10*time.Second, 100*time.Millisecond,
		"pump should autonomously process queued call after OS lock released")

	// Verify the provider was called (autonomous execution).
	require.GreaterOrEqual(t, providerCalls.Load(), int64(1),
		"provider should have been called for queued call")

	// Verify the orphaned call's content is in the message history.
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	var foundOrphanedContent bool
	for _, m := range msgs {
		for _, part := range m.Parts {
			if tc, ok := part.(message.TextContent); ok && tc.Text == "orphaned call" {
				foundOrphanedContent = true
				break
			}
		}
		if foundOrphanedContent {
			break
		}
	}
	require.True(t, foundOrphanedContent,
		"orphaned call's content should appear in message history after autonomous execution")
}

// TestReleaseGate_3_CrossProcessInterruptAutoResumed proves that a call
// enqueued via startDetachedRun (the real production entry point for
// `crush sessions inject --interrupt` recovering from cross-process OS-lock
// contention) is automatically picked up and executed by the autonomous
// pump, without the test manually driving execution.
//
// CRITERION: Cross-process interrupt → durably enqueued call automatically
//
//	picked up and EXECUTED by the autonomous pump (no manual Run()/
//	tick() call as a trigger).
//
// NO EXTERNAL POKE, precisely stated: this test DOES call
// coord.startDetachedRun(ctx, call) directly (see below) — that is
// deliberate and correct, not a poke. startDetachedRun IS the real
// production call site coordinator.go uses for this exact recovery path
// (see its call sites in the actual interrupt-inject/detached-run
// handling); calling it here simulates "the production code already ran
// its enqueue step", not "the test manually executed the work". What the
// test does NOT do is call Run()/tick()/executeEntry as a manual trigger
// for EXECUTION — that part is left entirely to the autonomous
// session.RunQueuePump started below, on its own TestTick.
//
// One correction from an earlier version of this doc comment: it used to
// describe a revert-check targeting coordinator.go's
// recreatePendingInjectRow/CreatePendingInject. That function is ONLY
// called on startDetachedRun's two ERROR paths (json.Marshal failing, or
// EnqueueRunQueueEntry failing) — see coordinator.go's startDetachedRun.
// This test's enqueue succeeds normally, so recreatePendingInjectRow is
// never reached, and that revert-check could never have produced the
// claimed FAIL. The corrected procedure below targets the mechanism this
// test actually exercises.
//
// REVERT CHECK PROCEDURE:
//  1. In coordinator.go's startDetachedRun, comment out the
//     `c.sessions.EnqueueRunQueueEntry(ctx, idempotencyKey, call.SessionID, callDataJSON)`
//     call (or force it to always return early without enqueuing).
//  2. Run: go test -run TestReleaseGate_3_CrossProcessInterruptAutoResumed -v
//  3. FAIL: `require.Len(t, pendingEntries, 1, "call should be enqueued in
//     durable run queue")` fails immediately (0 entries) — the call is
//     never durably recorded, so the pump has nothing to pick up and the
//     provider is never called.
//  4. Restore the EnqueueRunQueueEntry call and PASS.
func TestReleaseGate_3_CrossProcessInterruptAutoResumed(t *testing.T) {
	t.Parallel()

	var providerCalls atomic.Int64

	busyLockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
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
	t.Cleanup(busyLockSrv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(busyLockSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}

	env := testEnv(t)
	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel:           model,
		FastModel:            model,
		SystemPrompt:         "you are a probe",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	coord := &coordinator{
		cfg:          &config.ConfigStore{},
		sessions:     env.sessions,
		messages:     env.messages,
		currentAgent: sessionAgent,
	}

	ctx := context.Background()

	sess, err := env.sessions.Create(ctx, "release-gate-3")
	require.NoError(t, err)

	// Create a user message (simulating `crush sessions inject`).
	msg, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupted message"}},
	})
	require.NoError(t, err)

	// Create a pending_injects row (simulating the CLI creating it).
	inject := session.PendingInject{
		ID:        "test-inject-id-release-gate-3",
		SessionID: sess.ID,
		MessageID: msg.ID,
		Content:   msg.FullText(),
		Interrupt: true,
	}
	err = env.sessions.CreatePendingInject(ctx, inject)
	require.NoError(t, err)

	// Build the call manually (for testing purposes).
	call := SessionAgentCall{
		SessionID:         sess.ID,
		Prompt:            msg.FullText(),
		ExistingMessageID: msg.ID,
		InjectID:          inject.ID,
	}

	// Call startDetachedRun - it should enqueue the call to durable run queue.
	require.NoError(t, coord.startDetachedRun(ctx, call))

	// Wait for durable enqueue to complete.
	time.Sleep(100 * time.Millisecond)

	// Verify the call was enqueued
	pendingEntries, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingEntries, 1, "call should be enqueued in durable run queue")

	// Start pump - it will AUTONOMOUSLY process the queued call.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       env.sessions,
		Coordinator:    &p0338PumpCoordinator{sessionAgent: sa},
		PumpInstanceID: "release-gate-3-pump",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// Wait for pump to autonomously process and execute the queued call.
	require.Eventually(t, func() bool {
		pending, checkErr := env.sessions.ListPendingRunQueueEntries(ctx)
		if checkErr != nil {
			return false
		}
		return len(pending) == 0 && providerCalls.Load() >= 1
	}, 10*time.Second, 100*time.Millisecond,
		"pump should autonomously process and execute the queued interrupt call")

	// Verify the provider was called (autonomous execution).
	require.GreaterOrEqual(t, providerCalls.Load(), int64(1),
		"provider should have been called for interrupt message")

	// Verify the original user message is still in history.
	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	var foundOriginalMessage bool
	for _, m := range msgs {
		if m.ID == msg.ID {
			foundOriginalMessage = true
			require.Contains(t, m.FullText(), "interrupted message",
				"original message content should be preserved")
			break
		}
	}
	require.True(t, foundOriginalMessage,
		"original user message should exist after autonomous execution")
}

// TestReleaseGate_9_DoubleFailureNoDuplicate proves that when a detached run
// creates a user message then fails via ErrCallAlreadyAttempted, nothing is
// lost and no duplicate user messages are created.
//
// CRITERION: Detached run creates user message → fails via ErrCallAlreadyAttempted
//
//	→ entry deleted, len(mb.submitted)==0 AND exactly ONE persistent user message.
//
// This test specifically targets the ErrCallAlreadyAttempted fast path (not
// max-attempts exhaustion). We verify this by:
// 1. Directly enqueuing a call to the durable queue (via restartOrphanedWithRetry)
// 2. Configuring the mock message service to fail AFTER the user message is created
// 3. Verifying the entry is deleted (not retried)
// 4. Verifying there's exactly ONE user message (not zero, not duplicate)
//
// HONEST LIMITATION (found while hardening this test — read before "fixing"
// a revert-check that appears not to isolate the ErrCallAlreadyAttempted
// path): run_queue_pump.go has TWO independently-correct mechanisms that
// both converge on "entry removed, no retry, no duplicate" for this
// scenario — (1) the ErrCallAlreadyAttempted classification (task #339,
// fires on the FIRST attempt) and (2) the max-attempts-exhaustion terminal
// fail (fixed in this same round, fires after RunQueueMaxAttempts=10 Nack
// cycles). Disabling ONLY mechanism (1) — the intuitive revert-check — does
// NOT make this test fail: mechanism (2) independently cleans up the same
// entry, just slower (~1s of retries vs. one ~100-300ms pump tick), and by
// the time any observation window this test could reasonably use elapses,
// the outcome is indistinguishable from the outside (same len(pending)==0,
// same message count, same mb.submitted==0). A wall-clock-timing assertion
// was tried and discarded: require.Eventually's own poll can itself
// false-positive on a transiently-'leased' (not yet re-Nacked) row, making
// "time to first pending==0" an unreliable proxy for "which mechanism fired"
// — this was verified empirically, not assumed.
//
// What this test DOES prove, unconditionally regardless of which mechanism
// fires: under a double-failure (owner turn's persisted call + this
// detached run's failure), the system never silently drops the call
// (mb.submitted stays empty) and never persists it twice (exactly one user
// message). That is the actual safety property criterion 9 cares about; it
// holds by construction whichever of the two mechanisms resolves the entry.
// The ErrCallAlreadyAttempted-specific fast path itself is separately
// covered by p339_no_duplicate_execution_test.go's own tests, which don't
// have this fallback-masking problem because they don't also drive the
// entry to max-attempts exhaustion.
//
// REVERT CHECK PROCEDURE (for the underlying no-loss/no-duplicate property,
// not for isolating which of the two mechanisms fired):
//  1. In run_queue_pump.go, comment out the ENTIRE processEntry function
//     body's failure handling (both the AlreadyAttempted branch AND the
//     max-attempts-exhaustion branch), replacing both with a bare Nack that
//     never terminal-fails — i.e. remove BOTH safety nets, not just one.
//  2. Run: go test -run TestReleaseGate_9_DoubleFailureNoDuplicate -v -timeout 30s
//  3. FAIL: require.Eventually times out after 10s (pending never reaches 0 —
//     genuinely stuck in an infinite Nack loop with neither safety net).
//  4. Restore processEntry and PASS.
func TestReleaseGate_9_DoubleFailureNoDuplicate(t *testing.T) {
	t.Parallel()

	var providerCalls atomic.Int64
	busyLockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`,
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
	t.Cleanup(busyLockSrv.Close)

	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(busyLockSrv.URL),
		openaicompat.WithAPIKey("probe"),
	)
	require.NoError(t, err)
	lm, err := provider.LanguageModel(context.Background(), "probe")
	require.NoError(t, err)

	model := Model{
		Model:      lm,
		CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 1000},
	}

	env := testEnv(t)

	// Wrap the message service with our mock that fails AFTER user message
	// creation. mockMessageService.Create/Update/List all share ONE monotonic
	// counter and fail once count > failAfterCalls (see p339_no_duplicate_execution_test.go).
	//
	// Measured empirically (by sweeping failAfterCalls 1..4 and logging the
	// actual error coordinator.Run() returns to the pump — see git history
	// of this file if this ever needs re-deriving):
	//   failAfterCalls=1: fails ON the user-message Create call itself —
	//     agent.go's local userMessageCreated never becomes true, so the
	//     error is NOT wrapped in ErrCallAlreadyAttempted; it's retried
	//     forever (wrong scenario for this test).
	//   failAfterCalls=2: fails on the NEXT call after the user message is
	//     durably created — coordinator.Run() returns an error whose
	//     .Error() text is prefixed "call already attempted: ..." (i.e.
	//     genuinely wrapped in ErrCallAlreadyAttempted). This is the
	//     scenario criterion 9 asks for.
	//   failAfterCalls=3: the failing call turns out to be non-critical
	//     (best-effort persistence that gets logged but doesn't fail the
	//     turn) — coordinator.Run() returns err=nil, so NOTHING in this
	//     test exercises a failure at all; it passes for the wrong reason
	//     regardless of the AlreadyAttempted classification.
	// If this call count ever drifts (a future change adds/removes a
	// message-service call along the turn's error/success path), rerun the
	// revert-check below — it must show a genuine FAIL when the
	// classification is disabled; if it doesn't, re-derive failAfterCalls
	// the same way (sweep + log the error coordinator.Run() actually
	// returns, don't guess from reading agent.go alone — the call ordering
	// depends on runtime state, not just source order).
	mockMessages := &mockMessageService{
		inner:               env.messages,
		failAfterCalls:      2,
		failWith:            fmt.Errorf("simulated DB failure in error-handling path"),
		userMessageTracking: true,
	}

	sa := NewSessionAgent(SessionAgentOptions{
		SmartModel:           model,
		FastModel:            model,
		SystemPrompt:         "you are a probe",
		DataDirectory:        env.workingDir,
		Sessions:             env.sessions,
		Messages:             mockMessages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	sessionAgent := sa.(*sessionAgent)

	ctx := context.Background()

	sess, err := env.sessions.Create(ctx, "release-gate-9")
	require.NoError(t, err)
	sessionID := sess.ID

	// Add a seed message so the call is not the first message.
	// IMPORTANT: Use env.messages (not mockMessages) to avoid counting towards failAfterCalls
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	// Directly enqueue the call to the durable queue (simulating a detached run scenario).
	// This avoids the double registration issue of the previous test.
	callB := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "call B that will trigger ErrCallAlreadyAttempted",
	}
	sessionAgent.restartOrphanedWithRetry([]SessionAgentCall{callB})

	// Wait for durable enqueue to complete.
	time.Sleep(100 * time.Millisecond)

	// Verify that the call was enqueued.
	pendingEntries, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pendingEntries, 1, "call should be enqueued in durable run queue")
	entryID := pendingEntries[0].ID
	t.Logf("Entry enqueued: %s", entryID)

	// Verify mb.submitted is EMPTY (no double registration via QueueMessage).
	mb := sessionAgent.getMailbox(sessionID)
	require.NotNil(t, mb, "mailbox should exist")
	require.Equal(t, 0, len(mb.submitted),
		"mb.submitted should be empty - no double registration")

	// Start pump with TestTick - it will AUTONOMOUSLY execute the queued call.
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       env.sessions,
		Coordinator:    &p0338PumpCoordinator{sessionAgent: sa},
		PumpInstanceID: "release-gate-9-pump",
		TestTick:       func() time.Duration { return 100 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// Wait for pump to process the call and delete the entry via
	// TerminalFailRunQueueEntry.
	//
	// len(pending)==0 alone is NOT a sufficient wait condition:
	// ListPendingRunQueueEntries only scans status='pending', so an entry
	// that is currently 'leased' — the pump has claimed it and is running it
	// RIGHT NOW — reads as zero pending too. Waiting on that alone returns
	// while the run is still mid-flight, and every message-service call the
	// run is about to make lands AFTER the callCountAtTerminal snapshot
	// below, i.e. gets misread as "a retry loop kept calling". That is a
	// pure test-side race, and it is what failed on the macOS -race runner
	// (callCountAtTerminal==0, then 3 calls arrived during the hold loop)
	// while passing on faster machines that never observe the lease window.
	//
	// So wait for the row to be gone from BOTH states — that, and only that,
	// is "terminally removed".
	entryStillQueued := func() bool {
		pending, checkErr := env.sessions.ListPendingRunQueueEntries(ctx)
		if checkErr != nil {
			return true
		}
		for _, e := range pending {
			if e.ID == entryID {
				return true
			}
		}
		// math.MaxInt64 as the cutoff makes this "all leased entries",
		// not just the stale ones.
		leased, checkErr := env.sessions.ListStaleLeasedRunQueueEntries(ctx, math.MaxInt64)
		if checkErr != nil {
			return true
		}
		for _, e := range leased {
			if e.ID == entryID {
				return true
			}
		}
		return false
	}
	require.Eventually(t, func() bool { return !entryStillQueued() },
		10*time.Second, 100*time.Millisecond,
		"pump should delete the entry via ErrCallAlreadyAttempted path")

	// Now hold past several more TestTick cycles (well beyond
	// RunQueueMaxAttempts's worst case at this tick rate) and require the
	// entry STAYS gone and mockMessages sees no further calls — that is the
	// signature of terminal removal vs. an in-flight retry loop.
	// (mockMessages.callCount, not providerCalls: the induced failure lands
	// on a pre-provider call in this scenario, so the provider is never hit
	// at all — 0 calls is the correct/expected count here, not a bug.)
	callCountAtTerminal := mockMessages.callCount.Load()
	for i := 0; i < 5; i++ {
		time.Sleep(150 * time.Millisecond)
		require.False(t, entryStillQueued(),
			"entry must STAY deleted, not just transiently absent between retries")
		require.Equal(t, callCountAtTerminal, mockMessages.callCount.Load(),
			"no further message-service calls should happen after terminal-fail — a retry loop would keep calling")
	}

	// Verify the entry was deleted from the queue.
	pending, err := env.sessions.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "entry should be deleted from queue")

	// CRITICAL VERIFICATION 1: len(mb.submitted) == 0 - nothing lost in in-memory queue
	require.Equal(t, 0, len(mb.submitted),
		"mb.submitted should be empty - nothing lost in in-memory queue")

	// CRITICAL VERIFICATION 2: Exactly ONE user message with call B's content in history (not zero, not duplicate)
	// Use env.messages (not mockMessages) since mockMessages is already failing
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	userMessageCount := 0
	var foundCallBContent bool
	for _, m := range msgs {
		for _, part := range m.Parts {
			if tc, ok := part.(message.TextContent); ok {
				if tc.Text == "call B that will trigger ErrCallAlreadyAttempted" {
					userMessageCount++
					foundCallBContent = true
				}
			}
		}
	}

	require.True(t, foundCallBContent,
		"should find call B's user message in history")
	require.Equal(t, 1, userMessageCount,
		"should have exactly ONE user message with call B's content (not zero, not duplicate)")
}
