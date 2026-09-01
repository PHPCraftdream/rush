package agent

// R3-1 regression tests: per-call tool pinning.
//
// resolveSessionModels/applyModelOverrides must build the coder toolset from
// the CALL's context (CallOptions.DisableSubAgents / CallOptions.ModelRole)
// and pin it onto the SessionAgentCall, never publishing it to the shared
// currentAgent — the old per-run publisher's SetTools overwrote the ONE
// shared toolset every in-flight turn re-read at every step.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newToolPinningCoordinator builds a coordinator wired like
// newWorkerToolTestCoordinator (all services buildTools needs) plus the coder
// prompt resolveSessionModels needs to build the system prompt.
func newToolPinningCoordinator(t *testing.T, env fakeEnv, includeWorker bool) *coordinator {
	t.Helper()
	isolateAllGlobalConfigPaths(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)

	registerProvider := func(providerID, modelID string) config.SelectedModel {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID:   providerID,
			Type: openai.Name,
			Models: []catwalk.Model{
				{ID: modelID},
			},
		})
		return config.SelectedModel{Provider: providerID, Model: modelID}
	}
	cfg.Config().Models[config.SelectedModelTypeSmart] = registerProvider("smart-provider", "smart-model")
	cfg.Config().Models[config.SelectedModelTypeFast] = registerProvider("fast-provider", "fast-model")
	if includeWorker {
		cfg.Config().Models[config.SelectedModelTypeWorker] = registerProvider("worker-provider", "worker-model")
	}
	cfg.SetupAgents()

	p, err := coderPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)

	return &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		prompt:      p,
		modelCache:  csync.NewMap[string, cachedModelPair](),
	}
}

// toolPublishRecorder records every Set*/publish call landing on the shared
// agent, so tests can assert a code path NEVER publishes per-call state.
type toolPublishRecorder struct {
	*mockSessionAgent
	mu         sync.Mutex
	toolNames  [][]string // one entry per SetTools call
	modelCalls int
	prefixes   []string
	prompts    []string
}

func (r *toolPublishRecorder) SetTools(tools []fantasy.AgentTool) {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Info().Name)
	}
	r.mu.Lock()
	r.toolNames = append(r.toolNames, names)
	r.mu.Unlock()
}

func (r *toolPublishRecorder) SetModels(smart, fast Model) {
	r.mu.Lock()
	r.modelCalls++
	r.mu.Unlock()
}

func (r *toolPublishRecorder) SetSystemPromptPrefix(p string) {
	r.mu.Lock()
	r.prefixes = append(r.prefixes, p)
	r.mu.Unlock()
}

func (r *toolPublishRecorder) SetSystemPrompt(p string) {
	r.mu.Lock()
	r.prompts = append(r.prompts, p)
	r.mu.Unlock()
}

func (r *toolPublishRecorder) snapshot() ([][]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.toolNames, r.modelCalls
}

// pinnedToolNames extracts the tool names from a resolvedOverrides snapshot.
func pinnedToolNames(tools []fantasy.AgentTool) []string {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Info().Name)
	}
	return names
}

// TestResolveSessionModels_PinsPerCallDisableSubAgents proves the per-call
// DisableSubAgents filter decides the PINNED toolset, is isolated between
// calls with opposite policies, and never reaches the shared agent.
func TestResolveSessionModels_PinsPerCallDisableSubAgents(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false /* no worker configured */)
	rec := &toolPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = rec

	sess, err := coord.sessions.Create(t.Context(), "pin-subagents")
	require.NoError(t, err)

	ctxBanned := WithCallOptions(t.Context(), &CallOptions{DisableSubAgents: true})
	ctxAllowed := WithCallOptions(t.Context(), &CallOptions{DisableSubAgents: false})

	pinnedBanned, err := coord.resolveSessionModels(ctxBanned, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, pinnedBanned.tools, "the banned call must get a pinned toolset")

	namesBanned := pinnedToolNames(pinnedBanned.tools)
	assert.NotContains(t, namesBanned, AgentToolName, "DisableSubAgents must strip the agent tool from the pinned set")
	assert.NotContains(t, namesBanned, "agentic_fetch", "DisableSubAgents must strip agentic_fetch from the pinned set")
	assert.Contains(t, namesBanned, "bash", "unrelated tools must survive the per-call filter")

	pinnedAllowed, err := coord.resolveSessionModels(ctxAllowed, sess.ID)
	require.NoError(t, err)
	namesAllowed := pinnedToolNames(pinnedAllowed.tools)
	assert.Contains(t, namesAllowed, AgentToolName, "the allowed call keeps the agent tool")
	assert.Contains(t, namesAllowed, "agentic_fetch", "the allowed call keeps agentic_fetch")

	// Isolation: resolving for the banned policy again — AFTER the allowed
	// resolve — must still see the banned slice; no bleed between policies.
	pinnedBanned2, err := coord.resolveSessionModels(ctxBanned, sess.ID)
	require.NoError(t, err)
	namesBanned2 := pinnedToolNames(pinnedBanned2.tools)
	assert.NotContains(t, namesBanned2, AgentToolName, "resolving for another policy must not bleed into the banned call")
	assert.NotContains(t, namesBanned2, "agentic_fetch")

	// THE KEY ASSERTION: resolveSessionModels must never publish to the
	// shared agent (R3-1).
	names, modelCalls := rec.snapshot()
	assert.Empty(t, names, "resolveSessionModels must never SetTools the shared currentAgent")
	assert.Zero(t, modelCalls, "resolveSessionModels must never SetModels the shared currentAgent")
}

