package codexprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type cachePresenceNetworkCase struct {
	name    string
	details string
	usage   bool
	want    fantasy.Usage
	cached  *int64
}

type cachePresenceRequestCapture struct {
	methodOK             bool
	authOK               bool
	betaOK               bool
	accountOK            bool
	conversationOK       bool
	sessionOK            bool
	clientRequestIDOK    bool
	modelOK              bool
	requestCreateOK      bool
	streamOK             bool
	storeOK              bool
	noPreviousResponseID bool
	noBackground         bool
}

type cachePresenceNetworkFixture struct {
	server        *httptest.Server
	httpRequests  atomic.Int32
	wsRequests    atomic.Int32
	upgrades      atomic.Int32
	captureErrors atomic.Int32
	httpCaptures  chan cachePresenceRequestCapture
	wsCaptures    chan cachePresenceRequestCapture
	httpEvents    []string
	wsEvents      []string
}

func newCachePresenceNetworkFixture(t *testing.T, httpEvents, wsEvents []string) *cachePresenceNetworkFixture {
	t.Helper()
	fixture := &cachePresenceNetworkFixture{
		httpCaptures: make(chan cachePresenceRequestCapture, 64),
		wsCaptures:   make(chan cachePresenceRequestCapture, 64),
		httpEvents:   httpEvents,
		wsEvents:     wsEvents,
	}
	fixture.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			index := int(fixture.httpRequests.Add(1) - 1)
			body := make(map[string]json.RawMessage)
			decodeOK := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) == nil
			capture := cachePresenceRequestCapture{}
			if decodeOK {
				capture = summarizeCachePresenceRequest(r, body, false)
			}
			select {
			case fixture.httpCaptures <- capture:
			case <-r.Context().Done():
				return
			}
			if !decodeOK {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			writeCodexEvent(w, cachePresenceEventAt(fixture.httpEvents, index))
			return
		}

		fixture.upgrades.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			fixture.captureErrors.Add(1)
			return
		}
		defer conn.Close()
		conn.SetReadLimit(1 << 20)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, frame, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			index := int(fixture.wsRequests.Add(1) - 1)
			body := make(map[string]json.RawMessage)
			if json.Unmarshal(frame, &body) != nil {
				fixture.captureErrors.Add(1)
			} else {
				capture := summarizeCachePresenceRequest(r, body, true)
				select {
				case fixture.wsCaptures <- capture:
				case <-r.Context().Done():
					return
				}
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if writeErr := conn.WriteMessage(websocket.TextMessage, []byte(cachePresenceEventAt(fixture.wsEvents, index))); writeErr != nil {
				fixture.captureErrors.Add(1)
				return
			}
		}
	}))
	fixture.server.Config.ReadTimeout = 10 * time.Second
	fixture.server.Config.WriteTimeout = 10 * time.Second
	fixture.server.Config.IdleTimeout = 10 * time.Second
	fixture.server.Start()
	t.Cleanup(fixture.server.Close)
	return fixture
}

func summarizeCachePresenceRequest(r *http.Request, body map[string]json.RawMessage, ws bool) cachePresenceRequestCapture {
	var model string
	_ = json.Unmarshal(body["model"], &model)
	requestTypeOK := !ws && len(body["type"]) == 0
	if ws {
		var kind string
		_ = json.Unmarshal(body["type"], &kind)
		requestTypeOK = kind == "response.create"
	}
	return cachePresenceRequestCapture{
		methodOK:             (ws && r.Method == http.MethodGet) || (!ws && r.Method == http.MethodPost),
		authOK:               (r.Header.Get("Authorization") == "Bearer chatgpt-oauth-access-token" && !ws) || (r.Header.Get("Authorization") == "Bearer old-access" && ws),
		betaOK:               (r.Header.Get("OpenAI-Beta") == "responses=experimental" && !ws) || (r.Header.Get("OpenAI-Beta") == "responses_websockets=2026-02-06" && ws),
		accountOK:            (r.Header.Get("chatgpt-account-id") == "workspace-account-42" && !ws) || (r.Header.Get("chatgpt-account-id") == "old-account" && ws),
		conversationOK:       r.Header.Get("conversation_id") == "codex-ws-session",
		sessionOK:            r.Header.Get("session_id") == "codex-ws-session",
		clientRequestIDOK:    r.Header.Get("x-client-request-id") == "codex-ws-session",
		modelOK:              model == "gpt-5-codex",
		requestCreateOK:      requestTypeOK,
		streamOK:             cachePresenceJSONBool(body["stream"], true),
		storeOK:              cachePresenceJSONBool(body["store"], false),
		noPreviousResponseID: len(body["previous_response_id"]) == 0,
		noBackground:         len(body["background"]) == 0,
	}
}

