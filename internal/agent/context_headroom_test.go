package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/discover"
	"github.com/stretchr/testify/require"
)

// Incident: a direct openai-codex gpt-6.1-sol session reached 921,485 prompt
// tokens with no summary and was rejected with context_length_exceeded.
//
// Facts: window 1,050,000, default max output 128,000 => input budget
// 922,000. Headroom is max(20,000, budget/20) = 46,100, so the gate fires once
// prompt+completion >= 922,000-46,100 = 875,900 (old fixed 20k: 902,000).
const (
	solContextWindow int64 = 1_050_000
	solOutputReserve int64 = 128_000
	solInputBudget   int64 = 922_000
	solHeadroom      int64 = 46_100
	solGateUsage     int64 = solInputBudget - solHeadroom // 875,900
	solOldGateUsage  int64 = solInputBudget - 20_000      // 902,000
	solIncidentTotal int64 = 921_602
)

// solModelFacts resolves gpt-6.1-sol through the real model-facts table from
// a deliberately small catalog window, as the incident's config did.
func solModelFacts(t *testing.T) catwalk.Model {
	t.Helper()
	models := discover.ApplyModelFacts("openai-codex", []catwalk.Model{{ID: "gpt-6.1-sol", ContextWindow: 272_000}}, nil)
	require.Len(t, models, 1)
	m := models[0]
	require.Equal(t, solContextWindow, m.ContextWindow, "facts window")
	require.Equal(t, solOutputReserve, m.DefaultMaxTokens, "facts default max output")
	return m
}

// solModel is gpt-6.1-sol as the runtime sees it: facts-derived catalog entry
// plus the provider/model selection that keys the documented max input.
func solModel(t *testing.T) Model {
	t.Helper()
	return Model{
		CatwalkCfg: solModelFacts(t),
		ModelCfg:   config.SelectedModel{Provider: "openai-codex", Model: "gpt-6.1-sol"},
	}
}

// The documented max input (922,000) is a hard ceiling on the input budget:
// window-minus-reserve alone gives 1,018,000 for a 32,000 output override.
func TestModelInputBudgetNeverExceedsDocumentedMaxInput(t *testing.T) {
	t.Parallel()
	sol := solModel(t)
	require.Equal(t, solInputBudget, modelInputBudget(sol, 0), "default reserve 128,000")
	require.Equal(t, solInputBudget, modelInputBudget(sol, 32_000), "call override below default: 1,050,000-32,000 capped")
	require.Equal(t, int64(750_000), modelInputBudget(sol, 300_000), "larger reserve shrinks the budget below the cap")

	cfgOverride := sol
	cfgOverride.ModelCfg.MaxTokens = 32_000
	require.Equal(t, solInputBudget, modelInputBudget(cfgOverride, 0), "configured max_tokens below default")

	noReserve := sol
	noReserve.CatwalkCfg.DefaultMaxTokens = 0
	require.Equal(t, solInputBudget, modelInputBudget(noReserve, 0), "unknown reserve: whole window capped")

	userWindow := sol
	userWindow.CatwalkCfg.ContextWindow = 500_000
	require.Equal(t, int64(372_000), modelInputBudget(userWindow, 0), "a user-lowered window still wins")

	// Control: a model with no documented max input keeps window-minus-reserve.
	other := sol
	other.ModelCfg = config.SelectedModel{Provider: "openai-codex", Model: "codex-model"}
	other.CatwalkCfg.ID = "codex-model"
	require.Equal(t, int64(1_018_000), modelInputBudget(other, 32_000))
}

// The gate uses the capped budget: with a 32,000 output override the boundary
// stays at 875,900 (uncapped it would be 971,900).
func TestStopConditionsCapsBudgetWithLowerOutputOverride(t *testing.T) {
	t.Parallel()
	fires := func(usage int64) bool {
		ts := newContextCapacityRuntimeStream(solModel(t), SessionAgentCall{MaxOutputTokens: 32_000}, fantasy.Usage{InputTokens: usage})
		return ts.stopConditions()[0](nil)
	}
	require.False(t, fires(solGateUsage-1))
	require.True(t, fires(solGateUsage))
}

func TestContextAutoSummarizeThresholdProportionalHeadroom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		window, input int64
		want          int64
	}{
		{"gpt-6.1-sol proportional", solContextWindow, solInputBudget, solHeadroom},
		{"large window small budget keeps 20k floor", 272_000, 144_000, 20_000},
		{"budget/20 equals floor", 1_050_000, 400_000, 20_000},
		{"budget below floor is capped by budget", 300_000, 5_000, 5_000},
		{"unknown reserve legacy budget=window", solContextWindow, solContextWindow, 52_500},
		{"small window ratio unchanged", 128_000, 100_000, 20_000},
		{"window at 200k is still small", 200_000, 150_000, 30_000},
		{"unknown window", 0, 100, 0},
		{"no budget", solContextWindow, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, contextAutoSummarizeThreshold(tt.window, tt.input))
		})
	}
}

