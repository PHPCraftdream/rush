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
	"github.com/PHPCraftdream/rush/internal/permission"
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

// TestRunWithCredentials_PinsPerCallTools proves F1: the credentials entry
// point must pin the same per-call toolset Run pins — built from THIS
// call's context (CallOptions.DisableSubAgents / CallOptions.ModelRole) —
// onto the SessionAgentCall the turn actually executes. Pre-fix,
// resolveCredentialsModels never assigned resolved.tools, so pin() skipped
// Tools and runTurn fell back to the shared a.tools.Copy(): the model kept
// the delegation tools despite DisableSubAgents, and kept edit/write tools
// despite an orchestrator-mode role.
func TestRunWithCredentials_PinsPerCallTools(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, true /* worker configured: the role case needs the orchestrator strip */)

	var mu sync.Mutex
	var calls []SessionAgentCall
	mock := newMockAgent("cred-provider", 4096,
		func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			mu.Lock()
			calls = append(calls, call)
			mu.Unlock()
			return agentResultWithText("ok"), nil
		})
	coord.currentAgent = mock

	sess, err := coord.sessions.Create(t.Context(), "cred-pin")
	require.NoError(t, err)

	creds := &CredentialSet{
		Credentials: []Credential{{
			Provider: "cred-provider",
			Type:     ProviderTypeOpenAI,
			APIKey:   "sk-cred-test",
			Models:   []CredentialModel{{ID: "cred-model", ContextWindow: 200000, DefaultMaxTokens: 4096}},
		}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "cred-provider", Model: "cred-model"},
			RoleFast:  {Provider: "cred-provider", Model: "cred-model"},
		},
	}

	// pinnedToolsOf runs one full RunWithCredentials turn and returns the
	// Tools slice the turn's SessionAgentCall actually carried.
	pinnedToolsOf := func(run func() error) []fantasy.AgentTool {
		mu.Lock()
		before := len(calls)
		mu.Unlock()
		require.NoError(t, run())
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, calls, before+1, "the mock agent must have received exactly one new call")
		return calls[before].Tools
	}

	banned := pinnedToolsOf(func() error {
		_, err := coord.RunWithCredentials(
			WithCallOptions(t.Context(), &CallOptions{DisableSubAgents: true}),
			sess.ID, "cred run with sub-agents disabled", creds)
		return err
	})
	require.NotNil(t, banned, "RunWithCredentials must pin a per-call toolset; nil makes runTurn fall back to the shared slice")
	namesBanned := pinnedToolNames(banned)
	assert.NotContains(t, namesBanned, AgentToolName, "DisableSubAgents must strip the agent tool from the pinned set")
	assert.NotContains(t, namesBanned, "agentic_fetch", "DisableSubAgents must strip agentic_fetch from the pinned set")
	assert.Contains(t, namesBanned, "bash", "unrelated tools must survive the per-call filter")

	allowed := pinnedToolsOf(func() error {
		_, err := coord.RunWithCredentials(
			WithCallOptions(t.Context(), &CallOptions{DisableSubAgents: false}),
			sess.ID, "cred run with sub-agents allowed", creds)
		return err
	})
	namesAllowed := pinnedToolNames(allowed)
	assert.Contains(t, namesAllowed, AgentToolName, "the allowed call keeps the agent tool")
	assert.Contains(t, namesAllowed, "agentic_fetch", "the allowed call keeps agentic_fetch")

	// Role policy: an explicit smart role with a worker configured is an
	// orchestrator run — the pinned set must be stripped of the edit tools,
	// agreeing with the per-call orchestrator prompt.
	orchestrator := pinnedToolsOf(func() error {
		_, err := coord.RunWithCredentials(
			WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeSmart}),
			sess.ID, "cred run as orchestrator", creds)
		return err
	})
	namesOrchestrator := pinnedToolNames(orchestrator)
	assert.NotContains(t, namesOrchestrator, "edit", "per-call smart + worker configured => orchestrator strip")
	assert.NotContains(t, namesOrchestrator, "multiedit")
	assert.NotContains(t, namesOrchestrator, "write")
	assert.Contains(t, namesOrchestrator, AgentToolName, "the orchestrator keeps the agent tool")
}