func cachePresenceJSONBool(raw json.RawMessage, want bool) bool {
	return string(raw) == map[bool]string{true: "true", false: "false"}[want]
}

func cachePresenceEventAt(events []string, index int) string {
	if index >= 0 && index < len(events) {
		return events[index]
	}
	return `{"type":"response.completed","response":{"status":"completed"}}`
}

func cachePresenceCompletion(test cachePresenceNetworkCase) string {
	response := `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"reply"}]}]}`
	if test.usage {
		response = strings.TrimSuffix(response, "}") + `,"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120` + test.details + `}}`
	}
	return `{"type":"response.completed","response":` + response + `}`
}

func cachePresenceCases() []cachePresenceNetworkCase {
	return []cachePresenceNetworkCase{
		{name: "explicit zero", details: `,"input_tokens_details":{"cached_tokens":0}`, usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: 0}, cached: cachePresenceCounter(0)},
		{name: "positive", details: `,"input_tokens_details":{"cached_tokens":7}`, usage: true, want: fantasy.Usage{InputTokens: 93, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: 7}, cached: cachePresenceCounter(7)},
		{name: "omitted details", usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}},
		{name: "empty details", details: `,"input_tokens_details":{}`, usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}},
		{name: "null details", details: `,"input_tokens_details":null`, usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}},
		{name: "null cached counter", details: `,"input_tokens_details":{"cached_tokens":null}`, usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}},
		{name: "usage omitted", want: fantasy.Usage{}},
	}
}

func cachePresenceRepeatedEvents(cases []cachePresenceNetworkCase) []string {
	events := make([]string, 0, len(cases)*2)
	for _, test := range cases {
		event := cachePresenceCompletion(test)
		events = append(events, event, event)
	}
	return events
}

func cachePresenceCall() fantasy.Call {
	return fantasy.Call{Prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "question"}}}}}
}

func cachePresenceContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return context.WithValue(ctx, cliprovider.SessionIDContextKey, "codex-ws-session")
}

type cachePresenceStreamResult struct {
	text          string
	finish        fantasy.StreamPart
	finishCount   int
	errorCount    int
	metadataCount int
	streamError   error
}

func collectCachePresenceStream(stream fantasy.StreamResponse) cachePresenceStreamResult {
	var result cachePresenceStreamResult
	var text strings.Builder
	for part := range stream {
		if part.ProviderMetadata != nil {
			result.metadataCount++
		}
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			result.finish = part
			result.finishCount++
		case fantasy.StreamPartTypeError:
			result.streamError = part.Error
			result.errorCount++
		}
	}
	result.text = text.String()
	return result
}

func requireCachePresenceStream(t *testing.T, result cachePresenceStreamResult, want cachePresenceNetworkCase) {
	t.Helper()
	require.Zero(t, result.errorCount, "stream error count")
	require.Equal(t, 1, result.finishCount, "finish count")
	require.Equal(t, "reply", result.text, "stream text")
	require.Equal(t, fantasy.FinishReasonStop, result.finish.FinishReason, "finish reason")
	require.Equal(t, want.want, result.finish.Usage, "stream usage counters")
	require.Equal(t, want.cached, nativeCacheMetadata(t, result.finish.ProviderMetadata), "stream native cache metadata")
	expectedMetadataParts := 0
	if want.cached != nil {
		expectedMetadataParts = 1
	}
	require.Equal(t, expectedMetadataParts, result.metadataCount, "stream metadata part count")
}

