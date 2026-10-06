// Heartbeat usage tests (AddUsage at the five accounting sites).
//
// REVERT-CHECKS (the orchestrator runs the mutants; each test must fail on
// exactly the listed single-line production change):
//
//	TestHbUsage_TurnRecorded            — delete the addHeartbeatUsage call in
//	  applyStepUsage (agent_turn_step.go): per-step tokens/cost go unreported.
//	TestHbUsage_TwoTurnsSumExactlyOnce  — move the applyStepUsage add out of the
//	  per-step path (or double it): totals drift from DB accounting.
//	TestHbUsage_TitleRecorded           — delete the addHeartbeatUsage call in
//	  generateTitle (agent_title.go).
//	TestHbUsage_SummaryRecorded         — delete the addHeartbeatUsage call in
//	  runSummarizeBody (agent_compaction.go).
//	TestHbUsage_SilentSummaryRecorded   — delete the addHeartbeatUsage call in
//	  runSummarizeSilent (agent_compaction.go).
//	TestHbUsage_KeepaliveRecorded       — delete the addHeartbeatUsage call in
//	  recordCacheKeepAliveCost (agent_cache_keepalive.go).
//	TestHbUsage_ZeroUsageAddsNothing    — drop hbUsageNonZero's guard: an all-zero
//	  delta creates an empty registry entry.
package agent

import (
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hbReadUsage flushes the package-global registry into the test's
// RUSH_HEARTBEAT_DIR and returns the snapshot for rootID, if any.
func hbReadUsage(t *testing.T, rootID string) (*heartbeat.Entry, bool) {
	t.Helper()
	heartbeat.Shutdown(3 * time.Second)
	entries, err := heartbeat.ReadAll()
	require.NoError(t, err)
	for i := range entries {
		if entries[i].Session == rootID {
			return &entries[i], true
		}
	}
	return nil, false
}

// hbEntryCost recomputes the cost delta updateSessionUsage derives from the
// same catwalk figures, so the test never hard-codes an expected number.
func hbEntryCost(m Model, usage fantasy.Usage) float64 {
	return m.CatwalkCfg.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		m.CatwalkCfg.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		m.CatwalkCfg.CostPer1MIn/1e6*float64(usage.InputTokens) +
		m.CatwalkCfg.CostPer1MOut/1e6*float64(usage.OutputTokens)
}

// hbFindModel locates one provider/model entry inside a snapshot.
func hbFindModel(t *testing.T, entry *heartbeat.Entry, provider, name string) (input, output, cacheRead, cacheWrite int64, cost float64, ok bool) {
	t.Helper()
	require.NotNil(t, entry)
	for _, m := range entry.Models {
		if m.Provider == provider && m.Model == name {
			return m.Input, m.Output, m.CacheRead, m.CacheWrite, m.CostUSD, true
		}
	}
	return 0, 0, 0, 0, 0, false
}

// hbPurposeTotals reads one purpose bucket's input/output off a model entry.
func hbPurposeTotals(t *testing.T, entry *heartbeat.Entry, provider, name, purpose string) (in, out int64, found bool) {
	t.Helper()
	for _, m := range entry.Models {
		if m.Provider != provider || m.Model != name {
			continue
		}
		if ps := m.ByPurpose[purpose]; ps != nil {
			return ps.Input, ps.Output, true
		}
		return 0, 0, false
	}
	return 0, 0, false
}

// hbCostedTurnModel returns a smart Model with billable catwalk costs.
func hbCostedTurnModel(provider, name string, usage fantasy.Usage) Model {
	m := hbModel(provider, name, usage)
	m.CatwalkCfg.CostPer1MIn = 1000
	m.CatwalkCfg.CostPer1MOut = 2000
	m.CatwalkCfg.CostPer1MInCached = 3000
	m.CatwalkCfg.CostPer1MOutCached = 4000
	return m
}

// TestHbUsage_TurnRecorded: one accounted step's tokens/cost land once under
// the turn purpose, on the model that answered.
func TestHbUsage_TurnRecorded(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	usage := fantasy.Usage{
		InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5, CacheCreationTokens: 7,
	}
	smart := hbCostedTurnModel("hb-smart-prov", "hb-smart", usage)
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hello"})
	require.NoError(t, err)

	entry, found := hbReadUsage(t, sess.ID)
	require.True(t, found, "the turn's usage must create a snapshot for the root session")

	in, out, cr, cw, cost, ok := hbFindModel(t, entry, "hb-smart-prov", "hb-smart")
	require.True(t, ok, "the answering model must have its own entry")
	assert.Equal(t, usage.InputTokens, in)
	assert.Equal(t, usage.OutputTokens, out)
	assert.Equal(t, usage.CacheReadTokens, cr)
	assert.Equal(t, usage.CacheCreationTokens, cw)
	assert.InDelta(t, hbEntryCost(smart, usage), cost, 1e-9)

	pIn, pOut, ok := hbPurposeTotals(t, entry, "hb-smart-prov", "hb-smart", "turn")
	require.True(t, ok, "the turn purpose bucket must exist")
	assert.Equal(t, usage.InputTokens, pIn)
	assert.Equal(t, usage.OutputTokens, pOut)
}

// TestHbUsage_TwoTurnsSumExactlyOnce: two accounted turns (the shape of a
// durable retry re-running the turn) each count exactly once.
func TestHbUsage_TwoTurnsSumExactlyOnce(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	usage := fantasy.Usage{InputTokens: 50, OutputTokens: 10}
	smart := hbCostedTurnModel("hb-smart-prov", "hb-smart", usage)
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	for range 2 {
		_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "hello"})
		require.NoError(t, err)
	}

	entry, found := hbReadUsage(t, sess.ID)
	require.True(t, found)
	in, out, _, _, cost, ok := hbFindModel(t, entry, "hb-smart-prov", "hb-smart")
	require.True(t, ok)
	assert.Equal(t, 2*usage.InputTokens, in)
	assert.Equal(t, 2*usage.OutputTokens, out)
	assert.InDelta(t, 2*hbEntryCost(smart, usage), cost, 1e-9)
}

