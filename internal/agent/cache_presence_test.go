package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func cachePresenceValue(value int64) *int64 { return &value }

func TestCacheReadPresenceRequiresNativeEvidenceAndRejectsEstimates(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		usage    fantasy.Usage
		metadata fantasy.ProviderMetadata
		estimate bool
		want     *int64
	}{
		{"native omitted", codexprovider.Name, fantasy.Usage{}, nil, false, nil},
		{"native typed nil", codexprovider.Name, fantasy.Usage{}, fantasy.ProviderMetadata{codexprovider.Name: (*codexprovider.UsageMetadata)(nil)}, false, nil},
		{"native null", codexprovider.Name, fantasy.Usage{}, fantasy.ProviderMetadata{codexprovider.Name: &codexprovider.UsageMetadata{}}, false, nil},
		{"native explicit zero", codexprovider.Name, fantasy.Usage{}, fantasy.ProviderMetadata{codexprovider.Name: &codexprovider.UsageMetadata{CachedTokens: cachePresenceValue(0)}}, false, cachePresenceValue(0)},
		{"known reporter is not evidence", "openai", fantasy.Usage{}, nil, false, nil},
		{"foreign typed metadata", "openai", fantasy.Usage{}, fantasy.ProviderMetadata{codexprovider.Name: &codexprovider.UsageMetadata{CachedTokens: cachePresenceValue(0)}}, false, nil},
		{"wrong namespace", codexprovider.Name, fantasy.Usage{}, fantasy.ProviderMetadata{"other": &codexprovider.UsageMetadata{CachedTokens: cachePresenceValue(0)}}, false, nil},
		{"stale nonzero metadata", codexprovider.Name, fantasy.Usage{}, fantasy.ProviderMetadata{codexprovider.Name: &codexprovider.UsageMetadata{CachedTokens: cachePresenceValue(7)}}, false, nil},
		{"estimated zero", codexprovider.Name, fantasy.Usage{}, fantasy.ProviderMetadata{codexprovider.Name: &codexprovider.UsageMetadata{CachedTokens: cachePresenceValue(0)}}, true, nil},
		{"estimated nonzero", codexprovider.Name, fantasy.Usage{CacheReadTokens: 7}, fantasy.ProviderMetadata{codexprovider.Name: &codexprovider.UsageMetadata{CachedTokens: cachePresenceValue(7)}}, true, nil},
		{"observed nonzero", codexprovider.Name, fantasy.Usage{CacheReadTokens: 7}, nil, false, cachePresenceValue(7)},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := maps.Clone(test.metadata)
			require.Equal(t, test.want, observedCacheReadTokens(test.usage, test.estimate, test.provider, test.metadata))
			require.Equal(t, before, test.metadata, "diagnostics must not mutate provider metadata")
		})
	}
}

type cachePresenceHTTPTransport func(*http.Request) (*http.Response, error)

