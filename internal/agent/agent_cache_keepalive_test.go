// Tests for the idle cache keep-alive (agent_cache_keepalive.go). Real
// timers are exercised by shrinking cacheKeepAliveInterval /
// cacheKeepAliveMaxExtensions — the package's established test-seam idiom
// (see streamStallRetryBaseBackoff in coordinator_run.go) — instead of
// sleeping through the real ~5-minute delay.
package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/stretchr/testify/require"
)

// keepAliveCountingModel is a mockModel (p623_hold_ctx_test.go) that counts
// Stream invocations and reports a caller-controlled provider name, so tests
// can both detect a fired replay and gate on explicit-cache providers.
type keepAliveCountingModel struct {
	mockModel
	provider string
	calls    atomic.Int64
}

func (m *keepAliveCountingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	return m.mockModel.Stream(ctx, call)
}

func (m *keepAliveCountingModel) Provider() string { return m.provider }

// erroringKeepAliveModel always fails Stream, counting attempts — used to
// prove a failed replay does not reschedule.
type erroringKeepAliveModel struct {
	calls atomic.Int64
}

func (*erroringKeepAliveModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("boom")
}

func (m *erroringKeepAliveModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	return nil, errors.New("boom")
}

func (*erroringKeepAliveModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (*erroringKeepAliveModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (*erroringKeepAliveModel) Provider() string { return anthropic.Name }
func (*erroringKeepAliveModel) Model() string    { return "erroring-keepalive" }

func testKeepAliveModel(model Model) Model {
	model.CatwalkCfg = catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}
	return model
}

func newKeepAliveAgent(t *testing.T) (*sessionAgent, fakeEnv) {
	t.Helper()
	env := testEnv(t)
	m := &mockModel{}
	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:   Model{Model: m, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:    Model{Model: m, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt: "test",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})
	return agentIface.(*sessionAgent), env
}

// restoreKeepAliveVars shrinks the package-level timer vars for a test and
// restores them on cleanup, so tests never leak a mutated global.
func restoreKeepAliveVars(t *testing.T, interval time.Duration, maxExt int) {
	t.Helper()
	origInterval := cacheKeepAliveInterval
	origMaxExt := cacheKeepAliveMaxExtensions
	cacheKeepAliveInterval = interval
	cacheKeepAliveMaxExtensions = maxExt
	t.Cleanup(func() {
		cacheKeepAliveInterval = origInterval
		cacheKeepAliveMaxExtensions = origMaxExt
	})
}

func TestScheduleCacheKeepAlive_FiresReplayAfterInterval(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	// CancelAll stops any still-pending re-armed timer before the test
	// returns and restoreKeepAliveVars' cleanup mutates the shared vars —
	// otherwise a live background goroutine could still be reading them.
	t.Cleanup(func() { a.CancelAll() })

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-1", model, []fantasy.Message{fantasy.NewUserMessage("hi")})

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected a replay Stream call after the shrunk interval")
}

func TestScheduleCacheKeepAlive_SkipsNonExplicitCacheProvider(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &keepAliveCountingModel{provider: "openai"}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-2", model, []fantasy.Message{fantasy.NewUserMessage("hi")})

	_, scheduled := a.cacheKeepAlive.Get("sess-2")
	require.False(t, scheduled, "non-explicit-cache provider must not get a scheduled timer")

	time.Sleep(100 * time.Millisecond)
	require.Zero(t, lm.calls.Load(), "no replay should ever fire for a non-explicit-cache provider")
}

func TestScheduleCacheKeepAlive_SkipsWhenCacheDisabled(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	t.Setenv("RUSH_DISABLE_ANTHROPIC_CACHE", "true")
	a, _ := newKeepAliveAgent(t)

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-3", model, []fantasy.Message{fantasy.NewUserMessage("hi")})

	_, scheduled := a.cacheKeepAlive.Get("sess-3")
	require.False(t, scheduled, "must not schedule when RUSH_DISABLE_ANTHROPIC_CACHE is set")

	time.Sleep(100 * time.Millisecond)
	require.Zero(t, lm.calls.Load())
}

func TestCancelCacheKeepAlive_StopsPendingTimer(t *testing.T) {
	restoreKeepAliveVars(t, 30*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-4", model, []fantasy.Message{fantasy.NewUserMessage("hi")})
	_, scheduled := a.cacheKeepAlive.Get("sess-4")
	require.True(t, scheduled)

	a.cancelCacheKeepAlive("sess-4")
	_, stillScheduled := a.cacheKeepAlive.Get("sess-4")
	require.False(t, stillScheduled)

	// Advance well past the shrunk interval and confirm no replay fired.
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, lm.calls.Load(), "cancelled keep-alive must never fire")
}

func TestScheduleCacheKeepAlive_NewWriteResetsExtensionCounter(t *testing.T) {
	restoreKeepAliveVars(t, 200*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-5", model, []fantasy.Message{fantasy.NewUserMessage("hi")})
	entry, ok := a.cacheKeepAlive.Get("sess-5")
	require.True(t, ok)
	entry.extension = 2 // simulate having already extended twice

	// A fresh cache-writing turn re-schedules and must reset the counter.
	a.scheduleCacheKeepAlive("sess-5", model, []fantasy.Message{fantasy.NewUserMessage("hi again")})
	entry, ok = a.cacheKeepAlive.Get("sess-5")
	require.True(t, ok)
	require.Equal(t, 0, entry.extension, "a fresh cache write must reset the extension counter to 0")
}

func TestFireCacheKeepAlive_StopsAtExtensionCap(t *testing.T) {
	restoreKeepAliveVars(t, 15*time.Millisecond, 2)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-6", model, []fantasy.Message{fantasy.NewUserMessage("hi")})

	// cacheKeepAliveMaxExtensions=2: the initial fire (extension 0) succeeds
	// and reschedules once (extension 1), then that fire succeeds too but
	// extension+1 (2) >= max (2), so it stops. Expect exactly 2 calls total.
	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 2
	}, 2*time.Second, 5*time.Millisecond)

	// Give any incorrect further reschedule a chance to fire, then confirm
	// it didn't.
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 2, lm.calls.Load(), "must stop firing once the extension cap is reached")

	_, stillScheduled := a.cacheKeepAlive.Get("sess-6")
	require.False(t, stillScheduled)
}

func TestFireCacheKeepAlive_FailureDoesNotReschedule(t *testing.T) {
	restoreKeepAliveVars(t, 15*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &erroringKeepAliveModel{}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-7", model, []fantasy.Message{fantasy.NewUserMessage("hi")})

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond)

	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 1, lm.calls.Load(), "a failed replay must not reschedule")

	_, stillScheduled := a.cacheKeepAlive.Get("sess-7")
	require.False(t, stillScheduled)
}

func TestCancelAll_StopsPendingKeepAliveTimers(t *testing.T) {
	restoreKeepAliveVars(t, 30*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive("sess-8", model, []fantasy.Message{fantasy.NewUserMessage("hi")})
	_, scheduled := a.cacheKeepAlive.Get("sess-8")
	require.True(t, scheduled)

	stillBusy := a.CancelAll()
	require.False(t, stillBusy)

	require.Zero(t, a.cacheKeepAlive.Len(), "CancelAll must sweep all pending keep-alive timers")

	// Even past the shrunk interval, no replay should fire: the timer was
	// stopped AND tryAdmitRunWg refuses post-shutdown admission as a backstop.
	time.Sleep(150 * time.Millisecond)
	require.Zero(t, lm.calls.Load())
}
