package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type codexUsageTransport struct {
	payload string
	calls   atomic.Int64
}

func (f *codexUsageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	f.calls.Add(1)
	if request.Body != nil {
		_ = request.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(f.payload)),
		Request:    request,
	}, nil
}

func TestCodexCacheAccountingWireToStoredUsage(t *testing.T) {
	for _, method := range []string{"generate", "stream"} {
		for _, cached := range []int64{0, 90, 100} {
			t.Run(fmt.Sprintf("%s/cached-%d", method, cached), func(t *testing.T) {
				transport := &codexUsageTransport{payload: fmt.Sprintf(
					"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"fixture reply\"}]}],\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"total_tokens\":120,\"input_tokens_details\":{\"cached_tokens\":%d}}}}\n\n", cached)}
				var decoded, normalized fantasy.Usage
				t.Cleanup(func() {
					if t.Failed() {
						t.Logf("Codex usage fixture: method=%s cached=%d requests=%d decoded=%+v normalized=%+v", method, cached, transport.calls.Load(), decoded, normalized)
					}
				})
				provider, err := codexprovider.New(&http.Client{Transport: transport}, &oauth.Token{AccessToken: "fixture-only"})
				require.NoError(t, err)
				lm, err := provider.LanguageModel(t.Context(), "cache-fixture")
				require.NoError(t, err)
				require.Equal(t, codexprovider.Name, lm.Provider())
				profile := cacheProfileFor(lm.Provider())
				require.False(t, profile.inputIncludesCache)
				require.False(t, profile.explicitMarkers)
				require.False(t, profile.keepAliveEligible)
				require.Zero(t, profile.ttl)
				call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("fixture input")}}
				if method == "generate" {
					response, err := lm.Generate(t.Context(), call)
					require.NoError(t, err)
					decoded = response.Usage
				} else {
					stream, err := lm.Stream(t.Context(), call)
					require.NoError(t, err)
					finished := false
					for part := range stream {
						if part.Type == fantasy.StreamPartTypeError {
							require.NoError(t, part.Error)
						}
						if part.Type == fantasy.StreamPartTypeFinish {
							finished = true
							decoded = part.Usage
						}
					}
					require.True(t, finished)
				}
				require.EqualValues(t, 1, transport.calls.Load())
				require.Equal(t, fantasy.Usage{InputTokens: 100 - cached, CacheReadTokens: cached, OutputTokens: 20, TotalTokens: 120}, decoded)
				normalized = normalizeProviderUsage(lm.Provider(), decoded)
				require.Equal(t, decoded, normalized, "agent accounting must not subtract cached input a second time")
				require.EqualValues(t, 120, addStepUsageTokens(0, normalized))

				env := testEnv(t)
				sess, err := env.sessions.Create(t.Context(), "Codex cache accounting")
				require.NoError(t, err)
				a := &sessionAgent{messages: env.messages}
				model := Model{
					Model:      lm,
					ModelCfg:   config.SelectedModel{Provider: "fixture-codex", Model: "cache-fixture"},
					CatwalkCfg: catwalk.Model{CostPer1MIn: 2, CostPer1MOut: 10, CostPer1MOutCached: 0.2},
				}
				cost := a.updateSessionUsage(model, &sess, normalized, nil)
				wantCost := float64(100-cached)*2/1e6 + float64(cached)*0.2/1e6 + 20*10.0/1e6
				require.InDelta(t, wantCost, cost, 1e-12)
				require.EqualValues(t, 100, sess.PromptTokens)
				require.EqualValues(t, 20, sess.CompletionTokens)
				msg, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
					Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "fixture reply"}},
				})
				require.NoError(t, err)
				a.recordMessageUsage(t.Context(), msg.ID, model, normalized, cost, false)
				stored, err := env.messages.Get(t.Context(), msg.ID)
				require.NoError(t, err)
				require.NotNil(t, stored.Usage)
				require.EqualValues(t, 100-cached, stored.Usage.InputTokens)
				require.Equal(t, cached, stored.Usage.CacheReadTokens)
				require.EqualValues(t, 120, stored.Usage.TotalTokens)
				require.Equal(t, codexprovider.Name, stored.Usage.Provider)
				require.InDelta(t, wantCost, stored.Usage.CostUSD, 1e-12)
				require.Equal(t, message.CacheSupportNative, stored.Usage.CacheSupport, "a cold Codex response is a measured miss, not n/a")
				ratio, ok := stored.Usage.CacheHitRatio()
				require.True(t, ok)
				require.InDelta(t, float64(cached)/100, ratio, 1e-12)
				flat := model
				flat.FlatRate = true
				require.Zero(t, a.updateSessionUsage(flat, &sess, normalized, nil))
			})
		}
	}
}