func (f cachePresenceHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// This wrapper adds an unrelated finish field to test Fantasy's map forwarding;
// it never supplies or modifies the native decoder's cached-token evidence.
type cachePresenceOtherMetadataModel struct{ fantasy.LanguageModel }

func (m cachePresenceOtherMetadataModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeFinish {
				metadata := maps.Clone(part.ProviderMetadata)
				if metadata == nil {
					metadata = fantasy.ProviderMetadata{}
				}
				metadata["fixture-existing"] = &codexprovider.UsageMetadata{}
				part.ProviderMetadata = metadata
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func TestNativeCachePresenceThroughFantasyStepNormalizationAndDiagnostic(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	frames := []string{
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"one","type":"function_call","call_id":"call-one","name":"cache_probe","arguments":"{}"}],"usage":{"input_tokens":100,"output_tokens":2,"total_tokens":102,"input_tokens_details":{"cached_tokens":0}}}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"cached","type":"function_call","call_id":"call-cached","name":"cache_probe","arguments":"{}"}],"usage":{"input_tokens":100,"output_tokens":2,"total_tokens":102,"input_tokens_details":{"cached_tokens":7}}}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"two","type":"function_call","call_id":"call-two","name":"cache_probe","arguments":"{}"}],"usage":{"input_tokens":100,"output_tokens":2,"total_tokens":102}}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens_details":{"cached_tokens":0}}}}`,
	}
	var requests atomic.Int32
	client := &http.Client{Transport: cachePresenceHTTPTransport(func(r *http.Request) (*http.Response, error) {
		index := int(requests.Add(1)) - 1
		if index >= len(frames) {
			return nil, errors.New("unexpected extra provider attempt")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + frames[index] + "\n\n")), Request: r}, nil
	})}
	provider, err := codexprovider.New(client, &oauth.Token{AccessToken: "inert-cache-presence-fixture"})
	require.NoError(t, err)
	native, err := provider.LanguageModel(t.Context(), "presence-fixture")
	require.NoError(t, err)
	model := cachePresenceOtherMetadataModel{LanguageModel: native}
	f := newAttemptFixture(t, "cache-presence-pipeline", attemptFixtureOpts{noIdle: true})
	executing := f.model
	executing.Model = model
	executing.ModelCfg.Provider = "configured-alias-not-native-identity"
	ts := guardPrepareStream(t, f, executing, 0)
	probe := fantasy.NewAgentTool("cache_probe", "Presence fixture tool", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	ts.call.Tools = []fantasy.AgentTool{probe}
	runner := fantasy.NewAgent(model, fantasy.WithTools(probe), fantasy.WithStopConditions(fantasy.StepCountIs(4)))
	var steps []fantasy.StepResult
	maxRetries := 0
	_, err = runner.Stream(t.Context(), fantasy.AgentStreamCall{
		MaxRetries:  &maxRetries,
		Messages:    []fantasy.Message{fantasy.NewUserMessage("observe cache presence")},
		PrepareStep: ts.prepareStep,
		OnStepFinish: func(step fantasy.StepResult) error {
			steps = append(steps, step)
			_, err := ts.applyStepUsage(step)
			require.NoError(t, err)
			ts.mu.Lock()
			assistantID := ts.currentAssistant.ID
			ts.mu.Unlock()
			stored, err := f.env.messages.Get(t.Context(), assistantID)
			require.NoError(t, err)
			require.NotNil(t, stored.Usage)
			if !usageIsZero(step.Usage) {
				require.Equal(t, step.Usage.InputTokens, stored.Usage.InputTokens, "presence metadata must not change normalized billing input")
				require.Equal(t, step.Usage.CacheReadTokens, stored.Usage.CacheReadTokens)
			} else {
				require.True(t, stored.Usage.Estimated)
			}
			return err
		},
	})
	require.NoError(t, err)
	require.Len(t, steps, 4)
	require.EqualValues(t, 4, requests.Load())
	for _, step := range steps {
		require.Contains(t, step.ProviderMetadata, "fixture-existing", "normalization/diagnostics must preserve unrelated finish metadata")
	}
	first, ok := steps[0].ProviderMetadata[codexprovider.Name].(*codexprovider.UsageMetadata)
	require.True(t, ok)
	require.Equal(t, cachePresenceValue(0), first.CachedTokens)
	require.NotContains(t, steps[2].ProviderMetadata, codexprovider.Name, "an omitted field on the next step cannot inherit prior evidence")
	require.Equal(t, fantasy.Usage{InputTokens: 100, OutputTokens: 2, TotalTokens: 102}, steps[0].Usage)
	require.Equal(t, fantasy.Usage{InputTokens: 93, CacheReadTokens: 7, OutputTokens: 2, TotalTokens: 102}, steps[1].Usage)
	require.Equal(t, steps[0].Usage, steps[2].Usage, "presence alone must not change usage counters")
	var observations []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var row map[string]any
		require.NoError(t, json.Unmarshal(line, &row))
		if row["msg"] == "agent: prompt cache observation" {
			observations = append(observations, row)
		}
	}
	require.Len(t, observations, 4)
	for _, observation := range observations {
		require.Contains(t, observation, "cached_tokens", "unknown must be represented as null, not a missing diagnostic")
	}
	require.Equal(t, float64(0), observations[0]["cached_tokens"], "actual parser evidence must distinguish explicit zero")
	require.Equal(t, float64(7), observations[1]["cached_tokens"], "normalization must preserve the observed nonzero count")
	require.Nil(t, observations[2]["cached_tokens"], "omitted cached_tokens remains unknown")
	require.Nil(t, observations[3]["cached_tokens"], "estimated usage cannot prove cache reads, even with explicit-zero metadata")
	require.Equal(t, true, observations[3]["estimated"])
}