// TestResolveSessionModels_RolePolicyAgreesAcrossPromptModelAndTools catches
// the buildToolsAgentConfig bug: pre-fix the tool-layering gate read the
// SHARED activeModelRole while prompt/model selection read the per-call role,
// so an explicit --role call's tools could disagree with its own prompt.
func TestResolveSessionModels_RolePolicyAgreesAcrossPromptModelAndTools(t *testing.T) {
	t.Run("shared role unset + per-call fast", func(t *testing.T) {
		env := testEnv(t)
		coord := newToolPinningCoordinator(t, env, true /* worker configured */)
		// Shared activeModelRole deliberately left at zero "".

		sess, err := coord.sessions.Create(t.Context(), "role-fast")
		require.NoError(t, err)
		ctxFast := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeFast})

		pinned, err := coord.resolveSessionModels(ctxFast, sess.ID)
		require.NoError(t, err)
		names := pinnedToolNames(pinned.tools)
		assert.Contains(t, names, "edit", "per-call fast must NOT trigger the orchestrator strip")
		assert.Contains(t, names, "multiedit")
		assert.Contains(t, names, "write")
		assert.NotContains(t, pinned.systemPrompt, "Orchestrator mode",
			"per-call fast prompt is not an orchestrator prompt; tools must agree")
	})

	t.Run("shared role fast + per-call smart", func(t *testing.T) {
		env := testEnv(t)
		coord := newToolPinningCoordinator(t, env, true /* worker configured */)
		// Poison the shared field: pre-fix the tool gate read this instead of
		// the per-call role and KEPT the edit tools for this smart call.
		coord.SetActiveModelRole(config.SelectedModelTypeFast)

		sess, err := coord.sessions.Create(t.Context(), "role-smart")
		require.NoError(t, err)
		ctxSmart := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeSmart})

		pinned, err := coord.resolveSessionModels(ctxSmart, sess.ID)
		require.NoError(t, err)
		names := pinnedToolNames(pinned.tools)
		assert.NotContains(t, names, "edit", "per-call smart + worker configured => orchestrator strip")
		assert.NotContains(t, names, "multiedit")
		assert.NotContains(t, names, "write")
		assert.Contains(t, names, AgentToolName, "the orchestrator keeps the agent tool")
		assert.Contains(t, pinned.systemPrompt, "Orchestrator mode",
			"per-call smart prompt IS an orchestrator prompt; tools must agree")
	})

	t.Run("sub-agent toolset and model agree with the per-call role", func(t *testing.T) {
		env := testEnv(t)
		coord := newToolPinningCoordinator(t, env, true /* worker configured */)
		taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
		require.True(t, ok, "task agent must be configured")
		cfgSnap, _ := coord.cfg.Snapshot()

		ctxSmart := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeSmart})
		ctxFast := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeFast})

		subAgentToolsSmart, err := coord.buildTools(ctxSmart, cfgSnap, taskCfg, true)
		require.NoError(t, err)
		smartNames := pinnedToolNames(subAgentToolsSmart)
		assert.Contains(t, smartNames, "edit", "worker toolset for a per-call smart run")
		assert.Contains(t, smartNames, "bash")

		subAgentToolsFast, err := coord.buildTools(ctxFast, cfgSnap, taskCfg, true)
		require.NoError(t, err)
		fastNames := pinnedToolNames(subAgentToolsFast)
		assert.NotContains(t, fastNames, "edit", "read-only sub-agent toolset for a per-call fast run")
		assert.NotContains(t, fastNames, "bash")

		smartSmart, _, err := coord.buildAgentModelsFromCfg(ctxSmart, cfgSnap, true)
		require.NoError(t, err)
		assert.Equal(t, "worker-provider", smartSmart.ModelCfg.Provider,
			"sub-agent prefers the Worker slot for a per-call smart run")
		smartFast, _, err := coord.buildAgentModelsFromCfg(ctxFast, cfgSnap, true)
		require.NoError(t, err)
		assert.Equal(t, "smart-provider", smartFast.ModelCfg.Provider,
			"sub-agent falls back to Smart for a per-call fast run")
	})
}

