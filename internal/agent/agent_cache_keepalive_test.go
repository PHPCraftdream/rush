// Tests for the idle cache keep-alive (agent_cache_keepalive.go). Real
// timers are exercised by shrinking cacheKeepAliveInterval /
// cacheKeepAliveMaxExtensions — the package's established test-seam idiom
// (see streamStallRetryBaseBackoff in coordinator_run.go) — instead of
// sleeping through the real ~5-minute delay.
package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/message"
)

// keepAliveCountingModel is a mockModel (p623_hold_ctx_test.go) that counts
// Stream invocations, reports a caller-controlled provider name, and
// captures every fantasy.Call it was invoked with — so tests can both detect
// a fired replay, gate on explicit-cache providers, and assert the replay's
// request shape (tools/prompt) matches the triggering turn's.
type keepAliveCountingModel struct {
	mockModel
	provider string
	calls    atomic.Int64

	mu        sync.Mutex
	callsSeen []fantasy.Call
}

func (m *keepAliveCountingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.callsSeen = append(m.callsSeen, call)
	m.mu.Unlock()
	return m.mockModel.Stream(ctx, call)
}

func (m *keepAliveCountingModel) Provider() string { return m.provider }

// lastCall returns the most recently captured fantasy.Call, or the zero
// value if none has been seen yet.
func (m *keepAliveCountingModel) lastCall() fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.callsSeen) == 0 {
		return fantasy.Call{}
	}
	return m.callsSeen[len(m.callsSeen)-1]
}

// usageKeepAliveModel is a mockModel whose Stream reports a caller-controlled
// fantasy.Usage in its finish part, so tests can assert on the exact cost
// computed from a replay.
type usageKeepAliveModel struct {
	mockModel
	provider string
	usage    fantasy.Usage
	calls    atomic.Int64
}

func (m *usageKeepAliveModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	usage := m.usage
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        usage,
		})
	}, nil
}

func (m *usageKeepAliveModel) Provider() string { return m.provider }

// blockingKeepAliveModel's Stream blocks until ctx is Done, then returns
// ctx.Err() — used to test that an in-flight replay can actually be
// cancelled early instead of running out cacheKeepAliveCallTimeout.
type blockingKeepAliveModel struct {
	mockModel
	provider string
	calls    atomic.Int64
	started  chan struct{}
}

func (m *blockingKeepAliveModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	if m.started != nil {
		close(m.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingKeepAliveModel) Provider() string { return m.provider }

// blockingUsageKeepAliveModel signals started, then blocks until proceed is
// closed, then returns a SUCCESSFUL result carrying usage (unlike
// blockingKeepAliveModel, which returns ctx.Err() — this is for simulating
// "the replay eventually completes normally, but something else changed
// while it was in flight", not cancellation).
type blockingUsageKeepAliveModel struct {
	mockModel
	provider string
	usage    fantasy.Usage
	started  chan struct{}
	proceed  chan struct{}
	calls    atomic.Int64
}

func (m *blockingUsageKeepAliveModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	if m.started != nil {
		close(m.started)
	}
	<-m.proceed
	usage := m.usage
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        usage,
		})
	}, nil
}

