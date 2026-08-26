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

// TestFireCacheKeepAlive_CancelDuringMaxCostCheckIsHonored proves the fix for
// a real race (2026-08-26 review): before the fix, fireCacheKeepAlive deleted
// the pending entry and released cacheKeepAliveMu, then ran the maxCost
// check + tryAdmitRunWg + ctx construction, and only THEN registered the
// in-flight cancel — a cancelCacheKeepAlive call landing in that gap found
// neither the pending entry (already deleted) nor the in-flight cancel (not
// yet registered) and was silently lost, letting the stale replay run to
// completion unopposed and potentially rearm afterward. cacheKeepAliveFireSeam
// lands the cancel deterministically inside that exact span (now closed: the
// in-flight cancel is registered atomically with the pending-entry removal,
// before the seam fires) and asserts the cancel is actually honored.
func TestFireCacheKeepAlive_CancelDuringMaxCostCheckIsHonored(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &blockingKeepAliveModel{provider: anthropic.Name, started: make(chan struct{})}
	model := testKeepAliveModel(Model{Model: lm})

	seamHit := make(chan struct{})
	proceed := make(chan struct{})
	cacheKeepAliveFireSeam = func() { close(seamHit); <-proceed }
	t.Cleanup(func() { cacheKeepAliveFireSeam = nil })

	sessionID := "sess-cancel-race"
	a.scheduleCacheKeepAlive(sessionID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	select {
	case <-seamHit:
	case <-time.After(2 * time.Second):
		t.Fatal("fire never reached the seam")
	}

	// Land the cancel while the fire is PAUSED at the seam — the exact span
	// that used to be an unregistered gap before this fix — then let it
	// proceed. Blocking (not fire-and-forget) is required: without it, the
	// production goroutine's own scheduling could complete its registration
	// before this goroutine is even scheduled, closing the window before the
	// cancel has any chance to land inside it.
	a.cancelCacheKeepAlive(sessionID)
	close(proceed)

	select {
	case <-lm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("replay never reached Stream")
	}

	// The in-flight entry is cleared only by fireCacheKeepAlive's deferred
	// cleanup, which runs once Stream has actually RETURNED — not merely been
	// entered. This must happen promptly (the ctx was already cancelled)
	// rather than after the full 30s cacheKeepAliveCallTimeout: a test that
	// only checked "Stream was entered" (lm.calls>=1, true the instant Stream
	// starts regardless of whether its ctx is ever cancelled) would pass even
	// when the call is still hanging in the background well past this test's
	// own return — this check instead demands the round trip completed.
	require.Eventually(t, func() bool {
		_, stillInFlight := a.cacheKeepAliveInFlight.Get(sessionID)
		return !stillInFlight
	}, 2*time.Second, 5*time.Millisecond,
		"cancelled replay must return promptly, not run out the 30s call timeout")

	// Give any incorrect rearm a chance to fire, then confirm none did.
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, 1, lm.calls.Load(), "a cancelled fire must not rearm")
	_, stillScheduled := a.cacheKeepAlive.Get(sessionID)
	require.False(t, stillScheduled, "a cancelled fire must not leave a pending entry behind")
}

