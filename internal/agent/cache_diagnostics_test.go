package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

type cacheSchemaTool struct {
	fantasy.AgentTool
	info fantasy.ToolInfo
}

func (t cacheSchemaTool) Info() fantasy.ToolInfo { return t.info }

func TestCachePrefix_FinalAdmittedBatchToolsCanonical(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	bindBatchInventoryControls(t, coord)
	cfg := coord.cfg.Config()
	agentCfg := cfg.Agents[config.AgentCoder]
	agentCfg.AllowedMCP = map[string][]string{}
	build := func(allowed []string) []fantasy.AgentTool {
		t.Helper()
		agentCfg.AllowedTools = allowed
		opts := &CallOptions{DisableSubAgents: true}
		scope := EnsureCallBuildScope(opts)
		defer scope.Close()
		ctx := WithCallOptions(batchCallCtx("cache-final-admitted"), opts)
		got, err := coord.buildTools(ctx, cfg, agentCfg, false)
		require.NoError(t, err)
		return got
	}
	allowed := []string{"view", "todos", "batch_stop", "batch_output", "batch_status"}
	forward := build(allowed)
	reverseAllowed := slices.Clone(allowed)
	slices.Reverse(reverseAllowed)
	reverse := build(reverseAllowed)
	want := []string{"batch_add", "batch_output", "batch_run", "batch_status", "batch_stop", "todos", "view"}
	require.Equal(t, want, pinnedToolNames(forward), "sorting the earlier placeholders is insufficient: installed controls must participate")
	require.Equal(t, want, pinnedToolNames(reverse))
	for i := range forward {
		require.Equal(t, forward[i].Info(), reverse[i].Info())
	}
	messages := []fantasy.Message{fantasy.NewSystemMessage("stable prefix")}
	first := preparedCacheDiagnostic(messages, withProviderOptionsOnLast(forward, cacheOpts()))
	second := preparedCacheDiagnostic(messages, withProviderOptionsOnLast(reverse, cacheOpts()))
	require.Equal(t, first, second, "the fingerprint must describe the final marked schema, including controls")
	for _, tool := range forward {
		require.Empty(t, tool.ProviderOptions(), "diagnostics and markers must not mutate the pinned tools")
	}
	narrowed := build([]string{"view"})
	require.Equal(t, []string{"batch_add", "batch_run", "view"}, pinnedToolNames(narrowed))
	require.NotEqual(t, first.ToolsFingerprint, preparedCacheDiagnostic(messages, narrowed).ToolsFingerprint, "authority changes must change the observed schema")
}

func TestCachePrefix_PrepareStepDoesNotMutateRetainedMessagesOrPinnedTools(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	t.Setenv("RUSH_DISABLE_ANTHROPIC_CACHE", "false")
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	f := newAttemptFixture(t, "cache-step-clone", attemptFixtureOpts{noIdle: true})
	ts := guardPrepareStream(t, f, f.model, 0)
	ts.promptPrefix = "prefix pinned to this call"
	ts.call.Tools = []fantasy.AgentTool{newProviderOptionsTestTool("zeta"), newProviderOptionsTestTool("alpha")}
	messages := []fantasy.Message{fantasy.NewSystemMessage("system"), fantasy.NewUserMessage("request")}
	messages[0].ProviderOptions = cacheOpts()
	before := cloneFantasyMessages(messages)
	_, prepared, err := ts.prepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: messages})
	require.NoError(t, err)
	require.Equal(t, before, messages, "per-step cache marker clearing must not write through fantasy's retained slice")
	require.Equal(t, []string{"zeta", "alpha"}, pinnedToolNames(prepared.Tools), "explicit pinned ordering must survive preparation")
	require.Empty(t, ts.call.Tools[1].ProviderOptions())
	require.NotEmpty(t, prepared.Tools[1].ProviderOptions())
	require.Equal(t, "prefix pinned to this call", prepared.Messages[0].Content[0].(fantasy.TextPart).Text)
	wantDiagnostic := preparedCacheDiagnostic(prepared.Messages, prepared.Tools)
	require.Equal(t, wantDiagnostic, preparedCacheDiagnostic(ts.stepMessages, ts.stepTools))
	require.Equal(t, wantDiagnostic, ts.cacheDiagnostic, "the diagnostic must be captured after all step assembly")
	output.Reset()
	_, err = ts.applyStepUsage(fantasy.StepResult{Response: fantasy.Response{Usage: fantasy.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}}})
	require.NoError(t, err)
	var observation map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var row map[string]any
		require.NoError(t, json.Unmarshal(line, &row))
		if row["msg"] == "agent: prompt cache observation" {
			observation = row
		}
	}
	require.NotNil(t, observation, "applyStepUsage must emit the prepared diagnostic")
	require.Equal(t, wantDiagnostic.ToolsFingerprint, observation["tools_fingerprint"])
	require.Equal(t, wantDiagnostic.PrefixFingerprint, observation["prefix_fingerprint"])
	require.Nil(t, observation["cached_tokens"], "a zero counter without presence evidence remains unknown")
}