func (m *blockingUsageKeepAliveModel) Provider() string { return m.provider }

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
	// Opt-in gate (task #761): the machinery is disabled by default in
	// production, so tests exercising it must explicitly enable it.
	t.Setenv("RUSH_CACHE_KEEPALIVE", "true")
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

	a.scheduleCacheKeepAlive("sess-1", model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

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

	a.scheduleCacheKeepAlive("sess-2", model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

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

	a.scheduleCacheKeepAlive("sess-3", model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

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

	a.scheduleCacheKeepAlive("sess-4", model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)
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

	a.scheduleCacheKeepAlive("sess-5", model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)
	entry, ok := a.cacheKeepAlive.Get("sess-5")
	require.True(t, ok)
	entry.extension = 2 // simulate having already extended twice

	// A fresh cache-writing turn re-schedules and must reset the counter.
	a.scheduleCacheKeepAlive("sess-5", model, []fantasy.Message{fantasy.NewUserMessage("hi again")}, nil, nil, 0)
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

	a.scheduleCacheKeepAlive("sess-6", model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

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

	a.scheduleCacheKeepAlive("sess-7", model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

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

	a.scheduleCacheKeepAlive("sess-8", model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)
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

// keepAliveToolNames returns the Name of every fantasy.FunctionTool in
// call.Tools, in order — the shape prepareTools produces from WithTools.
func keepAliveToolNames(call fantasy.Call) []string {
	names := make([]string, 0, len(call.Tools))
	for _, tool := range call.Tools {
		if ft, ok := tool.(fantasy.FunctionTool); ok {
			names = append(names, ft.Name)
		}
	}
	return names
}

// TestFireCacheKeepAlive_ReplayMatchesTriggeringTurnShape proves the replay
// reproduces the cacheable prefix: same tools (via WithTools) as the
// triggering turn, and no duplicated system message (stepMessages already
// carries it — see scheduleCacheKeepAlive's doc comment).
func TestFireCacheKeepAlive_ReplayMatchesTriggeringTurnShape(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 1)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	echoTool := fantasy.NewAgentTool("echo", "echoes input",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, nil
		})
	tools := []fantasy.AgentTool{echoTool}

	triggerMessages := []fantasy.Message{
		fantasy.NewSystemMessage("you are a test agent"),
		fantasy.NewUserMessage("hi"),
	}

	a.scheduleCacheKeepAlive("sess-shape", model, triggerMessages, tools, nil, 0)

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected the replay to fire")

	replayCall := lm.lastCall()

	// Tools: the replay must carry the SAME tools as the triggering turn —
	// this is the confirmed-missing piece the defect report identified.
	require.Equal(t, []string{"echo"}, keepAliveToolNames(replayCall),
		"replay must reproduce the triggering turn's tools via WithTools")

	// System prompt: stepMessages already contains the system message (fantasy's
	// createPrompt folds WithSystemPrompt into the messages PrepareStep sees,
	// and stepMessages is cloned from those prepared messages) — so the
	// replay's prompt must contain exactly ONE system message, not two.
	systemCount := 0
	for _, msg := range replayCall.Prompt {
		if msg.Role == fantasy.MessageRoleSystem {
			systemCount++
		}
	}
	require.Equal(t, 1, systemCount,
		"replay must not duplicate the system message already present in stepMessages")
}

// toolSnapshotKeepAliveModel captures every fantasy.Call it receives across
// both a real triggering turn and any later keep-alive replay (the same
// model instance drives both, matching production where scheduleCacheKeepAlive
// captures the triggering turn's own smartModel), and reports a caller-
// controlled Usage including cache-write tokens so the turn's own usage
// recording triggers scheduling.
type toolSnapshotKeepAliveModel struct {
	mockModel
	provider string
	usage    fantasy.Usage

	mu        sync.Mutex
	callsSeen []fantasy.Call
}

func (m *toolSnapshotKeepAliveModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.callsSeen = append(m.callsSeen, call)
	m.mu.Unlock()
	usage := m.usage
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        usage,
		})
	}, nil
}

func (m *toolSnapshotKeepAliveModel) Provider() string { return m.provider }

func (m *toolSnapshotKeepAliveModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.callsSeen)
}

func (m *toolSnapshotKeepAliveModel) call(i int) fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callsSeen[i]
}

