package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestTask1314OAuthDiskCancellationSubprocess(t *testing.T) {
	// Revert-check: cancellation must terminate real provider HTTP requests and exchanges without retry or leaked response bodies.
	task1314IntegrationSubprocess(t, "TestTask1314OAuthDiskCancellationChild", "TASK1314_CANCELLATION_CHILD")
}

func TestTask1314OAuthDiskCancellationChild(t *testing.T) {
	// Revert-check: cancellation before a stale 401 reply and inside a real refresh exchange must preserve live disk credentials.
	root := os.Getenv("TASK1314_CANCELLATION_CHILD")
	if root == "" {
		t.Skip("subprocess helper")
	}
	// The child is a separate process: it inherits nothing from the
	// parent's package variables, so the gate must be disabled here too.
	// This is a legacy-JSON fixture: it rewrites rush.json at the literal
	// GlobalConfigData path and expects the reactive disk watcher to see it.
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	for _, mode := range []string{"before401", "inside-exchange"} {
		for _, id := range task1314IntegrationIDs {
			t.Run(id+"/"+mode, func(t *testing.T) {
				// Revert-check: context cancellation forbids a second provider send and closes every returned network response body, including the rejected 401.
				store, path := task1314LoadDiskStore(t)
				old, _ := task1314IntegrationTokens()
				task1314IntegrationSeed(store, id, old)
				task1314WriteAgentDisk(t, path, id, old)
				counted := &task1314CountingStore{ConfigStore: store}
				var bodies task1314IntegrationBodies
				var sends, exchanges, inflight atomic.Int32
				entered := make(chan struct{}, 1)
				ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
				defer cancel()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					inflight.Add(1)
					defer inflight.Add(-1)
					if !task1314IntegrationRequest(r) {
						exchanges.Add(1)
						if mode != "inside-exchange" {
							t.Error("unexpected refresh before401")
							http.Error(w, "unexpected exchange", 400)
							return
						}
						// This is the actual exchange HTTP endpoint, not a mocked source.Refresh.
						select {
						case entered <- struct{}{}:
						case <-ctx.Done():
							return
						}
						select {
						case <-r.Context().Done():
						case <-ctx.Done():
						}
						return
					}
					sends.Add(1)
					if !task1314IntegrationCredentials(r, id, old) {
						t.Error("wrong initial credentials")
						http.Error(w, "bad bearer", 400)
						return
					}
					if mode == "before401" {
						// Cancel the caller BEFORE replying. Refresh may never be entered in this variant.
						cancel()
					}
					task1314IntegrationReply(w, id, true)
				}))
				t.Cleanup(server.Close)
				t.Cleanup(cancel)
				model := task1314IntegrationModel(t, counted, id, server, &bodies)
				before := store.Generation()
				done := make(chan error, 1)
				go func() { _, err := model.Generate(ctx, fantasy.Call{}); done <- err }()
				if mode == "inside-exchange" {
					select {
					case <-entered:
						cancel()
					case <-ctx.Done():
						t.Error("real exchange was not entered")
					}
				}
				select {
				case err := <-done:
					require.ErrorIs(t, err, context.Canceled)
				case <-time.After(10 * time.Second):
					t.Fatal("cancelled model did not join")
				}
				require.Eventually(t, func() bool { return inflight.Load() == 0 && bodies.opened.Load() == bodies.closed.Load() }, 3*time.Second, 10*time.Millisecond, "handlers or response bodies remained live")
				require.EqualValues(t, 1, sends.Load())
				if mode == "inside-exchange" {
					require.EqualValues(t, 1, exchanges.Load())
					require.EqualValues(t, 1, counted.refreshes.Load())
					require.Positive(t, bodies.opened.Load(), "real rejected response body must be observed")
				} else {
					require.Zero(t, exchanges.Load())
					t.Logf("cancel-before401 refresh entries=%d returned bodies=%d", counted.refreshes.Load(), bodies.opened.Load())
				}
				require.Equal(t, before, store.Generation())
				live, ok := store.Config().Providers.Get(id)
				require.True(t, ok)
				require.Equal(t, old, live.OAuthToken)
			})
		}
	}
	if !t.Failed() {
		require.NoError(t, os.WriteFile(filepath.Join(root, "complete"), []byte("ok"), 0o600))
	}
}