func requireCachePresenceGenerate(t *testing.T, response *fantasy.Response, err error, want cachePresenceNetworkCase) {
	t.Helper()
	require.True(t, err == nil, "Generate error flag")
	require.NotNil(t, response, "Generate response presence")
	require.Equal(t, "reply", response.Content.Text(), "Generate text")
	require.Equal(t, fantasy.FinishReasonStop, response.FinishReason, "Generate finish reason")
	require.Equal(t, want.want, response.Usage, "Generate usage counters")
	require.Equal(t, want.cached, nativeCacheMetadata(t, response.ProviderMetadata), "Generate native cache metadata")
}

func requireCachePresenceCapture(t *testing.T, capture cachePresenceRequestCapture) {
	t.Helper()
	require.True(t, capture.methodOK, "inference method")
	require.True(t, capture.authOK, "authorization identity")
	require.True(t, capture.betaOK, "native protocol beta")
	require.True(t, capture.accountOK, "account identity")
	require.True(t, capture.conversationOK, "conversation identity")
	require.True(t, capture.sessionOK, "session identity")
	require.True(t, capture.clientRequestIDOK, "client request identity")
	require.True(t, capture.modelOK, "model framing")
	require.True(t, capture.requestCreateOK, "response.create framing")
	require.True(t, capture.streamOK, "stream framing")
	require.True(t, capture.storeOK, "store framing")
	require.True(t, capture.noPreviousResponseID, "previous response framing")
	require.True(t, capture.noBackground, "background framing")
}

func requireCachePresenceNetworkCounts(t *testing.T, fixture *cachePresenceNetworkFixture, httpCount, wsCount, upgradeCount int32) {
	t.Helper()
	require.EqualValues(t, httpCount, fixture.httpRequests.Load(), "HTTP inference request count")
	require.EqualValues(t, wsCount, fixture.wsRequests.Load(), "WebSocket inference frame count")
	require.EqualValues(t, upgradeCount, fixture.upgrades.Load(), "WebSocket upgrade count")
}

func requireCachePresenceHTTPCount(t *testing.T, fixture *cachePresenceNetworkFixture, want int32) {
	t.Helper()
	require.EqualValues(t, want, fixture.httpRequests.Load(), "HTTP inference request count")
}

func requireCachePresenceCaptures(t *testing.T, fixture *cachePresenceNetworkFixture, httpCount, wsCount int) {
	t.Helper()
	for range httpCount {
		select {
		case capture := <-fixture.httpCaptures:
			requireCachePresenceCapture(t, capture)
		case <-time.After(3 * time.Second):
			require.FailNow(t, "HTTP request capture timeout")
		}
	}
	for range wsCount {
		select {
		case capture := <-fixture.wsCaptures:
			requireCachePresenceCapture(t, capture)
		case <-time.After(3 * time.Second):
			require.FailNow(t, "WebSocket request capture timeout")
		}
	}
	require.Zero(t, fixture.captureErrors.Load(), "network capture error count")
}