// TestHbUsage_TitleRecorded: the title call's usage lands under the title
// purpose on the fast model.
func TestHbUsage_TitleRecorded(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	usage := fantasy.Usage{InputTokens: 30, OutputTokens: 4}
	smart := hbModel("hb-smart-prov", "hb-smart", fantasy.Usage{})
	fast := hbCostedTurnModel("hb-fast-prov", "hb-fast", usage)
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	agent.generateTitle(t.Context(), sess.ID, "a prompt worth titling", turnConfig{
		smartModel: smart,
		fastModel:  fast,
	})

	entry, found := hbReadUsage(t, sess.ID)
	require.True(t, found)
	_, _, _, _, cost, ok := hbFindModel(t, entry, "hb-fast-prov", "hb-fast")
	require.True(t, ok, "the title model must have its own entry")
	assert.InDelta(t, hbEntryCost(fast, usage), cost, 1e-9)

	pIn, _, ok := hbPurposeTotals(t, entry, "hb-fast-prov", "hb-fast", "title")
	require.True(t, ok, "the title purpose bucket must exist")
	assert.Equal(t, usage.InputTokens, pIn)
}

// TestHbUsage_SummaryRecorded: the manual compaction's usage lands under the
// summary purpose.
func TestHbUsage_SummaryRecorded(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	usage := fantasy.Usage{InputTokens: 200, OutputTokens: 40}
	smart := hbCostedTurnModel("hb-smart-prov", "hb-smart", usage)
	fast := hbModel("hb-fast-prov", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	require.NoError(t, agent.runSummarizeBody(t.Context(), sess.ID, nil, smart, ""))

	entry, found := hbReadUsage(t, sess.ID)
	require.True(t, found)
	_, _, _, _, cost, ok := hbFindModel(t, entry, "hb-smart-prov", "hb-smart")
	require.True(t, ok)
	assert.InDelta(t, hbEntryCost(smart, usage), cost, 1e-9)

	pIn, _, ok := hbPurposeTotals(t, entry, "hb-smart-prov", "hb-smart", "summary")
	require.True(t, ok, "the summary purpose bucket must exist")
	assert.Equal(t, usage.InputTokens, pIn)
}

// TestHbUsage_SilentSummaryRecorded: the silent compaction reports under the
// same summary purpose.
func TestHbUsage_SilentSummaryRecorded(t *testing.T) {
	hbIsolateEnv(t)
	env := testEnv(t)
	usage := fantasy.Usage{InputTokens: 80, OutputTokens: 15}
	smart := hbCostedTurnModel("hb-smart-prov", "hb-smart", usage)
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

	require.NoError(t, agent.runSummarizeSilent(t.Context(), sess.ID, nil, smart, ""))

	entry, found := hbReadUsage(t, sess.ID)
	require.True(t, found)
	pIn, _, ok := hbPurposeTotals(t, entry, "hb-smart-prov", "hb-smart", "summary")
	require.True(t, ok, "the summary purpose bucket must exist")
	assert.Equal(t, usage.InputTokens, pIn)
}

// TestHbUsage_KeepaliveRecorded: the replay's tokens are reported even when
// the cost is zero (no billable catwalk figures), under the keepalive purpose.
func TestHbUsage_KeepaliveRecorded(t *testing.T) {
	hbIsolateEnv(t)
	hbKeepaliveEnv(t)
	env := testEnv(t)
	usage := fantasy.Usage{InputTokens: 500, OutputTokens: 1}
	smart := hbModel("anthropic", "hb-smart", usage)
	fast := hbModel("anthropic", "hb-fast", fantasy.Usage{})
	agent := hbAgent(env, smart, fast)

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	agent.scheduleCacheKeepAlive(sess.ID, smart, []fantasy.Message{fantasy.NewUserMessage("hi")}, nil, nil, 0)
	hbWaitForCalls(t, smart.Model.(*hbCtxModel), 1)

	entry, found := hbReadUsage(t, sess.ID)
	require.True(t, found)
	in, out, _, _, cost, ok := hbFindModel(t, entry, "anthropic", "hb-smart")
	require.True(t, ok, "the replay model must have its own entry")
	assert.Equal(t, usage.InputTokens, in)
	assert.Equal(t, usage.OutputTokens, out)
	assert.InDelta(t, 0.0, cost, 1e-9)

	pIn, _, ok := hbPurposeTotals(t, entry, "anthropic", "hb-smart", "keepalive")
	require.True(t, ok, "the keepalive purpose bucket must exist")
	assert.Equal(t, usage.InputTokens, pIn)
}

// TestHbUsage_ZeroUsageAddsNothing: an all-zero accounted unit must not create
// a snapshot (proven at the title site, whose usage has no fallback estimate).
func TestHbUsage_ZeroUsageAddsNothing(t *testing.T) {
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
	require.GreaterOrEqual(t, fast.Model.(*hbCtxModel).calls(), 1, "precondition: the title call answered")

	_, found := hbReadUsage(t, sess.ID)
	assert.False(t, found, "a zero usage must not create a registry entry")

	// And the helper-level contract: an all-zero delta is dropped.
	assert.False(t, hbUsageNonZero(hbUsage(fantasy.Usage{}, 0)))
	assert.True(t, hbUsageNonZero(hbUsage(fantasy.Usage{OutputTokens: 1}, 0)))
	assert.True(t, hbUsageNonZero(hbUsage(fantasy.Usage{}, 0.01)))
}