// TestRunTurn_KeepAliveCapturesPostSetToolsSnapshot proves the fix for a real
// bug (2026-08-26 review): the keep-alive schedule call used to pass the
// turn-start `agentTools` snapshot, but PrepareStep re-reads a.tools fresh
// right before the actual request goes out (agent_turn.go's "use latest
// tools" comment on prepared.Tools) — if SetTools/an MCP update lands in
// between, the real request's tools diverge from what keep-alive captured,
// and the eventual replay no longer matches the cached prefix.
// runTurnToolsSnapshotSeam lands a SetTools call deterministically in that
// exact window, on runTurn's own goroutine — no real concurrency or
// scheduling luck needed to reproduce it.
func TestRunTurn_KeepAliveCapturesPostSetToolsSnapshot(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 1)
	t.Setenv("RUSH_CACHE_KEEPALIVE", "true")

	env := testEnv(t)
	toolA := fantasy.NewAgentTool("toolA", "turn-start tool, must NOT be what the replay carries",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, nil
		})
	toolB := fantasy.NewAgentTool("toolB", "set mid-turn via the seam, must be what the replay carries",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, nil
		})

	lm := &toolSnapshotKeepAliveModel{
		provider: anthropic.Name,
		usage:    fantasy.Usage{InputTokens: 100, OutputTokens: 10, CacheCreationTokens: 500},
	}
	model := testKeepAliveModel(Model{Model: lm})

	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:   model,
		FastModel:    model,
		SystemPrompt: "test",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		Tools:        []fantasy.AgentTool{toolA},
	})
	a := agentIface.(*sessionAgent)
	t.Cleanup(func() { a.CancelAll() })

	runTurnToolsSnapshotSeam = func() { a.SetTools([]fantasy.AgentTool{toolB}) }
	t.Cleanup(func() { runTurnToolsSnapshotSeam = nil })

	sess, err := env.sessions.Create(t.Context(), "keepalive-tools-race")
	require.NoError(t, err)
	// A non-default title + a pre-existing message keep needsTitle false, so
	// generateTitle's own concurrent model calls (which share this same mock
	// via SmartModel==FastModel) don't land in lm.callsSeen and get mistaken
	// for the keep-alive replay this test is trying to isolate.
	require.NoError(t, env.sessions.Rename(t.Context(), sess.ID, "already titled"))
	sess, err = env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "prior turn"}},
	})
	require.NoError(t, err)

	_, err = a.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hi"})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return lm.callCount() >= 2
	}, 2*time.Second, 5*time.Millisecond, "expected the keep-alive replay to fire a second call")

	replayCall := lm.call(lm.callCount() - 1)
	require.Equal(t, []string{"toolB"}, keepAliveToolNames(replayCall),
		"replay must carry the POST-SetTools tools PrepareStep actually used, not the turn-start snapshot")
}

// TestFireCacheKeepAlive_GenerationGuardSurvivesRace exercises the
// generation-token compare-and-act guard directly: a stale fireCacheKeepAlive
// callback (as if its timer had already fired) races a fresh
// scheduleCacheKeepAlive landing for the same session at nearly the same
// instant. Without the generation guard, the stale callback's unconditional
// Del/reschedule-Set could delete the new entry or overwrite it with stale
// messages. Run with -race -count=20+ to build confidence this isn't passing
// by luck of timing.
func TestFireCacheKeepAlive_GenerationGuardSurvivesRace(t *testing.T) {
	restoreKeepAliveVars(t, time.Hour, 3) // long enough that no real timer fires during the test
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	oldMessages := []fantasy.Message{fantasy.NewUserMessage("old turn")}
	newMessages := []fantasy.Message{fantasy.NewUserMessage("new turn")}

	a.scheduleCacheKeepAlive("sess-race", model, oldMessages, nil, nil, 0)
	oldEntry, ok := a.cacheKeepAlive.Get("sess-race")
	require.True(t, ok)
	oldGen := oldEntry.generation

	// Race: a stale fire for the OLD generation (simulating a timer callback
	// that already started running before being superseded) against a fresh
	// schedule for the SAME session with NEW messages.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		a.fireCacheKeepAlive("sess-race", model, oldMessages, nil, nil, 0, oldGen, 0)
	}()
	go func() {
		defer wg.Done()
		a.scheduleCacheKeepAlive("sess-race", model, newMessages, nil, nil, 0)
	}()
	wg.Wait()

	// Whichever entry survives, it must never be the stale old-generation
	// one: either the new schedule's entry is present, or (if the stale fire
	// happened to run first, delete it, and the new schedule then landed) the
	// new schedule's entry is present — the old generation must never be the
	// one left standing, and the map must never end up empty when the new
	// schedule ran (scheduleCacheKeepAlive always leaves an entry behind).
	entry, ok := a.cacheKeepAlive.Get("sess-race")
	require.True(t, ok, "the new schedule must always leave an entry behind")
	require.NotEqual(t, oldGen, entry.generation,
		"surviving entry must never be the stale pre-race generation")

	// The Stream call, if the stale fire's admit+replay actually ran, must
	// have used the OLD messages (that's fine and expected — it's a real
	// in-flight replay for the turn that scheduled it). What must NOT happen
	// is the stored entry regressing to old messages/generation afterward.
	time.Sleep(50 * time.Millisecond) // let any in-flight stale replay finish
	entry, ok = a.cacheKeepAlive.Get("sess-race")
	require.True(t, ok, "entry must still be present after any stale in-flight replay settles")
	require.NotEqual(t, oldGen, entry.generation,
		"stale callback's tail must not clobber the new entry after settling")
}
