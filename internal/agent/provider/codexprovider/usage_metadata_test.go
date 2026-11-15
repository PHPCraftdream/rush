package codexprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func cachePresenceCounter(value int64) *int64 { return &value }

func nativeCacheMetadata(t *testing.T, metadata fantasy.ProviderMetadata) *int64 {
	t.Helper()
	if metadata[Name] == nil {
		return nil
	}
	observed, ok := metadata[Name].(*UsageMetadata)
	require.True(t, ok, "native decoder evidence must retain its concrete type")
	require.NotNil(t, observed)
	return observed.CachedTokens
}

func TestNativeCachedTokensPresenceHTTPGenerateAndRawFrameParity(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, test := range []struct {
		name    string
		details string
		want    *int64
	}{
		{"omitted", "", nil},
		{"empty details", `,"input_tokens_details":{}`, nil},
		{"null details", `,"input_tokens_details":null`, nil},
		{"null counter", `,"input_tokens_details":{"cached_tokens":null}`, nil},
		{"explicit zero", `,"input_tokens_details":{"cached_tokens":0}`, cachePresenceCounter(0)},
		{"nonzero", `,"input_tokens_details":{"cached_tokens":7}`, cachePresenceCounter(7)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"reply"}]}],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120` + test.details + `}}`
			frame := `{"type":"response.completed","response":` + response + `}`
			decoded, err := decodeResponse(json.RawMessage(response))
			require.NoError(t, err)
			require.Equal(t, test.want, nativeCacheMetadata(t, decoded.metadata))
			cached := int64(0)
			if test.want != nil {
				cached = *test.want
			}
			require.Equal(t, fantasy.Usage{InputTokens: 100 - cached, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: cached}, decoded.usage, "presence must not alter billing counters")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeCodexEvent(w, frame)
			}))
			t.Cleanup(server.Close)
			model := codexProviderForTest(t, server)
			stream, err := model.Stream(t.Context(), fantasy.Call{})
			require.NoError(t, err)
			var httpFinish fantasy.StreamPart
			for part := range stream {
				require.NotEqual(t, fantasy.StreamPartTypeError, part.Type, "unexpected stream error: %v", part.Error)
				if part.Type == fantasy.StreamPartTypeFinish {
					httpFinish = part
				}
			}
			require.Equal(t, fantasy.StreamPartTypeFinish, httpFinish.Type)
			require.Equal(t, test.want, nativeCacheMetadata(t, httpFinish.ProviderMetadata))
			// The WS adapter uses this same consume/finish pipeline. This proves
			// full-frame decoder parity, not a network WebSocket exchange.
			state := newStreamState()
			var frameFinish fantasy.StreamPart
			yield := func(part fantasy.StreamPart) bool {
				if part.Type == fantasy.StreamPartTypeFinish {
					frameFinish = part
				}
				return true
			}
			require.NoError(t, state.consume([]byte(frame), yield))
			require.True(t, state.completed)
			state.finish(yield)
			require.Equal(t, httpFinish, frameFinish)
			generated, err := model.Generate(t.Context(), fantasy.Call{})
			require.NoError(t, err)
			require.Equal(t, httpFinish.ProviderMetadata, generated.ProviderMetadata, "Generate must not drop finish metadata")
			require.Equal(t, httpFinish.Usage, generated.Usage)
		})
	}
}

func TestNativeCachedTokensPresenceClearsAcrossResponsesAndFailedAttempts(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	known := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}}}`
	unknown := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":1}}}`
	state := newStreamState()
	yield := func(fantasy.StreamPart) bool { return true }
	require.NoError(t, state.consume([]byte(known), yield))
	require.Equal(t, cachePresenceCounter(0), nativeCacheMetadata(t, state.metadata))
	require.NoError(t, state.consume([]byte(unknown), yield))
	require.Nil(t, nativeCacheMetadata(t, state.metadata), "a later omitted field must replace, not inherit, prior evidence")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests.Add(1) {
		case 1:
			writeCodexEvent(w, known)
		case 2:
			writeCodexEvent(w, known)
			writeCodexEvent(w, `{"type":"error","error":{"message":"failed attempt fixture"}}`)
		default:
			writeCodexEvent(w, unknown)
		}
	}))
	t.Cleanup(server.Close)
	model := codexProviderForTest(t, server)
	first, err := model.Generate(context.Background(), fantasy.Call{})
	require.NoError(t, err)
	require.Equal(t, cachePresenceCounter(0), nativeCacheMetadata(t, first.ProviderMetadata))
	failed, err := model.Generate(context.Background(), fantasy.Call{})
	require.Error(t, err)
	require.Nil(t, failed, "a failed attempt cannot publish its cached-token evidence")
	next, err := model.Generate(context.Background(), fantasy.Call{})
	require.NoError(t, err)
	require.Nil(t, nativeCacheMetadata(t, next.ProviderMetadata))
	require.EqualValues(t, 3, requests.Load())
}

func TestUsageMetadataJSONPresenceDoesNotReuseOldEvidence(t *testing.T) {
	var metadata UsageMetadata
	for _, test := range []struct {
		wire string
		want *int64
	}{
		{`{"cached_tokens":0}`, cachePresenceCounter(0)},
		{`{}`, nil},
		{`{"cached_tokens":7}`, cachePresenceCounter(7)},
		{`{"cached_tokens":null}`, nil},
	} {
		require.NoError(t, json.Unmarshal([]byte(test.wire), &metadata))
		require.Equal(t, test.want, metadata.CachedTokens)
		encoded, err := json.Marshal(&metadata)
		require.NoError(t, err)
		var replay UsageMetadata
		require.NoError(t, json.Unmarshal(encoded, &replay))
		require.Equal(t, metadata, replay)
	}
}