// TestUpdateModels_NeverPublishesPerCallToolFilter pins the global-publish
// contract: UpdateModels strips CallOptions from its context, so a single
// run's DisableSubAgents/ModelRole must NOT decide what every other in-flight
// turn sees.
func TestUpdateModels_NeverPublishesPerCallToolFilter(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	rec := &toolPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = rec

	sess, err := coord.sessions.Create(t.Context(), "update-models")
	require.NoError(t, err)
	_ = sess

	ctx := WithCallOptions(t.Context(), &CallOptions{
		DisableSubAgents: true,
		ModelRole:        config.SelectedModelTypeFast,
	})
	require.NoError(t, coord.UpdateModels(ctx))

	names, _ := rec.snapshot()
	require.Len(t, names, 1, "UpdateModels must publish exactly one global toolset")
	assert.Contains(t, names[0], AgentToolName, "the per-call DisableSubAgents filter must NOT be applied to the global publish")
	assert.Contains(t, names[0], "agentic_fetch", "the per-call filter must NOT strip agentic_fetch from the global publish")
}

// toolPinningProbeModel is a provider-gated mock model: each turn's FIRST
// request parks on `release` (after reporting its marker on step1), so the
// test can poison the shared toolset BETWEEN steps deterministically.
type toolPinningProbeModel struct {
	mockModel
	mu             sync.Mutex
	seen           []fantasy.Call
	perMarkerCount map[string]int
	step1          chan string   // buffered 4: marker of each turn's FIRST request
	release        chan struct{} // closed to unblock first requests
}

func (m *toolPinningProbeModel) markerOf(call fantasy.Call) string {
	s := fmt.Sprintf("%+v", call.Prompt)
	switch {
	case strings.Contains(s, "PINNED_MARKER_A"):
		return "A"
	case strings.Contains(s, "PINNED_MARKER_B"):
		return "B"
	default:
		return "?"
	}
}

func (m *toolPinningProbeModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.seen = append(m.seen, call)
	marker := m.markerOf(call)
	m.perMarkerCount[marker]++
	first := m.perMarkerCount[marker] == 1
	m.mu.Unlock()

	if marker == "?" {
		return nil, fmt.Errorf("unexpected request")
	}

	if first {
		select {
		case m.step1 <- marker:
		default:
		}
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// Step-1 response: a tool call on the tool BOTH turns have, so a
		// second step happens (and must still see the turn's own pinned
		// tools, not whatever the shared slice holds by then).
		toolName := "toolShared"
		return func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "call-1-" + marker,
				ToolCallName:  toolName,
				ToolCallInput: "{}",
			}) {
				return
			}
			yield(fantasy.StreamPart{
				Type:         fantasy.StreamPartTypeFinish,
				FinishReason: fantasy.FinishReasonToolCalls,
				Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
			})
		}, nil
	}

	// Step >= 2: end the turn.
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}, nil
}

func (m *toolPinningProbeModel) snapshot() []fantasy.Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]fantasy.Call, len(m.seen))
	copy(out, m.seen)
	return out
}