// ---- per-call folder-scope toolset (T8) ----

// newFolderScope compiles a single-entry scope rooted at workingDir
// granting the given ops.
func newFolderScope(t *testing.T, workingDir string, ops ...permission.FileOp) permission.FolderScope {
	t.Helper()
	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: workingDir,
		Entries: []permission.FolderScopeEntry{
			{Dir: ".", Ops: ops},
		},
	})
	require.NoError(t, err)
	return scope
}

// mustBuildTools is buildTools with the error pre-asserted: the scoped
// toolset tests only care about the resulting tool names.
func mustBuildTools(t *testing.T, coord *coordinator, ctx context.Context, cfg *config.Config, agentCfg config.Agent, isSubAgent bool) []fantasy.AgentTool {
	t.Helper()
	got, err := coord.buildTools(ctx, cfg, agentCfg, isSubAgent)
	require.NoError(t, err)
	return got
}

func assertToolsContain(t *testing.T, names []string, want ...string) {
	t.Helper()
	for _, w := range want {
		assert.Contains(t, names, w)
	}
}

func assertToolsOmit(t *testing.T, names []string, banned ...string) {
	t.Helper()
	for _, b := range banned {
		assert.NotContains(t, names, b)
	}
}

// TestFolderScope_PinsPerCallScopedToolsetWithoutCrossContamination proves
// the scoped toolset is decided per call: a scoped call pins only the
// fs_* tools its scope grants plus the non-filesystem tools the scope
// does not touch, a second unscoped call on a DIFFERENT session in the
// same coordinator keeps the full legacy toolset, and re-resolving in the
// opposite order does not bleed either way.
func TestFolderScope_PinsPerCallScopedToolsetWithoutCrossContamination(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false /* no worker configured */)
	rec := &toolPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = rec

	scope := newFolderScope(t, env.workingDir,
		permission.FileOpList, permission.FileOpFind, permission.FileOpGrep, permission.FileOpRead)

	sessScoped, err := coord.sessions.Create(t.Context(), "scoped-session")
	require.NoError(t, err)
	sessUnscoped, err := coord.sessions.Create(t.Context(), "unscoped-session")
	require.NoError(t, err)

	ctxScoped := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})
	ctxUnscoped := WithCallOptions(t.Context(), &CallOptions{})

	pinnedScoped, err := coord.resolveSessionModels(ctxScoped, sessScoped.ID)
	require.NoError(t, err)
	require.NotNil(t, pinnedScoped.tools, "the scoped call must get a pinned toolset")
	namesScoped := pinnedToolNames(pinnedScoped.tools)

	assertToolsContain(t, namesScoped,
		"fs_list", "fs_find", "fs_grep", "fs_read",
		AgentToolName, "todos", "ask_question")
	assertToolsOmit(t, namesScoped,
		// legacy file tools
		"view", "glob", "grep", "ls", "write", "edit", "multiedit",
		// escape hatches
		"download", "git_read", "agentic_fetch", "list_mcp_resources", "read_mcp_resource",
		// command tools without KeepsCommandTools
		"bash", "run_command", "job_output", "job_kill",
		// fs_* tools whose op the scope does not grant
		"fs_write", "fs_replace", "fs_write_lines", "fs_delete")

	// The concurrent unscoped call on the other session keeps the full
	// legacy toolset.
	pinnedUnscoped, err := coord.resolveSessionModels(ctxUnscoped, sessUnscoped.ID)
	require.NoError(t, err)
	require.NotNil(t, pinnedUnscoped.tools)
	namesUnscoped := pinnedToolNames(pinnedUnscoped.tools)
	assertToolsContain(t, namesUnscoped,
		"view", "glob", "grep", "ls", "write", "edit", "multiedit", "bash",
		"download", "git_read", "agentic_fetch", "run_command")

	// Isolation, both directions: re-resolving the scoped policy AFTER the
	// unscoped one must still see the scoped slice, and vice versa.
	pinnedScoped2, err := coord.resolveSessionModels(ctxScoped, sessScoped.ID)
	require.NoError(t, err)
	namesScoped2 := pinnedToolNames(pinnedScoped2.tools)
	assertToolsOmit(t, namesScoped2, "bash", "view", "fs_delete")
	assertToolsContain(t, namesScoped2, "fs_read")

	pinnedUnscoped2, err := coord.resolveSessionModels(ctxUnscoped, sessUnscoped.ID)
	require.NoError(t, err)
	namesUnscoped2 := pinnedToolNames(pinnedUnscoped2.tools)
	assertToolsContain(t, namesUnscoped2, "bash", "view", "edit")

	// THE KEY ASSERTION: neither resolution may publish to the shared
	// agent (R3-1).
	names, modelCalls := rec.snapshot()
	assert.Empty(t, names, "resolveSessionModels must never SetTools the shared currentAgent")
	assert.Zero(t, modelCalls)
}

