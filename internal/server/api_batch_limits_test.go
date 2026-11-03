package server

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// gatedBatchProvider blocks every model request until release is called (or the
// request is cancelled), so a submitted root stays live for the test.
func gatedBatchProvider() (http.HandlerFunc, func()) {
	gate := make(chan struct{})
	var once sync.Once
	handler := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, batchSSEText("released"))
	}
	return handler, func() { once.Do(func() { close(gate) }) }
}

func batchBinding(roots, parallel int) config.APITokenBinding {
	return config.APITokenBinding{Kind: "batch", Batch: &config.APITokenBatchBinding{MaxRoots: roots, MaxParallel: parallel}}
}

func TestBatchIntakeMaxRootsIsEnforcedWith429AndRetryAfter(t *testing.T) {
	provider, release := gatedBatchProvider()
	f := newBatchFixture(t, provider)
	t.Cleanup(release)
	token := f.token(t, "roots", []string{"read", "run"}, "ask", batchBinding(1, 1))
	first := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "one"}).accepted(t)

	over := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "two"})
	require.Equal(t, http.StatusTooManyRequests, over.Status, string(over.Body))
	require.Equal(t, "5", over.Header.Get("Retry-After"))

	// An idempotent replay of the live root is not a new root and is not limited.
	keyed := f.do(t, token, http.MethodGet, "/batches/"+first.RootID, nil).batch(t)
	require.False(t, keyed.Done)

	release()
	f.waitBatchDone(t, token, first.RootID)
	f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "three"}).accepted(t)
}

func TestBatchIntakeLeafBudgetIsSharedAcrossTokens(t *testing.T) {
	provider, release := gatedBatchProvider()
	f := newBatchFixture(t, provider)
	t.Cleanup(release)
	a := f.token(t, "a", []string{"run"}, "ask", batchBinding(2, 4))
	b := f.token(t, "b", []string{"run"}, "ask", batchBinding(2, 4))
	c := f.token(t, "c", []string{"run"}, "ask", batchBinding(2, 4))
	f.do(t, a, http.MethodPost, "/tasks", map[string]any{"prompt": "a"}).accepted(t)
	f.do(t, b, http.MethodPost, "/tasks", map[string]any{"prompt": "b"}).accepted(t)
	over := f.do(t, c, http.MethodPost, "/tasks", map[string]any{"prompt": "c"})
	require.Equal(t, http.StatusTooManyRequests, over.Status, "API leaves together stay under half of the engine slots: %s", over.Body)
	require.NotEmpty(t, over.Header.Get("Retry-After"))
}

func TestBatchIntakeStopCancelsTheRoot(t *testing.T) {
	provider, release := gatedBatchProvider()
	f := newBatchFixture(t, provider)
	t.Cleanup(release)
	token := f.token(t, "stop", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "long"}).accepted(t)
	stop := f.do(t, token, http.MethodPost, "/batches/"+accepted.RootID+"/stop", nil)
	require.Equal(t, http.StatusAccepted, stop.Status, string(stop.Body))
	done := f.waitBatchDone(t, token, accepted.RootID)
	require.Equal(t, "cancelled", done.State, "the watcher pumped the stop request without any client action")
	release()
}

func TestBatchIntakeWatcherReleasesATerminalRootWithoutAClient(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	f.service.outputTTL = 50 * time.Millisecond
	token := f.token(t, "release", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "p"}).accepted(t)
	owner := serviceSessionID(token2ID(t, f, token))
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		// The watcher removes its entry only after it settled the debt and released the root.
		f.service.mu.Lock()
		_, watched := f.service.watching[accepted.RootID]
		f.service.mu.Unlock()
		if !watched {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			require.FailNow(t, "the watcher never finished the root")
		}
	}
	fac, err := f.app.BatchCLIStore()
	require.NoError(t, err)
	debt, err := fac.Store.BatchReactionDebtExists(t.Context(), owner)
	require.NoError(t, err)
	require.False(t, debt, "the server settles the notice debt of an API root")
	done := f.do(t, token, http.MethodGet, "/batches/"+accepted.RootID, nil).batch(t)
	require.True(t, done.Done, "the durable read still answers after the release")
	require.Equal(t, "completed", done.State)
}