func TestNativeCachedTokensPresenceHTTPAndWebSocketNetworkParity(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	cases := cachePresenceCases()
	events := cachePresenceRepeatedEvents(cases)
	fixture := newCachePresenceNetworkFixture(t, events, events)
	httpModel := codexProviderForTest(t, fixture.server)
	wsModel := websocketCodexModel(t, fixture.server, newTestSource())
	ctx := cachePresenceContext(t)
	call := cachePresenceCall()

	for caseIndex, test := range cases {
		priorUpgrades := int32(0)
		if caseIndex > 0 {
			priorUpgrades = 1
		}
		httpStream, err := httpModel.Stream(ctx, call)
		require.True(t, err == nil, "HTTP Stream API error flag")
		httpStreamResult := collectCachePresenceStream(httpStream)
		requireCachePresenceStream(t, httpStreamResult, test)
		requireCachePresenceNetworkCounts(t, fixture, int32(caseIndex*2+1), int32(caseIndex*2), priorUpgrades)
		httpGenerate, err := httpModel.Generate(ctx, call)
		requireCachePresenceGenerate(t, httpGenerate, err, test)
		requireCachePresenceNetworkCounts(t, fixture, int32(caseIndex*2+2), int32(caseIndex*2), priorUpgrades)
		wsStream, err := wsModel.Stream(ctx, call)
		require.True(t, err == nil, "WebSocket Stream API error flag")
		wsStreamResult := collectCachePresenceStream(wsStream)
		requireCachePresenceStream(t, wsStreamResult, test)
		requireCachePresenceNetworkCounts(t, fixture, int32(caseIndex*2+2), int32(caseIndex*2+1), 1)
		wsGenerate, err := wsModel.Generate(ctx, call)
		requireCachePresenceGenerate(t, wsGenerate, err, test)
		requireCachePresenceNetworkCounts(t, fixture, int32(caseIndex*2+2), int32(caseIndex*2+2), 1)

		require.Equal(t, httpStreamResult.text, wsStreamResult.text, "HTTP/WebSocket stream text parity")
		require.Equal(t, httpStreamResult.finish.Usage, wsStreamResult.finish.Usage, "HTTP/WebSocket stream usage parity")
		require.Equal(t, nativeCacheMetadata(t, httpStreamResult.finish.ProviderMetadata), nativeCacheMetadata(t, wsStreamResult.finish.ProviderMetadata), "HTTP/WebSocket stream metadata parity")
		require.Equal(t, httpGenerate.Content.Text(), wsGenerate.Content.Text(), "HTTP/WebSocket Generate text parity")
		require.Equal(t, httpGenerate.Usage, wsGenerate.Usage, "HTTP/WebSocket Generate usage parity")
		require.Equal(t, nativeCacheMetadata(t, httpGenerate.ProviderMetadata), nativeCacheMetadata(t, wsGenerate.ProviderMetadata), "HTTP/WebSocket Generate metadata parity")
	}

	requireCachePresenceNetworkCounts(t, fixture, int32(len(cases)*2), int32(len(cases)*2), 1)
	requireCachePresenceCaptures(t, fixture, len(cases)*2, len(cases)*2)
}

func cachePresenceFailedEvent(cached int) string {
	return `{"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":100,"output_tokens":2,"total_tokens":102,"input_tokens_details":{"cached_tokens":` + strconv.Itoa(cached) + `}},"error":{"code":"terminal_fixture","message":"fixture"}}}`
}

func requireCachePresenceNoReplay(t *testing.T, err error) {
	t.Helper()
	require.True(t, err != nil, "failed attempt error flag")
	noReplay, ok := err.(interface{ NoReplay() bool })
	require.True(t, ok, "network failure classification")
	require.True(t, noReplay.NoReplay(), "failed exchange replay safety")
}

func TestNativeCachedTokensPresenceHTTPFailedAttemptIsolation(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	seed := cachePresenceNetworkCase{name: "seed", details: `,"input_tokens_details":{"cached_tokens":9}`, usage: true, want: fantasy.Usage{InputTokens: 91, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: 9}, cached: cachePresenceCounter(9)}
	unknown := cachePresenceNetworkCase{name: "omitted details", usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}}
	events := []string{
		cachePresenceCompletion(seed),
		cachePresenceFailedEvent(42),
		cachePresenceCompletion(unknown),
		cachePresenceFailedEvent(0),
		cachePresenceCompletion(unknown),
	}
	fixture := newCachePresenceNetworkFixture(t, events, nil)
	model := codexProviderForTest(t, fixture.server)
	ctx := cachePresenceContext(t)
	call := cachePresenceCall()

	seeded, err := model.Generate(ctx, call)
	requireCachePresenceGenerate(t, seeded, err, seed)
	requireCachePresenceHTTPCount(t, fixture, 1)

	failedStream, err := model.Stream(ctx, call)
	require.True(t, err == nil, "HTTP Stream API error flag")
	failedStreamResult := collectCachePresenceStream(failedStream)
	require.Equal(t, 0, failedStreamResult.finishCount, "failed HTTP stream finish count")
	require.Equal(t, 1, failedStreamResult.errorCount, "failed HTTP stream error count")
	require.Zero(t, failedStreamResult.metadataCount, "failed HTTP stream metadata part count")
	requireCachePresenceHTTPCount(t, fixture, 2)

	stream, err := model.Stream(ctx, call)
	require.True(t, err == nil, "HTTP Stream API error flag")
	requireCachePresenceStream(t, collectCachePresenceStream(stream), unknown)
	requireCachePresenceHTTPCount(t, fixture, 3)

	failedGenerate, err := model.Generate(ctx, call)
	require.True(t, err != nil, "HTTP Generate error flag")
	require.True(t, failedGenerate == nil, "failed HTTP Generate response presence")
	requireCachePresenceHTTPCount(t, fixture, 4)

	generated, err := model.Generate(ctx, call)
	requireCachePresenceGenerate(t, generated, err, unknown)
	requireCachePresenceHTTPCount(t, fixture, 5)
	requireCachePresenceCaptures(t, fixture, len(events), 0)
}