// TestFireCacheKeepAlive_RecordsReplayCost proves a successful replay's usage
// is billed to the session via IncrementCost, using the same formula
// agent_title.go's generateTitle uses (CostPer1M* x usage fields).
func TestFireCacheKeepAlive_RecordsReplayCost(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 1)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	sess, err := env.sessions.Create(t.Context(), "keepalive cost test")
	require.NoError(t, err)

	lm := &usageKeepAliveModel{
		provider: anthropic.Name,
		usage:    fantasy.Usage{InputTokens: 1000, OutputTokens: 1, CacheReadTokens: 2000, CacheCreationTokens: 500},
	}
	model := testKeepAliveModel(Model{Model: lm})
	model.CatwalkCfg.CostPer1MIn = 3.0
	model.CatwalkCfg.CostPer1MOut = 15.0
	model.CatwalkCfg.CostPer1MInCached = 6.0
	model.CatwalkCfg.CostPer1MOutCached = 0.30

	expectedCost := model.CatwalkCfg.CostPer1MInCached/1e6*float64(lm.usage.CacheCreationTokens) +
		model.CatwalkCfg.CostPer1MOutCached/1e6*float64(lm.usage.CacheReadTokens) +
		model.CatwalkCfg.CostPer1MIn/1e6*float64(lm.usage.InputTokens) +
		model.CatwalkCfg.CostPer1MOut/1e6*float64(lm.usage.OutputTokens)
	require.Greater(t, expectedCost, 0.0, "test setup sanity: expected cost must be nonzero")

	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	require.Eventually(t, func() bool {
		return lm.calls.Load() >= 1
	}, 2*time.Second, 5*time.Millisecond, "expected the replay to fire")

	require.Eventually(t, func() bool {
		updated, err := env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		return updated.Cost > 0
	}, 2*time.Second, 5*time.Millisecond, "expected replay cost to be recorded on the session")

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.InDelta(t, expectedCost, updated.Cost, 1e-9, "recorded cost must match the usage-derived formula")

	// PromptTokens/CompletionTokens are a session-level SNAPSHOT the main
	// turn owns (see recordCacheKeepAliveCost's doc) — a replay must never
	// touch them.
	require.Zero(t, updated.PromptTokens, "replay must not touch the PromptTokens snapshot")
	require.Zero(t, updated.CompletionTokens, "replay must not touch the CompletionTokens snapshot")
}

// TestFireCacheKeepAlive_SkipsWhenSessionAtMaxCost proves a session already
// at or over its MaxCost cap gets no replay call at all, and is not
// rescheduled — mirroring agent_turn.go's own max-cost abort being terminal.
func TestFireCacheKeepAlive_SkipsWhenSessionAtMaxCost(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	sess, err := env.sessions.Create(t.Context(), "keepalive max-cost test")
	require.NoError(t, err)
	_, err = env.sessions.IncrementCost(t.Context(), sess.ID, 5.0)
	require.NoError(t, err)

	lm := &keepAliveCountingModel{provider: anthropic.Name}
	model := testKeepAliveModel(Model{Model: lm})

	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 5.0)

	// Give the shrunk interval plenty of time to fire and settle.
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, lm.calls.Load(), "session at max cost must never get a replay Stream call")

	_, stillScheduled := a.cacheKeepAlive.Get(sess.ID)
	require.False(t, stillScheduled, "a cost-capped skip must not reschedule")

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.InDelta(t, 5.0, updated.Cost, 1e-9, "cost must remain unchanged, no replay spend")
}

// TestFireCacheKeepAlive_SkipsChargeWhenSessionCrossesMaxCostMidFlight proves
// the fix for a real TOCTOU gap (2026-08-26 review): the up-front maxCost
// check only bounds the wait BEFORE the replay call, which can itself take
// up to cacheKeepAliveCallTimeout (30s). A concurrent real turn's own spend
// landing during that window must still be caught before the replay's own
// cost is charged, and must also block the next rearm — a session already
// over its cap must not keep spending on further extensions.
func TestFireCacheKeepAlive_SkipsChargeWhenSessionCrossesMaxCostMidFlight(t *testing.T) {
	restoreKeepAliveVars(t, 20*time.Millisecond, 3)
	a, env := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	sess, err := env.sessions.Create(t.Context(), "keepalive max-cost mid-flight test")
	require.NoError(t, err)

	lm := &blockingUsageKeepAliveModel{
		provider: anthropic.Name,
		usage:    fantasy.Usage{InputTokens: 1000, OutputTokens: 1},
		started:  make(chan struct{}),
		proceed:  make(chan struct{}),
	}
	model := testKeepAliveModel(Model{Model: lm})
	model.CatwalkCfg.CostPer1MIn = 1.0

	const maxCost = 5.0
	a.scheduleCacheKeepAlive(sess.ID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, maxCost)

	select {
	case <-lm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("replay never reached Stream")
	}

	// The replay is now blocked in Stream, past the up-front maxCost check
	// (which passed: the session started at 0 cost). Simulate a concurrent
	// real turn pushing the session over the cap WHILE the replay is in
	// flight.
	_, err = env.sessions.IncrementCost(t.Context(), sess.ID, maxCost)
	require.NoError(t, err)

	close(lm.proceed)

	// The replay completes successfully (Stream returned real usage, no
	// error) — but its own cost must NOT be layered on top of the
	// already-over-cap session, and it must not rearm.
	require.Eventually(t, func() bool {
		_, stillInFlight := a.cacheKeepAliveInFlight.Get(sess.ID)
		return !stillInFlight
	}, 2*time.Second, 5*time.Millisecond, "replay must complete promptly")

	time.Sleep(100 * time.Millisecond)
	_, stillScheduled := a.cacheKeepAlive.Get(sess.ID)
	require.False(t, stillScheduled, "a mid-flight max-cost crossing must not rearm")

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.InDelta(t, maxCost, updated.Cost, 1e-9,
		"the replay's own cost must not be charged on top of an already-over-cap session")
}