// TestUpdateModels_NeverPublishesFolderScopeFilter extends the
// global-publish contract to the folder-scope filter: UpdateModels strips
// CallOptions from its context, so one scoped call's fs_* toolset must
// NOT decide what every other in-flight turn sees on the shared agent.
func TestUpdateModels_NeverPublishesFolderScopeFilter(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	rec := &toolPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = rec

	sess, err := coord.sessions.Create(t.Context(), "update-models-scoped")
	require.NoError(t, err)
	_ = sess

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctx := WithCallOptions(t.Context(), &CallOptions{
		FolderScope:      &scope,
		DisableSubAgents: true,
	})
	require.NoError(t, coord.UpdateModels(ctx))

	names, _ := rec.snapshot()
	require.Len(t, names, 1, "UpdateModels must publish exactly one global toolset")
	assert.Contains(t, names[0], AgentToolName, "the per-call folder-scope filter must NOT be applied to the global publish")
	assert.Contains(t, names[0], "agentic_fetch")
	assert.Contains(t, names[0], "view", "legacy file tools must survive the global publish")
	assert.Contains(t, names[0], "bash")
}

// TestFolderScope_RunsAfterWorkerToolLayering proves the scope filter
// sees the FINAL AllowedTools: for a sub-agent acting as a worker (worker
// configured, per-call smart role) buildToolsAgentConfigForCall ADDS
// bash/edit/write via workerToolNames, and the scoped filter strips them
// anyway -- it genuinely runs AFTER the layering, not before.
func TestFolderScope_RunsAfterWorkerToolLayering(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, true /* worker configured */)
	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "task agent must be configured")
	cfgSnap, _ := coord.cfg.Snapshot()

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead, permission.FileOpWriteLines)
	ctxScopedWorker := WithCallOptions(t.Context(), &CallOptions{
		ModelRole:   config.SelectedModelTypeSmart,
		FolderScope: &scope,
	})

	names := pinnedToolNames(mustBuildTools(t, coord, ctxScopedWorker, cfgSnap, taskCfg, true))
	assertToolsOmit(t, names,
		"bash", "edit", "multiedit", "write",
		"download", "git_read", "agentic_fetch")
	assertToolsContain(t, names, "fs_read", "fs_write_lines")
	assertToolsOmit(t, names, "fs_delete", "fs_write", "fs_replace")

	// Control: the same worker layering WITHOUT a scope keeps bash --
	// the strip above is the scope filter's doing, not the layering's
	// absence.
	ctxUnscopedWorker := WithCallOptions(t.Context(), &CallOptions{ModelRole: config.SelectedModelTypeSmart})
	namesUnscoped := pinnedToolNames(mustBuildTools(t, coord, ctxUnscopedWorker, cfgSnap, taskCfg, true))
	assert.Contains(t, namesUnscoped, "bash",
		"worker layering must add bash when no scope is set, so the strip above is attributable to the scope filter")
}

