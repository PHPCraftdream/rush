// Release gate test for lock-release independence: a metadata cleanup
// goroutine blocked forever must not delay Run() or the OS lock.
// Holds TestReleaseGate_1_MetadataCleanupBlockedForever.

package agent

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
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestReleaseGate_1_MetadataCleanupBlockedForever proves that metadata cleanup
// being blocked forever does NOT prevent new Run() calls on the same session.
//
// CRITERION: Block metadata cleanup forever → OS lock released, mailbox idle,
//
//	NEW Run() executes successfully WITHOUT unblocking the hung cleanup goroutine.
//
// NO EXTERNAL POKE: This test does NOT unblock cleanup. It relies entirely on
// the autonomous OS lock release in SessionLock.Release() which runs cleanup
// in a background goroutine, not blocking the return path.
//
// REVERT CHECK PROCEDURE:
//  1. In session/lock.go Release(), change "go cleanupFn(path)" to "cleanupFn(path)"
//  2. Run: go test -run TestReleaseGate_1_MetadataCleanupBlockedForever -v
//  3. FAIL: Run() hangs forever on Release() because cleanup blocks synchronously
//  4. Restore "go cleanupFn(path)" and PASS
func TestReleaseGate_1_MetadataCleanupBlockedForever(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Set up a mock provider that responds normally.
	var providerCalls atomic.Int64
	providerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
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

	// Create DB and services.
	conn, err := db.Connect(t.Context(), tmpDir)
	require.NoError(t, err)
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	// Create the session.
	sess, err := sessions.Create(t.Context(), "release-gate-1")
	require.NoError(t, err)
	sessionID := sess.ID

	// Add a message so the session isn't empty.
	_, err = messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "test message"}},
	})
	require.NoError(t, err)

	// Inject a PERMANENTLY BLOCKED cleanup - we NEVER unblock it.
	// This is KEY DIFFERENCE from p0_338 which eventually unblocks.
	cleanupStarted := atomic.Bool{}
	cleanupUnblock := make(chan struct{})
	t.Cleanup(func() { close(cleanupUnblock) })

	sa2 := NewSessionAgent(SessionAgentOptions{
		DataDirectory: tmpDir,
		SmartModel: Model{
			Model:      lm,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		FastModel: Model{
			Model:      lm,
			CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		Sessions:     sessions,
		Messages:     messages,
		IsYolo:       true,
		SystemPrompt: "You are a test assistant.",
		LockOptions: []session.LockOption{
			session.WithClearHolderMetadataFn(func(path string, expectedGeneration string) {
				cleanupStarted.Store(true)
				// PERMANENTLY BLOCK
				select {
				case <-time.After(10 * time.Second):
					// Timeout to allow test to complete
				case <-cleanupUnblock:
				}
			}),
		},
	})

	// Start a Run() that will acquire the lock and return.
	// CRITICAL: This Run() SUCCEEDS and returns quickly because
	// the cleanup goroutine is running in background.
	firstCall := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "first call",
	}

	runErrCh := make(chan error, 1)
	runStart := time.Now()
	go func() {
		_, err := sa2.Run(t.Context(), firstCall)
		runErrCh <- err
	}()

	// Wait for Run() to return - should be QUICK despite blocked cleanup
	select {
	case runErr := <-runErrCh:
		runDuration := time.Since(runStart)
		// Bound is releaseMetadataCleanupBound (50ms, see internal/session/
		// lock.go) plus generous headroom for scheduling jitter on loaded
		// CI runners (flagged as a latent Windows flake risk in the final
		// @oh review of tasks #337-349, P3) — this test's cleanup fn is
		// permanently blocked for the whole test, so Run() always pays the
		// full bound, not just occasionally.
		require.Less(t, runDuration, 500*time.Millisecond,
			"Run() should return quickly despite hung cleanup, got %v", runDuration)
		// Run() should SUCCEED (not fail) because it completes before cleanup finishes
		require.NoError(t, runErr, "Run should succeed")
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return within 5s - cleanup is NOT running in background")
	}

	// Wait for the cleanup goroutine to start (proves Release() reached it).
	deadline := time.After(2 * time.Second)
	for !cleanupStarted.Load() {
		select {
		case <-deadline:
			t.Fatal("cleanup goroutine did not start within 2s - Release() was never called")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// CRITICAL VERIFICATION 1: The cleanup goroutine started.
	require.True(t, cleanupStarted.Load(),
		"Cleanup goroutine should have started (this proves Release() was called)")

	// CRITICAL VERIFICATION 2: OS lock should be available even though cleanup is blocked.
	var lk2 *session.SessionLock
	require.Eventually(t, func() bool {
		var err error
		lk2, err = session.TryAcquireSessionLock(tmpDir, sessionID)
		return err == nil && lk2 != nil
	}, 2*time.Second, 10*time.Millisecond,
		"OS lock should be acquirable even though cleanup is blocked")
	require.NotNil(t, lk2)
	_ = lk2.Release()

	// CRITICAL VERIFICATION 3: NEW Run() should succeed WITHOUT unblocking cleanup.
	newCall := SessionAgentCall{
		SessionID: sessionID,
		Prompt:    "new call",
	}

	_, err = sa2.Run(t.Context(), newCall)
	require.NoError(t, err, "NEW Run() should succeed without unblocking cleanup")

	// Clean up DB.
	require.NoError(t, db.Release(tmpDir))
}
