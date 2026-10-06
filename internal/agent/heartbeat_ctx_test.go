// Heartbeat attribution tests (internal/agent/heartbeat_ctx.go wiring).
//
// REVERT-CHECKS (the orchestrator runs the mutants; each test must fail on
// exactly the listed single-line production change):
//
//	TestHbCtx_TurnAttributionRoot          — delete the withHeartbeat attach in
//	  runTurn (agent_turn.go): ctx reaches the provider without attribution.
//	TestHbCtx_TurnRoleAndSource            — drop hbTurnRole's CallOptions branch or
//	  the withModelSource attach in resolveSessionModelsInternal: role/source regress.
//	TestHbCtx_TitleAttribution             — delete the withHeartbeat attach in
//	  generateTitle (agent_title.go).
//	TestHbCtx_SummaryAttribution           — delete the withHeartbeat attach in
//	  runSummarizeBody (agent_compaction.go).
//	TestHbCtx_SilentSummaryAttribution     — delete the withHeartbeat attach in
//	  runSummarizeSilent (agent_compaction.go).
//	TestHbCtx_KeepaliveAttribution         — delete the withHeartbeat attach in
//	  fireCacheKeepAlive (agent_cache_keepalive.go).
//	TestHbCtx_KeepaliveInheritsTurnRoot    — drop the heartbeat.Context field from
//	  scheduleCacheKeepAliveFromCtx (root would self-attach to the replay).
//	TestHbCtx_FetchPurposeSurvivesChildTurn— delete the withHeartbeat attach in
//	  agentic_fetch_tool.go, or the purpose-preservation branch in withHeartbeat.
//	TestHbCtx_SubAgentTwoLevelsInheritsRoot— delete the inheritHeartbeat wrap in
//	  runSubAgent (coordinator_subagents.go) or the root-inheritance in withHeartbeat.
//	TestHbCtx_AsyncDetachedCarriesAttribution — documents WithoutCancel value
//	  preservation (async_tool.go path) and the uncovered wakeSession
//	  (context.Background()) gap; no production line to break by design.
package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hbIsolateEnv points every global heartbeat/config path at throwaway dirs so
// the package-global registry and any config write stay test-local.
func hbIsolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RUSH_HEARTBEAT_DIR", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
}

// hbCtxModel is a fake language model that records the context of every
// Stream call and answers with a tiny text turn carrying configurable usage.
type hbCtxModel struct {
	mu       sync.Mutex
	ctxSeen  []context.Context
	provider string
	name     string
	usage    fantasy.Usage
}

func (m *hbCtxModel) record(ctx context.Context) {
	m.mu.Lock()
	m.ctxSeen = append(m.ctxSeen, ctx)
	m.mu.Unlock()
}

func (m *hbCtxModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ctxSeen)
}

func (m *hbCtxModel) lastCtx() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.ctxSeen) == 0 {
		return context.Background()
	}
	return m.ctxSeen[len(m.ctxSeen)-1]
}

func (m *hbCtxModel) Generate(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	m.record(ctx)
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "hb"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        m.usage,
	}, nil
}

func (m *hbCtxModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	m.record(ctx)
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "t1"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "t1", Delta: "hb"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "t1"})
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        m.usage,
		})
	}, nil
}

func (m *hbCtxModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *hbCtxModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *hbCtxModel) Provider() string { return m.provider }
func (m *hbCtxModel) Model() string    { return m.name }

// hbModel builds a Model around an hbCtxModel with the given catwalk costs.
func hbModel(provider, name string, usage fantasy.Usage) Model {
	return Model{
		Model: &hbCtxModel{provider: provider, name: name, usage: usage},
		CatwalkCfg: catwalk.Model{
			Name:             name,
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
	}
}

// hbAgent builds a sessionAgent from two Models, mirroring testSessionAgent.
func hbAgent(env fakeEnv, smart, fast Model) *sessionAgent {
	agent := NewSessionAgent(SessionAgentOptions{
		SmartModel:   smart,
		FastModel:    fast,
		SystemPrompt: "hb system prompt",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	})
	if sa, ok := agent.(*sessionAgent); ok {
		env.cleanup(sa.runWg.Wait)
		return sa
	}
	return nil
}

// hbAssertCtx asserts the full attribution of a captured provider ctx.
func hbAssertCtx(t *testing.T, ctx context.Context, wantSession, wantRoot, wantRole, wantSource string, wantPurpose heartbeat.Purpose) {
	t.Helper()
	hb := heartbeat.FromContext(ctx)
	assert.Equal(t, wantSession, hb.SessionID, "SessionID")
	assert.Equal(t, wantRoot, hb.RootSessionID, "RootSessionID")
	assert.Equal(t, wantSession, hb.AgentID, "AgentID")
	assert.Equal(t, wantPurpose, hb.Purpose, "Purpose")
	assert.Equal(t, wantRole, hb.Role, "Role")
	assert.Equal(t, wantSource, hb.Source, "Source")
}

// TestHbCtx_TurnAttributionRoot: a root run's provider ctx carries the turn
// purpose, the default smart role, the slot@start source, and self-rooted ids.
func TestHbCtx_TurnAttributionRoot(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	smart := hbModel("hb-smart-prov", "hb-smart", fantasy.Usage{InputTokens: 1, OutputTokens: 1})
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{InputTokens: 1, OutputTokens: 1})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hello"})
	require.NoError(t, err)
	require.GreaterOrEqual(t, smart.Model.(*hbCtxModel).calls(), 1, "the turn must have reached the smart model")

	hbAssertCtx(t, smart.Model.(*hbCtxModel).lastCtx(), sess.ID, sess.ID, "smart", hbSourceSlotStart, heartbeat.PurposeTurn)
}