// TestFolderScope_PlainSubAgentGetsGrantedReadSideFsTools confirms the
// context propagation empirically: buildAgent registers its tool build on
// readyWg with the CALL's context, so the sub-agent behind the agent tool
// inside a scoped call is built through buildTools with that same scoped
// context -- its toolset is the scoped one, not the legacy read-only
// default.
func TestFolderScope_PlainSubAgentGetsGrantedReadSideFsTools(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "task agent must be configured")

	scope := newFolderScope(t, env.workingDir,
		permission.FileOpList, permission.FileOpFind, permission.FileOpGrep, permission.FileOpRead)
	ctxScoped := WithCallOptions(t.Context(), &CallOptions{FolderScope: &scope})

	tp, err := taskPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)

	subAgent, err := coord.buildAgent(ctxScoped, tp, taskCfg, true)
	require.NoError(t, err)
	require.NoError(t, coord.readyWg.Wait(), "the sub-agent's async build must succeed")

	sa, ok := subAgent.(*sessionAgent)
	require.True(t, ok, "buildAgent must return a *sessionAgent")
	names := pinnedToolNames(sa.tools.Copy())

	assertToolsContain(t, names, "fs_list", "fs_find", "fs_grep", "fs_read")
	assertToolsOmit(t, names,
		"view", "glob", "grep", "ls", "git_read", "bash",
		"fs_write", "fs_replace", "fs_write_lines", "fs_delete")

	// Control: the SAME sub-agent build on an unscoped context keeps the
	// legacy read-only toolset, so the difference above is attributable
	// to the scoped context, not the task agent's defaults.
	subAgentUnscoped, err := coord.buildAgent(WithCallOptions(t.Context(), &CallOptions{}), tp, taskCfg, true)
	require.NoError(t, err)
	require.NoError(t, coord.readyWg.Wait())
	namesUnscoped := pinnedToolNames(subAgentUnscoped.(*sessionAgent).tools.Copy())
	assertToolsContain(t, namesUnscoped, "view", "grep", "ls", "git_read")
}

// TestFolderScope_CommandToolsFollowKeepsCommandTools proves the command
// tools stay only by an explicit KeepsCommandTools grant (what the run
// plumbing will set from the restricted-run bash allowlist; the raw
// CallOptions here stubs that decision).
func TestFolderScope_CommandToolsFollowKeepsCommandTools(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	cfgSnap, _ := coord.cfg.Snapshot()

	commandTools := []string{"bash", "run_command", "job_output", "job_kill"}

	keptScope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir:       env.workingDir,
		Entries:          []permission.FolderScopeEntry{{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead}}},
		KeepCommandTools: true,
	})
	require.NoError(t, err)

	ctxKept := WithCallOptions(t.Context(), &CallOptions{FolderScope: &keptScope})
	namesKept := pinnedToolNames(mustBuildTools(t, coord, ctxKept, cfgSnap, coderCfg, false))
	assertToolsContain(t, namesKept, commandTools...)
	assertToolsOmit(t, namesKept, "view", "write")

	strippedScope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctxStripped := WithCallOptions(t.Context(), &CallOptions{FolderScope: &strippedScope})
	namesStripped := pinnedToolNames(mustBuildTools(t, coord, ctxStripped, cfgSnap, coderCfg, false))
	assertToolsOmit(t, namesStripped, commandTools...)
	assert.Contains(t, namesStripped, "fs_read")
}

// TestFolderScope_StripsAgenticFetchEvenWhenSubAgentsAllowed proves the
// escape-hatch strip is the scope filter's own decision: with
// DisableSubAgents explicitly false, applyCallDisableSubAgents keeps
// agentic_fetch, and applyCallFolderScope still removes it (and download)
// from the scoped toolset.
func TestFolderScope_StripsAgenticFetchEvenWhenSubAgentsAllowed(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	coderCfg, ok := coord.cfg.Config().Agents[config.AgentCoder]
	require.True(t, ok, "coder agent must be configured")
	cfgSnap, _ := coord.cfg.Snapshot()

	scope := newFolderScope(t, env.workingDir, permission.FileOpRead)
	ctx := WithCallOptions(t.Context(), &CallOptions{
		DisableSubAgents: false,
		FolderScope:      &scope,
	})
	names := pinnedToolNames(mustBuildTools(t, coord, ctx, cfgSnap, coderCfg, false))
	assert.NotContains(t, names, "agentic_fetch",
		"the scope filter must strip agentic_fetch even when the sub-agent ban does not")
	assert.NotContains(t, names, "download")
	assert.Contains(t, names, AgentToolName,
		"the agent delegation tool stays: the sub-agent inherits the same scoped context")
	assert.Contains(t, names, "fs_read")
}