// TestRunTurn_PinnedToolsAreStableAcrossStepsAndSharedSetTools proves the
// R3-1 end-to-end contract: two concurrent turns on ONE sessionAgent with
// OPPOSITE pinned tool slices keep their own tools at EVERY step, even while
// the shared toolset is poisoned (SetTools) between their steps — exactly the
// window the old per-run publisher raced.
func TestRunTurn_PinnedToolsAreStableAcrossStepsAndSharedSetTools(t *testing.T) {
	env := testEnv(t)

	newTool := func(name string) fantasy.AgentTool {
		return fantasy.NewAgentTool(name, "desc of "+name,
			func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.ToolResponse{}, nil
			})
	}
	toolShared := newTool("toolShared")
	toolA1 := newTool("toolA1")
	toolB1 := newTool("toolB1")
	bogus1 := newTool("bogus1")
	bogus2 := newTool("bogus2")

	pinnedA := []fantasy.AgentTool{toolShared, toolA1}
	pinnedB := []fantasy.AgentTool{toolShared, toolB1}

	model := &toolPinningProbeModel{
		step1:          make(chan string, 4),
		release:        make(chan struct{}),
		perMarkerCount: map[string]int{},
	}
	modelRef := Model{Model: model, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}}

	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:   modelRef,
		FastModel:    modelRef,
		SystemPrompt: "test",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
		Tools:        []fantasy.AgentTool{toolShared}, // the SHARED initial toolset
	})
	a := agentIface.(*sessionAgent)
	t.Cleanup(func() { a.CancelAll() })

	seedSession := func(title string) string {
		sess, err := env.sessions.Create(t.Context(), title)
		require.NoError(t, err)
		require.NoError(t, env.sessions.Rename(t.Context(), sess.ID, title))
		_, err = env.sessions.Get(t.Context(), sess.ID)
		require.NoError(t, err)
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
		})
		require.NoError(t, err)
		return sess.ID
	}
	sessA := seedSession("titled-a")
	sessB := seedSession("titled-b")

	callA := SessionAgentCall{SessionID: sessA, Prompt: "turn one PINNED_MARKER_A", Tools: pinnedA}
	callB := SessionAgentCall{SessionID: sessB, Prompt: "turn one PINNED_MARKER_B", Tools: pinnedB}

	waitStep1 := func(want string) {
		t.Helper()
		select {
		case got := <-model.step1:
			require.Equal(t, want, got, "turn %s's first step must be the one parked on", want)
		case <-time.After(60 * time.Second):
			t.Fatalf("timed out waiting for turn %s's first step", want)
		}
	}

	// Launch A, wait for its first step to be parked.
	resA := make(chan error, 1)
	go func() {
		_, err := a.Run(t.Context(), callA)
		resA <- err
	}()
	waitStep1("A")

	// POISON: simulate the old per-call publisher landing between A's steps.
	a.SetTools([]fantasy.AgentTool{bogus1})

	// Launch B, wait for its first step to be parked.
	resB := make(chan error, 1)
	go func() {
		_, err := a.Run(t.Context(), callB)
		resB <- err
	}()
	waitStep1("B")

	// POISON again — both turns are now mid-flight with a corrupted shared
	// toolset.
	a.SetTools([]fantasy.AgentTool{bogus2})

	close(model.release) // both first steps proceed

	select {
	case err := <-resA:
		require.NoError(t, err, "turn A must complete")
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for turn A to complete")
	}
	select {
	case err := <-resB:
		require.NoError(t, err, "turn B must complete")
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for turn B to complete")
	}

	calls := model.snapshot()
	require.Len(t, calls, 4, "2 steps x 2 turns")

	markerCounts := map[string]int{}
	for _, call := range calls {
		marker := model.markerOf(call)
		markerCounts[marker]++
		names := keepAliveToolNames(call)
		switch marker {
		case "A":
			assert.Contains(t, names, "toolShared", "turn A step must keep its pinned shared tool")
			assert.Contains(t, names, "toolA1", "turn A step must keep its pinned A-only tool")
			assert.NotContains(t, names, "bogus1", "turn A must never see the poisoned shared toolset")
			assert.NotContains(t, names, "bogus2")
			assert.NotContains(t, names, "toolB1", "turn A must never see turn B's pinned tools")
		case "B":
			assert.Contains(t, names, "toolShared", "turn B step must keep its pinned shared tool")
			assert.Contains(t, names, "toolB1", "turn B step must keep its pinned B-only tool")
			assert.NotContains(t, names, "bogus1", "turn B must never see the poisoned shared toolset")
			assert.NotContains(t, names, "bogus2")
			assert.NotContains(t, names, "toolA1", "turn B must never see turn A's pinned tools")
		default:
			t.Fatalf("unexpected request with marker %q", marker)
		}
	}
	assert.Equal(t, 2, markerCounts["A"], "turn A must make exactly two requests")
	assert.Equal(t, 2, markerCounts["B"], "turn B must make exactly two requests")

	// The pinned slices themselves must not have been mutated.
	assert.Len(t, pinnedA, 2)
	assert.Len(t, pinnedB, 2)
}