// TestHbCtx_TurnRoleAndSource: a per-call role and a session-override source
// marker reach the provider ctx untouched.
func TestHbCtx_TurnRoleAndSource(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	smart := hbModel("hb-smart-prov", "hb-smart", fantasy.Usage{})
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	ctx := withModelSource(t.Context(), hbSourceSessionOverride, hbSourceSlotStart)
	// As runInternal does: the per-call options ride both the ctx and the call.
	opts := &CallOptions{ModelRole: config.SelectedModelTypeFast}
	ctx = WithCallOptions(ctx, opts)
	_, err = agent.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "hello", CallOptions: opts})
	require.NoError(t, err)

	hbAssertCtx(t, smart.Model.(*hbCtxModel).lastCtx(), sess.ID, sess.ID, "fast", hbSourceSessionOverride, heartbeat.PurposeTurn)
}

// TestHbCtx_TitleAttribution: the title goroutine's provider ctx is purpose
// title on the fast role.
func TestHbCtx_TitleAttribution(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	smart := hbModel("hb-smart-prov", "hb-smart", fantasy.Usage{})
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	agent.generateTitle(t.Context(), sess.ID, "a prompt worth titling", turnConfig{
		smartModel: smart,
		fastModel:  fast,
	})
	require.GreaterOrEqual(t, fast.Model.(*hbCtxModel).calls(), 1, "the fast model must answer the title call")

	hbAssertCtx(t, fast.Model.(*hbCtxModel).lastCtx(), sess.ID, sess.ID, "fast", hbSourceSlotStart, heartbeat.PurposeTitle)
}

// TestHbCtx_SummaryAttribution: the manual compaction body's provider ctx is
// purpose summary.
func TestHbCtx_SummaryAttribution(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	smart := hbModel("hb-smart-prov", "hb-smart", fantasy.Usage{})
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	err = agent.runSummarizeBody(t.Context(), sess.ID, nil, smart, "")
	require.NoError(t, err)
	require.GreaterOrEqual(t, smart.Model.(*hbCtxModel).calls(), 1)

	hbAssertCtx(t, smart.Model.(*hbCtxModel).lastCtx(), sess.ID, sess.ID, "smart", hbSourceSlotStart, heartbeat.PurposeSummary)
}

// TestHbCtx_SilentSummaryAttribution: the silent sliding-window compaction is
// purpose summary too.
func TestHbCtx_SilentSummaryAttribution(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	smart := hbModel("hb-smart-prov", "hb-smart", fantasy.Usage{})
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)
	for range 4 {
		_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
		})
		require.NoError(t, err)
	}

	err = agent.runSummarizeSilent(t.Context(), sess.ID, nil, smart, "")
	require.NoError(t, err)
	require.GreaterOrEqual(t, smart.Model.(*hbCtxModel).calls(), 1)

	hbAssertCtx(t, smart.Model.(*hbCtxModel).lastCtx(), sess.ID, sess.ID, "smart", hbSourceSlotStart, heartbeat.PurposeSummary)
}

// hbKeepaliveEnv shrinks the keepalive seams so a replay fires in ~10ms.
func hbKeepaliveEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RUSH_CACHE_KEEPALIVE", "1")
	oldInterval := cacheKeepAliveInterval
	cacheKeepAliveInterval = 10 * time.Millisecond
	t.Cleanup(func() { cacheKeepAliveInterval = oldInterval })
	oldExt := cacheKeepAliveMaxExtensions
	cacheKeepAliveMaxExtensions = 1
	t.Cleanup(func() { cacheKeepAliveMaxExtensions = oldExt })
}

func hbWaitForCalls(t *testing.T, m *hbCtxModel, want int) {
	t.Helper()
	require.Eventually(t, func() bool { return m.calls() >= want }, 10*time.Second, 5*time.Millisecond,
		"keep-alive replay did not reach the model")
}