// TestCancelCacheKeepAlive_CancelsInFlightReplay proves an in-flight replay
// (already past admission, blocked inside Stream) is cut off promptly by
// cancelCacheKeepAlive instead of running out the full 30s
// cacheKeepAliveCallTimeout. Run with -race -count=10+.
func TestCancelCacheKeepAlive_CancelsInFlightReplay(t *testing.T) {
	restoreKeepAliveVars(t, 15*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)
	t.Cleanup(func() { a.CancelAll() })

	lm := &blockingKeepAliveModel{provider: anthropic.Name, started: make(chan struct{})}
	model := testKeepAliveModel(Model{Model: lm})

	sessionID := "sess-inflight-cancel"
	a.scheduleCacheKeepAlive(sessionID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	select {
	case <-lm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the replay's Stream call to start")
	}

	// The replay is now blocked inside Stream, in flight. cancelCacheKeepAlive
	// must cut it off promptly rather than waiting out the 30s call timeout.
	start := time.Now()
	a.cancelCacheKeepAlive(sessionID)

	require.Eventually(t, func() bool {
		return lm.calls.Load() == 1
	}, 5*time.Second, 5*time.Millisecond)
	elapsed := time.Since(start)
	require.Less(t, elapsed, 5*time.Second,
		"in-flight replay must be cancelled promptly, not run out cacheKeepAliveCallTimeout (30s)")

	_, stillInFlight := a.cacheKeepAliveInFlight.Get(sessionID)
	require.False(t, stillInFlight, "in-flight entry must be cleared after the call returns")
}

// TestCancelAll_CancelsInFlightReplay is TestCancelCacheKeepAlive_CancelsInFlightReplay's
// CancelAll counterpart: CancelAll's own shutdown sweep must also reach and
// cancel a replay call already in flight, within its existing grace period.
func TestCancelAll_CancelsInFlightReplay(t *testing.T) {
	restoreKeepAliveVars(t, 15*time.Millisecond, 3)
	a, _ := newKeepAliveAgent(t)

	lm := &blockingKeepAliveModel{provider: anthropic.Name, started: make(chan struct{})}
	model := testKeepAliveModel(Model{Model: lm})

	sessionID := "sess-inflight-cancelall"
	a.scheduleCacheKeepAlive(sessionID, model, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)

	select {
	case <-lm.started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the replay's Stream call to start")
	}

	start := time.Now()
	stillBusy := a.CancelAll()
	elapsed := time.Since(start)

	require.False(t, stillBusy, "CancelAll must observe the in-flight replay exit within its grace period")
	require.Equal(t, int64(1), lm.calls.Load())
	require.Less(t, elapsed, 5*time.Second,
		"CancelAll must cancel the in-flight replay promptly rather than waiting out its own grace period")
}