func TestNativeCachedTokensPresenceWebSocketFailedAttemptIsolation(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	seed := cachePresenceNetworkCase{name: "seed", details: `,"input_tokens_details":{"cached_tokens":9}`, usage: true, want: fantasy.Usage{InputTokens: 91, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: 9}, cached: cachePresenceCounter(9)}
	unknownCases := []cachePresenceNetworkCase{
		{name: "omitted details", usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}},
		{name: "null cached counter", details: `,"input_tokens_details":{"cached_tokens":null}`, usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}},
		{name: "null details", details: `,"input_tokens_details":null`, usage: true, want: fantasy.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}},
		{name: "usage omitted", want: fantasy.Usage{}},
	}
	wsEvents := []string{
		cachePresenceCompletion(seed),
		cachePresenceFailedEvent(42),
		cachePresenceCompletion(unknownCases[0]),
		cachePresenceCompletion(unknownCases[1]),
		cachePresenceFailedEvent(0),
		cachePresenceCompletion(unknownCases[2]),
		cachePresenceCompletion(unknownCases[3]),
	}
	fixture := newCachePresenceNetworkFixture(t, []string{cachePresenceCompletion(unknownCases[0])}, wsEvents)
	model := websocketCodexModel(t, fixture.server, newTestSource())
	ctx := cachePresenceContext(t)
	call := cachePresenceCall()

	seeded, err := model.Generate(ctx, call)
	requireCachePresenceGenerate(t, seeded, err, seed)
	requireCachePresenceNetworkCounts(t, fixture, 0, 1, 1)

	failedStream, err := model.Stream(ctx, call)
	require.True(t, err == nil, "WebSocket Stream API error flag")
	failedStreamResult := collectCachePresenceStream(failedStream)
	require.Equal(t, 0, failedStreamResult.finishCount, "failed stream finish count")
	require.Equal(t, 1, failedStreamResult.errorCount, "failed stream error count")
	require.Zero(t, failedStreamResult.metadataCount, "failed stream metadata part count")
	requireCachePresenceNoReplay(t, failedStreamResult.streamError)
	requireCachePresenceNetworkCounts(t, fixture, 0, 2, 1)

	for index, test := range unknownCases[:2] {
		stream, streamErr := model.Stream(ctx, call)
		require.True(t, streamErr == nil, "WebSocket Stream API error flag")
		requireCachePresenceStream(t, collectCachePresenceStream(stream), test)
		requireCachePresenceNetworkCounts(t, fixture, 0, int32(index+3), 2)
	}

	failedGenerate, err := model.Generate(ctx, call)
	require.True(t, failedGenerate == nil, "failed Generate response presence")
	requireCachePresenceNoReplay(t, err)
	requireCachePresenceNetworkCounts(t, fixture, 0, 5, 2)

	for index, test := range unknownCases[2:] {
		generated, generateErr := model.Generate(ctx, call)
		requireCachePresenceGenerate(t, generated, generateErr, test)
		requireCachePresenceNetworkCounts(t, fixture, 0, int32(index+6), 3)
	}

	requireCachePresenceCaptures(t, fixture, 0, len(wsEvents))
}