// TestHbCtx_KeepaliveAttribution: the detached replay's provider ctx is
// purpose keepalive with the default role/source when the turn carried none.
func TestHbCtx_KeepaliveAttribution(t *testing.T) {
	hbIsolateEnv(t)
	hbKeepaliveEnv(t)
	env := testEnv(t)
	// "anthropic" is keep-alive eligible (cache_profile.go).
	smart := hbModel("anthropic", "hb-smart", fantasy.Usage{})
	fast := hbModel("anthropic", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	agent.scheduleCacheKeepAlive(sess.ID, smart, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)
	hbWaitForCalls(t, smart.Model.(*hbCtxModel), 1)

	hbAssertCtx(t, smart.Model.(*hbCtxModel).lastCtx(), sess.ID, sess.ID, "smart", hbSourceSlotStart, heartbeat.PurposeKeepalive)
}

// TestHbCtx_KeepaliveInheritsTurnRoot: a replay scheduled by a turn with
// attribution keeps the turn's root, role, and source on the detached ctx.
func TestHbCtx_KeepaliveInheritsTurnRoot(t *testing.T) {
	hbIsolateEnv(t)
	hbKeepaliveEnv(t)
	env := testEnv(t)
	smart := hbModel("anthropic", "hb-smart", fantasy.Usage{})
	fast := hbModel("anthropic", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	turnHB := heartbeat.Context{
		SessionID: sess.ID, RootSessionID: "root-parent", AgentID: sess.ID,
		Purpose: heartbeat.PurposeTurn, Role: "worker", Source: hbSourcePerCall,
	}
	agent.scheduleCacheKeepAliveFromCtx(turnHB, sess.ID, smart, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)
	hbWaitForCalls(t, smart.Model.(*hbCtxModel), 1)

	hbAssertCtx(t, smart.Model.(*hbCtxModel).lastCtx(), sess.ID, "root-parent", "worker", hbSourcePerCall, heartbeat.PurposeKeepalive)
}

// TestHbCtx_FetchPurposeSurvivesChildTurn: a fetch call point pre-stamps
// purpose fetch (role fast, per-call source); the spawned sub-agent's turn
// keeps it instead of relabeling itself purpose turn.
func TestHbCtx_FetchPurposeSurvivesChildTurn(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	smart := hbModel("hb-smart-prov", "hb-smart", fantasy.Usage{})
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	// What agentic_fetch_tool.go stamps before runSubAgent, with the parent
	// session id the child then re-parents away from.
	ctx := withHeartbeat(t.Context(), "parent-session", heartbeat.PurposeFetch, "fast", hbSourcePerCall)
	_, err = agent.Run(ctx, SessionAgentCall{SessionID: sess.ID, Prompt: "fetch this"})
	require.NoError(t, err)

	hbAssertCtx(t, smart.Model.(*hbCtxModel).lastCtx(), sess.ID, "parent-session", "fast", hbSourcePerCall, heartbeat.PurposeFetch)
}

// TestHbCtx_SubAgentTwoLevelsInheritsRoot: a sub-agent two levels deep still
// reports the top run's root session id, never its own and never a parsed id.
func TestHbCtx_SubAgentTwoLevelsInheritsRoot(t *testing.T) {
	hbIsolateEnv(t)

	root := withHeartbeat(context.Background(), "root-session", heartbeat.PurposeTurn, "smart", hbSourceSlotStart)
	hbAssertCtx(t, root, "root-session", "root-session", "smart", hbSourceSlotStart, heartbeat.PurposeTurn)

	// Level 1: runSubAgent's delegation boundary, then the child's own turn.
	child1 := inheritHeartbeat(root, "msg1$$call1", "worker", hbSourceSlotStart)
	child1 = withHeartbeat(child1, "msg1$$call1", heartbeat.PurposeTurn, "worker", hbSourceSlotStart)
	hbAssertCtx(t, child1, "msg1$$call1", "root-session", "worker", hbSourceSlotStart, heartbeat.PurposeTurn)

	// Level 2: the grandchild dispatch must inherit the TOP root, not level 1.
	child2 := inheritHeartbeat(child1, "msg2$$call2", "worker", hbSourceSlotStart)
	child2 = withHeartbeat(child2, "msg2$$call2", heartbeat.PurposeTurn, "worker", hbSourceSlotStart)
	hbAssertCtx(t, child2, "msg2$$call2", "root-session", "worker", hbSourceSlotStart, heartbeat.PurposeTurn)
}

// TestHbCtx_AsyncDetachedCarriesAttribution documents the async path split:
// async_tool.go's detached jobCtx (context.WithoutCancel) keeps attribution,
// while the coordinator_background.go wake path starts from
// context.Background() and self-roots — the known uncovered gap.
func TestHbCtx_AsyncDetachedCarriesAttribution(t *testing.T) {
	hbIsolateEnv(t)

	stamped := withHeartbeat(context.Background(), "child-session", heartbeat.PurposeTurn, "worker", hbSourcePerCall)
	detached := context.WithoutCancel(stamped)
	hb := heartbeat.FromContext(detached)
	assert.Equal(t, "child-session", hb.SessionID)
	assert.Equal(t, hb.RootSessionID, heartbeat.FromContext(stamped).RootSessionID,
		"WithoutCancel (async_tool.go's jobCtx) must preserve attribution values")

	// The documented gap: a ctx built from context.Background() (the
	// out-of-scope wake path) carries nothing and self-roots at runTurn.
	assert.Equal(t, heartbeat.Context{}, heartbeat.FromContext(context.Background()))
}