func TestStopConditionsGPT61SolHeadroomBoundary(t *testing.T) {
	t.Parallel()
	model := solModel(t)
	require.Equal(t, solInputBudget, contextInputBudget(solContextWindow, effectiveMaxOutputTokens(model, 0)))
	require.Equal(t, solHeadroom, contextAutoSummarizeThreshold(solContextWindow, solInputBudget))

	fires := func(usage int64) bool {
		ts := newContextCapacityRuntimeStream(model, SessionAgentCall{}, fantasy.Usage{InputTokens: usage})
		return ts.stopConditions()[0](nil)
	}
	require.False(t, fires(solGateUsage-1), "remaining 46,101 is above the 46,100 headroom")
	require.True(t, fires(solGateUsage), "remaining 46,100 equals the headroom")
	// 890,000 leaves 32,000 tokens: the old fixed 20k gate stayed silent here.
	require.Less(t, int64(890_000), solOldGateUsage)
	require.True(t, fires(890_000))
	require.True(t, fires(solOldGateUsage))
	require.Less(t, solGateUsage, solIncidentTotal-40_000, "gate fires well before the incident total")
	require.True(t, fires(solIncidentTotal))
}

// Without a known output reserve the legacy budget=window gate is kept; only
// the large-window headroom (52,500 = 1,050,000/20) applies: fires at
// usage >= 997,500.
func TestStopConditionsUnknownReserveKeepsLegacyBudget(t *testing.T) {
	t.Parallel()
	model := Model{CatwalkCfg: catwalk.Model{ContextWindow: solContextWindow}}
	fires := func(usage int64) bool {
		ts := newContextCapacityRuntimeStream(model, SessionAgentCall{}, fantasy.Usage{InputTokens: usage})
		return ts.stopConditions()[0](nil)
	}
	require.False(t, fires(997_499))
	require.True(t, fires(997_500))
}

func TestStopConditionsSmallWindowGateUnchanged(t *testing.T) {
	t.Parallel()
	// Window 128,000, reserve 28,000 => budget 100,000, threshold 20% = 20,000.
	model := Model{CatwalkCfg: catwalk.Model{ContextWindow: 128_000, DefaultMaxTokens: 28_000}}
	fires := func(usage int64) bool {
		ts := newContextCapacityRuntimeStream(model, SessionAgentCall{}, fantasy.Usage{InputTokens: usage})
		return ts.stopConditions()[0](nil)
	}
	require.False(t, fires(79_999))
	require.True(t, fires(80_000))
}

// headroomPrepare runs one prepareStep with gpt-6.1-sol's real limits and
// prompt tokens as the previous step's reported usage.
func headroomPrepare(t *testing.T, name string, prompt int64, msgs []fantasy.Message) *turnStream {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	f := newAttemptFixture(t, name, attemptFixtureOpts{noIdle: true})
	f.sa.disableAutoSummarize = false
	model := f.model
	sol := solModel(t)
	model.CatwalkCfg, model.ModelCfg = sol.CatwalkCfg, sol.ModelCfg
	ts := guardPrepareStream(t, f, model, prompt)
	_, _, err := ts.prepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: msgs})
	require.NoError(t, err)
	return ts
}

// headroomToolTail is user, assistant tool call, then a tool result of the
// given size; assistantAfter appends the assistant reply that reports it.
func headroomToolTail(chars int, assistantAfter bool) []fantasy.Message {
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("start"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "read", Input: "{}"},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("x", chars)}},
		}},
	}
	if assistantAfter {
		msgs = append(msgs, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "done"}}})
	}
	return msgs
}

func TestPrepareStepGPT61SolHeadroomBoundary(t *testing.T) {
	// Ends with an assistant reply, so no unreported tail is added.
	small := []fantasy.Message{
		fantasy.NewUserMessage("hello"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi"}}},
	}
	ts := headroomPrepare(t, "headroom-below", solGateUsage-1, small)
	require.False(t, ts.silentCompactNeeded, "remaining 46,101 > headroom 46,100: no compaction")
	ts = headroomPrepare(t, "headroom-at", solGateUsage, small)
	require.True(t, ts.silentCompactNeeded, "remaining 46,100 == headroom: silent compaction requested")
}

// The previous step's usage cannot include tool output appended after it. With
// 800,000 reported, remaining is 122,000 and the gate fires once the unreported
// tail estimate (ceil(chars/3)) reaches 75,900 tokens = 227,700 chars.
func TestPrepareStepCountsUnreportedToolOutput(t *testing.T) {
	const reported int64 = 800_000
	ts := headroomPrepare(t, "tail-below", reported, headroomToolTail(227_697, false))
	require.False(t, ts.silentCompactNeeded, "tail estimate 75,899 tokens leaves remaining 46,101")
	ts = headroomPrepare(t, "tail-at", reported, headroomToolTail(227_700, false))
	require.True(t, ts.silentCompactNeeded, "tail estimate 75,900 tokens leaves remaining 46,100")
}

// Content before the last assistant message was reported by the previous step.
func TestPrepareStepIgnoresToolOutputBeforeLastAssistant(t *testing.T) {
	ts := headroomPrepare(t, "tail-reported", 800_000, headroomToolTail(600_000, true))
	require.False(t, ts.silentCompactNeeded, "tool output already followed by an assistant reply is in the usage report")
}

func TestUnreportedTailTokens(t *testing.T) {
	t.Parallel()
	require.Equal(t, int64(0), unreportedTailTokens(nil))
	require.Equal(t, int64(0), unreportedTailTokens(headroomToolTail(900, true)))
	require.Equal(t, int64(300), unreportedTailTokens(headroomToolTail(900, false)))
	require.Equal(t, int64(301), unreportedTailTokens(headroomToolTail(901, false)))
}