func TestCacheDiagnostic_MapPermutationsPreserveArraysAndSemantics(t *testing.T) {
	mkTool := func(parameters map[string]any, required []string) fantasy.AgentTool {
		return cacheSchemaTool{AgentTool: newProviderOptionsTestTool("fixture"), info: fantasy.ToolInfo{
			Name: "fixture", Description: "schema", Parameters: parameters, Required: required,
		}}
	}
	first := mkTool(map[string]any{"z": map[string]any{"enum": []string{"first", "second"}, "type": "string"}, "a": map[string]any{"type": "number"}}, []string{"z", "a"})
	permuted := mkTool(map[string]any{"a": map[string]any{"type": "number"}, "z": map[string]any{"type": "string", "enum": []string{"first", "second"}}}, []string{"z", "a"})
	messages := []fantasy.Message{fantasy.NewSystemMessage("system-secret"), fantasy.NewUserMessage("request-secret")}
	baseline := preparedCacheDiagnostic(messages, []fantasy.AgentTool{first})
	require.Equal(t, baseline, preparedCacheDiagnostic(messages, []fantasy.AgentTool{permuted}))
	reorderedArray := mkTool(map[string]any{"a": map[string]any{"type": "number"}, "z": map[string]any{"type": "string", "enum": []string{"second", "first"}}}, []string{"z", "a"})
	require.NotEqual(t, baseline.ToolsFingerprint, preparedCacheDiagnostic(messages, []fantasy.AgentTool{reorderedArray}).ToolsFingerprint)
	changedSchema := mkTool(map[string]any{"a": map[string]any{"type": "boolean"}, "z": map[string]any{"type": "string", "enum": []string{"first", "second"}}}, []string{"z", "a"})
	require.NotEqual(t, baseline.ToolsFingerprint, preparedCacheDiagnostic(messages, []fantasy.AgentTool{changedSchema}).ToolsFingerprint)
	changedRequired := mkTool(first.Info().Parameters, []string{"z"})
	require.NotEqual(t, baseline.ToolsFingerprint, preparedCacheDiagnostic(messages, []fantasy.AgentTool{changedRequired}).ToolsFingerprint)
	require.Equal(t, []string{"z", "a"}, first.Info().Required)
	changedHistory := []fantasy.Message{messages[0], fantasy.NewUserMessage("different request")}
	require.Equal(t, baseline.PrefixFingerprint, preparedCacheDiagnostic(changedHistory, []fantasy.AgentTool{first}).PrefixFingerprint)
	changedPrefix := []fantasy.Message{fantasy.NewSystemMessage("different system"), messages[1]}
	require.NotEqual(t, baseline.PrefixFingerprint, preparedCacheDiagnostic(changedPrefix, []fantasy.AgentTool{first}).PrefixFingerprint)
	require.Greater(t, baseline.MessageBytes, baseline.PrefixBytes)
}

func TestCacheDiagnostic_AmbiguousZeroAndContentFreeLog(t *testing.T) {
	require.Nil(t, observedCacheReadTokens(fantasy.Usage{}, false, "", nil), "fantasy.Usage alone cannot distinguish omitted cached_tokens from explicit zero")
	require.Nil(t, observedCacheReadTokens(fantasy.Usage{CacheReadTokens: 10}, true, "", nil))
	require.EqualValues(t, 10, *observedCacheReadTokens(fantasy.Usage{CacheReadTokens: 10}, false, "", nil))
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	messages := []fantasy.Message{fantasy.NewSystemMessage("system-secret"), fantasy.NewUserMessage("arguments-secret")}
	ts := &turnStream{cacheDiagnostic: preparedCacheDiagnostic(messages, nil), cachePreparedAt: time.Now()}
	ts.logCacheStepUsage(fantasy.Usage{}, false, nil)
	var row map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &row))
	require.Contains(t, row, "cached_tokens")
	require.Nil(t, row["cached_tokens"])
	require.NotContains(t, output.String(), "system-secret")
	require.NotContains(t, output.String(), "arguments-secret")
	require.Contains(t, row["byte_measurement"], "not wire bytes")
	require.Contains(t, row["semantic_invalidation_expectation"], "do not identify the cause")
}

func obs(model, prefix, tools string, digests ...string) *cacheRequestObs {
	return &cacheRequestObs{prefix: prefix, tools: tools, model: model, digests: digests, at: time.Now()}
}

func TestClassifyCacheMiss(t *testing.T) {
	base := obs("p/m", "pre", "tools", "a", "b", "c")
	tests := []struct {
		name       string
		prev, cur  *cacheRequestObs
		wantCause  string
		wantCommon int
	}{
		{"first request", nil, obs("p/m", "pre", "tools", "a"), cacheMissFirstRequest, 0},
		{"history only grew", base, obs("p/m", "pre", "tools", "a", "b", "c", "d"), cacheMissProviderSide, 3},
		{"model changed", base, obs("p/other", "pre", "tools", "a", "b", "c", "d"), cacheMissModelChanged, 3},
		{"system prefix changed", base, obs("p/m", "pre2", "tools", "a", "b", "c", "d"), cacheMissPrefixChanged, 3},
		{"tools changed", base, obs("p/m", "pre", "tools2", "a", "b", "c", "d"), cacheMissToolsChanged, 3},
		{"history rewritten in the middle", base, obs("p/m", "pre", "tools", "a", "x", "c", "d"), cacheMissHistoryChanged, 1},
		{"history truncated", base, obs("p/m", "pre", "tools", "a", "b"), cacheMissHistoryChanged, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cause, common := classifyCacheMiss(tc.prev, tc.cur)
			require.Equal(t, tc.wantCause, cause)
			require.Equal(t, tc.wantCommon, common)
		})
	}
}

// The per-message digests must move only where the message moved, otherwise the
// divergence index in the log line is meaningless.
func TestPreparedCacheDiagnostic_MessageDigestsLocateTheChange(t *testing.T) {
	msgs := func(texts ...string) []fantasy.Message {
		out := make([]fantasy.Message, len(texts))
		for i, s := range texts {
			role := fantasy.MessageRoleUser
			if i == 0 {
				role = fantasy.MessageRoleSystem
			}
			out[i] = fantasy.Message{Role: role, Content: []fantasy.MessagePart{fantasy.TextPart{Text: s}}}
		}
		return out
	}
	a := preparedCacheDiagnostic(msgs("sys", "one", "two", "three"), nil)
	b := preparedCacheDiagnostic(msgs("sys", "one", "TWO", "three"), nil)
	require.Len(t, a.MessageDigests, 4)
	require.Equal(t, 2, commonPrefixLen(a.MessageDigests, b.MessageDigests))
	grown := preparedCacheDiagnostic(msgs("sys", "one", "two", "three", "four"), nil)
	require.Equal(t, 4, commonPrefixLen(a.MessageDigests, grown.MessageDigests))
}

func TestSwapCacheObs_BoundedAndReturnsPrevious(t *testing.T) {
	cacheObs.mu.Lock()
	cacheObs.last = map[string]*cacheRequestObs{}
	cacheObs.mu.Unlock()
	first := obs("p/m", "p", "t", "a")
	require.Nil(t, swapCacheObs("s", first))
	require.Same(t, first, swapCacheObs("s", obs("p/m", "p", "t", "a", "b")))
	for i := 0; i < cacheObsMaxSessions+40; i++ {
		swapCacheObs("k"+strconv.Itoa(i), obs("p/m", "p", "t"))
	}
	cacheObs.mu.Lock()
	n := len(cacheObs.last)
	cacheObs.mu.Unlock()
	require.LessOrEqual(t, n, cacheObsMaxSessions+1)
}
