package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const coordinatorReplayFirstPrompt = "Carry out the requested side effect, then answer the user."

type coordinatorReplayTrace struct {
	mu             sync.Mutex
	modelEvents    []string
	modelErrors    []error
	ownerEvents    []string
	ownerErrors    []error
	joinedErrors   []error
	peerEvents     []string
	modelPairBuild atomic.Int32
	ownerRuns      atomic.Int32
}

func (tr *coordinatorReplayTrace) recordModel(event string, err error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	appendCoordinatorReplayEvent(&tr.modelEvents, event)
	if err != nil && len(tr.modelErrors) < 4 {
		tr.modelErrors = append(tr.modelErrors, err)
	}
}

func (tr *coordinatorReplayTrace) recordOwner(event string, rawErr, joinedErr error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	appendCoordinatorReplayEvent(&tr.ownerEvents, event)
	if rawErr != nil && len(tr.ownerErrors) < 4 {
		tr.ownerErrors = append(tr.ownerErrors, rawErr)
	}
	if joinedErr != nil && len(tr.joinedErrors) < 4 {
		tr.joinedErrors = append(tr.joinedErrors, joinedErr)
	}
}

func (tr *coordinatorReplayTrace) recordPeer(event string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	appendCoordinatorReplayEvent(&tr.peerEvents, event)
}

func appendCoordinatorReplayEvent(events *[]string, event string) {
	const maxEvents, maxEventBytes = 24, 1024
	if len(*events) >= maxEvents {
		return
	}
	if len(event) > maxEventBytes {
		event = event[:maxEventBytes] + "…[truncated]"
	}
	*events = append(*events, event)
}

func (tr *coordinatorReplayTrace) snapshot() (modelEvents []string, modelErrors []error, ownerEvents []string, ownerErrors []error, joinedErrors []error, peerEvents []string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.modelEvents...), append([]error(nil), tr.modelErrors...),
		append([]string(nil), tr.ownerEvents...), append([]error(nil), tr.ownerErrors...),
		append([]error(nil), tr.joinedErrors...), append([]string(nil), tr.peerEvents...)
}

func coordinatorReplayErrorTree(err error) string {
	if err == nil {
		return "<nil>"
	}
	const maxTreeBytes = 4096
	var lines []string
	var visit func(error, string, int)
	visit = func(current error, prefix string, depth int) {
		if current == nil || depth > 8 || len(lines) >= 24 {
			return
		}
		details := fmt.Sprintf("%s: %v", fmt.Sprintf("%T", current), current)
		if guard, ok := current.(interface{ NoReplay() bool }); ok {
			details += fmt.Sprintf(" NoReplay=%t", guard.NoReplay())
		}
		if state, ok := current.(interface {
			State() nettransport.ResponsesWSSendState
		}); ok {
			details += fmt.Sprintf(" State=%s", state.State())
		}
		if retrySafe, ok := current.(interface{ RetrySafe() bool }); ok {
			details += fmt.Sprintf(" RetrySafe=%t", retrySafe.RetrySafe())
		}
		if providerErr, ok := current.(*fantasy.ProviderError); ok {
			details += fmt.Sprintf(" StatusCode=%d retryable=%t coordinatorClass=%d", providerErr.StatusCode, providerErr.IsRetryable(), classifyProviderError(providerErr))
		}
		lines = append(lines, prefix+coordinatorReplayDiagnosticText(details, 2048))
		if multi, ok := current.(interface{ Unwrap() []error }); ok {
			for index, child := range multi.Unwrap() {
				visit(child, fmt.Sprintf("%s  [%d] ", prefix, index), depth+1)
			}
			return
		}
		if single, ok := current.(interface{ Unwrap() error }); ok {
			visit(single.Unwrap(), prefix+"  -> ", depth+1)
		}
	}
	visit(err, "", 0)
	return coordinatorReplayDiagnosticText(strings.Join(lines, "\n"), maxTreeBytes)
}

func coordinatorReplayDiagnosticText(text string, maxBytes int) string {
	text = strings.ReplaceAll(text, "local-only-test-key", "<redacted-api-key>")
	text = strings.ReplaceAll(text, "refreshable-local-token", "<redacted-oauth-token>")
	if len(text) > maxBytes {
		return text[:maxBytes] + "…[truncated]"
	}
	return text
}

type coordinatorReplayCallLog struct {
	frame []byte
	path  string
}

type coordinatorReplayFailureModel struct {
	fantasy.LanguageModel
	trace   *coordinatorReplayTrace
	streams *atomic.Int32
}

func (m *coordinatorReplayFailureModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	streamNumber := m.streams.Add(1)
	m.trace.recordModel(fmt.Sprintf("Stream #%d started model=%s/%s ctx_err=%v ctx_cause=%v", streamNumber, m.LanguageModel.Provider(), m.LanguageModel.Model(), ctx.Err(), context.Cause(ctx)), nil)
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		m.trace.recordModel(fmt.Sprintf("Stream #%d returned startup error", streamNumber), err)
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeError {
				m.trace.recordModel(fmt.Sprintf("Stream #%d yielded error part ctx_err=%v ctx_cause=%v", streamNumber, ctx.Err(), context.Cause(ctx)), part.Error)
			}
			if !yield(part) || part.Type == fantasy.StreamPartTypeError {
				return
			}
		}
	}, nil
}

type coordinatorReplayOwnerAgent struct {
	SessionAgent
	trace           *coordinatorReplayTrace
	syntheticStatus int
}

func (a *coordinatorReplayOwnerAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	run := a.trace.ownerRuns.Add(1)
	modelIdentity := "<unpinned>"
	if call.SmartModel != nil {
		modelIdentity = call.SmartModel.ModelCfg.Provider + "/" + call.SmartModel.ModelCfg.Model
	}
	a.trace.recordOwner(fmt.Sprintf("SessionAgent.Run #%d started model=%s ctx_err=%v ctx_cause=%v", run, modelIdentity, ctx.Err(), context.Cause(ctx)), nil, nil)
	result, rawErr := a.SessionAgent.Run(ctx, call)
	joinedErr := rawErr
	if rawErr != nil && a.syntheticStatus != 0 {
		// Injection here keeps the joined status outside Fantasy's retry
		// classifier. Otherwise, a synthetic 5xx can be retried and a canceled
		// retry wait can return only ctx.Err(), losing the bridge error. The
		// actual bridge error remains the coordinator-owner input.
		joinedErr = errors.Join(&fantasy.ProviderError{StatusCode: a.syntheticStatus, Message: "synthetic owner-path status"}, rawErr)
	}
	a.trace.recordOwner(fmt.Sprintf("SessionAgent.Run #%d returned raw=%t syntheticStatus=%d ctx_err=%v ctx_cause=%v", run, rawErr != nil, a.syntheticStatus, ctx.Err(), context.Cause(ctx)), rawErr, joinedErr)
	return result, joinedErr
}

func coordinatorReplayPeer(t *testing.T, ctx context.Context, noProgress bool, trace *coordinatorReplayTrace) (*httptest.Server, <-chan coordinatorReplayCallLog, *atomic.Int32, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	calls := make(chan coordinatorReplayCallLog, 8)
	var upgrades, httpInference, wsRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpInference.Add(1)
			trace.recordPeer(fmt.Sprintf("unexpected HTTP inference %s %s", r.Method, r.URL.Path))
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`data: {"error":{"message":"unexpected bounded local HTTP inference"}}` + "\n\n"))
			return
		}
		upgrades.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			trace.recordPeer(fmt.Sprintf("websocket upgrade error: %v", err))
			return
		}
		trace.recordPeer("websocket upgrade accepted")
		defer conn.Close()
		for {
			_, frame, readErr := conn.ReadMessage()
			if readErr != nil {
				trace.recordPeer(fmt.Sprintf("websocket read error after %d requests: %v", wsRequests.Load(), readErr))
				return
			}
			n := wsRequests.Add(1)
			path := "unexpected-request"
			switch n {
			case 1:
				path = "first-tool-turn"
			case 2:
				path = "second-partial-turn"
				if noProgress {
					path = "second-no-progress-turn"
				}
			}
			trace.recordPeer(fmt.Sprintf("websocket request #%d path=%s frame=%s", n, path, string(frame)))
			select {
			case calls <- coordinatorReplayCallLog{frame: append([]byte(nil), frame...), path: path}:
			case <-ctx.Done():
				trace.recordPeer(fmt.Sprintf("peer call log dropped on context completion: %v", ctx.Err()))
				return
			default:
				trace.recordPeer("peer call log channel was full; request frame retained only in trace")
			}
			switch n {
			case 1:
				for _, event := range []string{
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc-real-side-effect","call_id":"call-real-side-effect","name":"record_side_effect","arguments":""}}`,
					`{"type":"response.function_call_arguments.delta","item_id":"fc-real-side-effect","output_index":0,"delta":"{}"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc-real-side-effect","call_id":"call-real-side-effect","name":"record_side_effect","arguments":"{}"}}`,
					`{"type":"response.completed","response":{"id":"resp-real-first","status":"completed","output":[{"type":"function_call","id":"fc-real-side-effect","call_id":"call-real-side-effect","name":"record_side_effect","arguments":"{}"}]}}`,
				} {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
						trace.recordPeer(fmt.Sprintf("websocket event write failed for request #%d event=%s: %v", n, event, err))
						return
					}
					trace.recordPeer(fmt.Sprintf("websocket event written for request #%d: %s", n, event))
				}
			case 2:
				// This is a full-history fallback request without previous_response_id, not a legitimate parent delta.
				// The peer sends the documented nested status-400 missing-parent payload after a genuine send.
				// The current bridge converts event:error into an aborted exchange, so this is a no-replay boundary fixture,
				// not evidence that the nested payload itself is preserved by the SDK parser.
				events := []string{
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg-real-partial","role":"assistant","content":[]}}`,
					`{"type":"response.output_text.delta","item_id":"msg-real-partial","output_index":0,"content_index":0,"delta":"partial answer before missing parent"}`,
					`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id","message":"The specified previous_response_id was not found."}}`,
				}
				if noProgress {
					// No output_item event means this real PrepareStep-owned assistant row stays empty.
					events = []string{`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id","message":"The specified previous_response_id was not found."}}`}
				}
				for _, event := range events {
					if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
						trace.recordPeer(fmt.Sprintf("websocket event write failed for request #%d event=%s: %v", n, event, err))
						return
					}
					trace.recordPeer(fmt.Sprintf("websocket event written for request #%d: %s", n, event))
				}
				return
			default:
				// Any buggy automatic replay gets a bounded terminal failure, never a hanging peer.
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"fixture_unexpected_extra_request","message":"unexpected extra inference attempt"}}`))
				trace.recordPeer("unexpected extra WebSocket request received; bounded fixture error sent")
				return
			}
		}
	}))
	return server, calls, &upgrades, &httpInference, &wsRequests
}

func coordinatorReplayPublicModel(t *testing.T, coord *coordinator, server *httptest.Server) fantasy.LanguageModel {
	t.Helper()
	manager := nettransport.NewResponsesWSManager(t.Context(), 2)
	coord.responsesWS = manager
	t.Cleanup(func() { _ = manager.Close() })
	bridge := &publicResponsesClient{
		manager:  manager,
		endpoint: strings.Replace(server.URL, "http://", "ws://", 1),
		current: func(context.Context) (publicResponsesPolicy, error) {
			return publicResponsesPolicy{
				apiKey:  "local-only-test-key",
				network: nettransport.ResolveNetworkConfig(nil, &config.NetworkConfig{Proxy: config.ProxyNone}),
				client:  publicResponsesLocalClient(server),
			}, nil
		},
	}
	provider, err := newPublicResponsesSDKProvider("https://api.openai.com/v1", "local-only-test-key", nil, bridge)
	require.NoError(t, err)
	public := &publicResponsesProvider{Provider: provider}
	model, err := public.LanguageModel(t.Context(), "gpt-5.4")
	require.NoError(t, err)
	return model
}

func TestCoordinatorRunResponsesReplayFailureComposition(t *testing.T) {
	// Global config must be redirected before either testEnv or config fixtures.
	isolateAllGlobalConfigPaths(t)

	for _, tc := range []struct {
		name            string
		syntheticStatus int
		noProgress      bool
	}{
		{name: "plain nested previous_response_not_found"},
		{name: "synthetic joined 503 vetoes retries", syntheticStatus: http.StatusServiceUnavailable},
		{name: "synthetic joined 401 vetoes refresh", syntheticStatus: http.StatusUnauthorized},
		{name: "no progress joined 503 vetoes blind replay", syntheticStatus: http.StatusServiceUnavailable, noProgress: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := &coordinatorReplayTrace{}
			var modelStreams, toolCalls, refreshCalls atomic.Int32
			var runCtx context.Context
			var runErr error
			var modelName, providerName string
			var providerTokenExpired bool
			var upgrades, httpInference, wsRequests *atomic.Int32
			defer func() {
				if !t.Failed() {
					return
				}
				modelEvents, modelErrors, ownerEvents, ownerErrors, joinedErrors, peerEvents := trace.snapshot()
				ctxErr, ctxCause := error(nil), error(nil)
				if runCtx != nil {
					ctxErr = runCtx.Err()
					ctxCause = context.Cause(runCtx)
				}
				var upgradeCount, httpCount, requestCount int32
				if upgrades != nil {
					upgradeCount = upgrades.Load()
				}
				if httpInference != nil {
					httpCount = httpInference.Load()
				}
				if wsRequests != nil {
					requestCount = wsRequests.Load()
				}
				t.Logf("replay diagnostic: case=%q synthetic_status=%d runErr=%s\nrunErr tree:\n%s\ncontext err=%v cause=%v model=%s/%s provider_token_expired=%t model_pair_builds=%d owner_runs=%d model_streams=%d tool_calls=%d refresh_calls=%d websocket_upgrades=%d websocket_requests=%d http_inference=%d",
					tc.name, tc.syntheticStatus, coordinatorReplayDiagnosticText(fmt.Sprintf("%T %+v", runErr, runErr), 8192), coordinatorReplayErrorTree(runErr), ctxErr, ctxCause,
					providerName, modelName, providerTokenExpired, trace.modelPairBuild.Load(), trace.ownerRuns.Load(), modelStreams.Load(), toolCalls.Load(), refreshCalls.Load(), upgradeCount, requestCount, httpCount)
				for i, err := range modelErrors {
					t.Logf("model boundary error[%d] tree:\n%s", i, coordinatorReplayErrorTree(err))
				}
				for i, err := range ownerErrors {
					t.Logf("real SessionAgent.Run error[%d] tree:\n%s", i, coordinatorReplayErrorTree(err))
				}
				for i, err := range joinedErrors {
					t.Logf("synthetic outer owner error[%d] tree:\n%s", i, coordinatorReplayErrorTree(err))
				}
				t.Logf("model events: %s", coordinatorReplayDiagnosticText(strings.Join(modelEvents, " | "), 8192))
				t.Logf("real owner events: %s", coordinatorReplayDiagnosticText(strings.Join(ownerEvents, " | "), 8192))
				t.Logf("peer events: %s", coordinatorReplayDiagnosticText(strings.Join(peerEvents, " | "), 8192))
			}()
			env := testEnv(t)
			coord := newWorkerToolTestCoordinator(t, env, false)
			coord.cfg.Config().Options.Network = &config.NetworkConfig{Proxy: config.ProxyNone}
			coord.cfg.Config().Options.ResponsesTransport = config.ResponsesTransportAuto
			oneRetry := 1
			coord.cfg.Config().Options.StreamStallRetries = &oneRetry
			require.Equal(t, 1, *coord.cfg.Config().Options.StreamStallRetries, "coordinator retry budget must remain positive")
			coord.cfg.Config().Options.DisableAutoSummarize = true
			providerCfg, ok := coord.cfg.Config().Providers.Get("smart-provider")
			require.True(t, ok)
			providerCfg.ID = "openai"
			providerCfg.Type = openai.Name
			providerCfg.APIKey = "local-only-test-key"
			providerCfg.OAuthToken = &oauth.Token{AccessToken: "refreshable-local-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
			providerCfg.Network = &config.NetworkConfig{Proxy: config.ProxyNone}
			providerCfg.Models = []catwalk.Model{{
				ID: "gpt-5.4", Name: "gpt-5.4", ContextWindow: 200000, DefaultMaxTokens: 2048,
			}}
			coord.cfg.SetProviderRuntimeConfig("openai", providerCfg)
			coord.cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: "openai", Model: "gpt-5.4"}
			coord.cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: "openai", Model: "gpt-5.4"}

			ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "responses-replay"), 5*time.Second)
			t.Cleanup(cancel)
			server, peerCalls, upgradeCounter, httpCounter, requestCounter := coordinatorReplayPeer(t, ctx, tc.noProgress, trace)
			upgrades, httpInference, wsRequests = upgradeCounter, httpCounter, requestCounter
			t.Cleanup(server.Close)
			baseModel := coordinatorReplayPublicModel(t, coord, server)
			model := &coordinatorReplayFailureModel{LanguageModel: baseModel, trace: trace, streams: &modelStreams}
			coord.modelPairBuilder = func(_ context.Context, _ *config.Config, smart, fast config.SelectedModel) (Model, Model, error) {
				trace.modelPairBuild.Add(1)
				trace.recordModel(fmt.Sprintf("modelPairBuilder smart=%s/%s fast=%s/%s", smart.Provider, smart.Model, fast.Provider, fast.Model), nil)
				return Model{Model: model, CatwalkCfg: catwalk.Model{ID: smart.Model, Name: smart.Model, ContextWindow: 200000, DefaultMaxTokens: 2048}, ModelCfg: smart}, Model{Model: model, CatwalkCfg: catwalk.Model{ID: fast.Model, Name: fast.Model, ContextWindow: 200000, DefaultMaxTokens: 2048}, ModelCfg: fast}, nil
			}

			// Add synthetic owner classification after the real SessionAgent/
			// Fantasy retry boundary.
			previousBackoff := streamStallRetryBaseBackoff
			streamStallRetryBaseBackoff = time.Millisecond
			t.Cleanup(func() { streamStallRetryBaseBackoff = previousBackoff })
			coord.refreshOAuth2TokenFn = func(context.Context, config.ProviderConfig) error {
				refreshCalls.Add(1)
				trace.recordOwner("OAuth refresh callback invoked", nil, nil)
				return nil
			}
			tool := fantasy.NewAgentTool("record_side_effect", "Record the requested local fixture side effect.", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				toolCalls.Add(1)
				return fantasy.NewTextResponse("side effect recorded"), nil
			})
			sessionAgent := testSessionAgent(env, model, model, "fixture system prompt", tool).(*sessionAgent)
			sessionAgent.config = coord.cfg
			sessionAgent.coord = coord
			sessionAgent.disableAutoSummarize = true
			sessionAgent.streamIdleTimeout = time.Second
			sessionAgent.streamWatchdogTick = 10 * time.Millisecond
			sessionAgent.toolMaxDuration = time.Second
			coord.currentAgent = &coordinatorReplayOwnerAgent{SessionAgent: sessionAgent, trace: trace, syntheticStatus: tc.syntheticStatus}
			coord.driverHints = newBatchDriverHints()
			coord.subAgentDrivers = newSubAgentDriverRegistry()
			coord.skillTracker = skills.NewTracker(nil)
			coord.activeSkills = nil
			sess, err := env.sessions.Create(ctx, "replay composition acceptance")
			require.NoError(t, err)
			// Existing history plus a real title suppresses unrelated title-generation inference.
			// The counted transcript keeps seeded rows distinguishable from artifacts of this call.
			require.NoError(t, env.sessions.Rename(ctx, sess.ID, "Responses replay fixture"))
			_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "fixture history"}}})
			require.NoError(t, err)
			_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "prior answer"}, message.Finish{Reason: message.FinishReasonEndTurn}}})
			require.NoError(t, err)

			runCtx = ctx
			var callResult *CallResultRecorder
			if tc.noProgress {
				callResult = NewCallResultRecorder()
				runCtx = WithCallResultRecorder(ctx, callResult)
			}
			pinned, err := coord.resolveSessionModelsInternal(ctx, sess.ID, false)
			require.NoError(t, err)
			modelName, providerName = pinned.smart.ModelCfg.Model, pinned.smart.ModelCfg.Provider
			providerCfg, providerTokenExists := coord.cfg.Config().Providers.Get(providerName)
			providerTokenExpired = providerTokenExists && providerCfg.OAuthToken != nil && providerCfg.OAuthToken.IsExpired()
			_, runErr = coord.runInternal(runCtx, sess.ID, coordinatorReplayFirstPrompt, pinned)
			require.Error(t, runErr)
			_, modelBoundaryErrors, _, ownerErrors, _, _ := trace.snapshot()
			require.Len(t, modelBoundaryErrors, 1, "the real public bridge error must reach the model boundary once")
			require.True(t, responsesWSReplayForbidden(modelBoundaryErrors[0]), "the bridge error must carry NoReplay before coordinator owner classification")
			require.Len(t, ownerErrors, 1, "the real SessionAgent owner must return exactly one raw error")
			require.True(t, responsesWSReplayForbidden(ownerErrors[0]), "the actual SessionAgent error must preserve the bridge NoReplay guard")
			require.EqualValues(t, 1, trace.ownerRuns.Load(), "the coordinator must not invoke its SessionAgent owner again")
			require.True(t, responsesWSReplayForbidden(runErr), "the joined coordinator error must preserve the real bridge replay guard")
			require.ErrorIs(t, runErr, ownerErrors[0], "the joined coordinator error must retain the unchanged real owner error")
			if tc.noProgress {
				// A plain 503 is retryable; the joined live bridge NoReplay error must veto it.
				require.Equal(t, classTransient, classifyProviderError(&fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable}))
				require.NotEmpty(t, callResult.Resolve(), "the real failed PrepareStep assistant row must be captured by this call")
				require.True(t, callResult.Owns(callResult.Resolve()), "the captured failed-step ID must belong to this coordinator call")
			}
			if tc.syntheticStatus != 0 {
				var syntheticErr *fantasy.ProviderError
				require.ErrorAs(t, runErr, &syntheticErr, "synthetic status must be joined after the real SessionAgent error returns")
				require.Equal(t, tc.syntheticStatus, syntheticErr.StatusCode)
				var rawProviderErr *fantasy.ProviderError
				if errors.As(ownerErrors[0], &rawProviderErr) {
					require.NotEqual(t, tc.syntheticStatus, rawProviderErr.StatusCode, "synthetic provider classification must not enter Fantasy's model/step retry boundary")
				}
			}
			if tc.syntheticStatus == http.StatusUnauthorized {
				require.Equal(t, int32(0), refreshCalls.Load(), "synthetic joined 401 must not trigger the real refresh callback")
			}

			first := publicResponsesCapture(t, ctx, peerCalls, "first real Responses request")
			second := publicResponsesCapture(t, ctx, peerCalls, "second real Responses request")
			require.Equal(t, int32(0), refreshCalls.Load(), "no 401 owner refresh is allowed")
			require.Equal(t, "first-tool-turn", first.path)
			if tc.noProgress {
				require.Equal(t, "second-no-progress-turn", second.path)
			} else {
				require.Equal(t, "second-partial-turn", second.path)
			}
			firstFrame := publicResponsesHistoryDecode(t, first.frame)
			secondFrame := publicResponsesHistoryDecode(t, second.frame)
			require.Equal(t, "response.create", firstFrame["type"])
			require.Equal(t, "response.create", secondFrame["type"])
			require.NotContains(t, secondFrame, "previous_response_id")
			require.NotContains(t, secondFrame, "stream")
			firstTools, ok := firstFrame["tools"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, firstTools)
			secondInput, ok := secondFrame["input"].([]any)
			require.True(t, ok)
			var sawInitialPrompt, sawSerializedToolCall, sawToolResult bool
			initialPromptCount := 0
			for _, rawItem := range secondInput {
				item, ok := rawItem.(map[string]any)
				require.True(t, ok)
				// Public Responses serializers may omit the message type, so recognize
				// the original request by its user role and content independently.
				if item["role"] == "user" && strings.Contains(fmt.Sprint(item["content"]), coordinatorReplayFirstPrompt) {
					sawInitialPrompt = true
					initialPromptCount++
				}
				switch item["type"] {
				case "function_call":
					_, hasOutputItemID := item["id"]
					sawSerializedToolCall = !hasOutputItemID &&
						item["call_id"] == "call-real-side-effect" &&
						item["name"] == "record_side_effect" &&
						item["arguments"] == "{}"
				case "function_call_output":
					sawToolResult = item["call_id"] == "call-real-side-effect" && item["output"] == "side effect recorded"
				}
			}
			require.True(t, sawInitialPrompt, "native serializer must retain the original user request in its full-frame fallback")
			require.Equal(t, 1, initialPromptCount, "the failed attempt must include the original user prompt exactly once")
			require.True(t, sawSerializedToolCall, "native serializer must replay the genuine function call from the first response")
			require.True(t, sawToolResult, "native serializer must replay the real tool result")
			// The pinned SDK serializer has no Responses output item IDs on its replayed function call,
			// so this real history is full-frame fallback—not a fabricated delta-compatible transcript.
			require.Contains(t, string(second.frame), `"type":"function_call_output"`)
			require.Greater(t, ProviderMaxRetries, 0, "Fantasy's provider retry budget must remain enabled")
			require.Equal(t, int32(1), toolCalls.Load(), "real Fantasy must execute the tool call once before the failing continuation request")
			require.Equal(t, int32(2), modelStreams.Load(), "only two bounded inference streams are expected")
			require.Equal(t, int32(1), trace.ownerRuns.Load(), "one coordinator owner run, without coordinator retry")
			require.Equal(t, int32(2), wsRequests.Load(), "first tool turn plus one continuation request; no coordinator retry")
			require.Equal(t, int32(1), upgrades.Load(), "the accepted first exchange reuses one local WebSocket")
			require.Zero(t, httpInference.Load(), "no inference may fall back to HTTP")
			require.Zero(t, refreshCalls.Load(), "the actual unauthorized owner must not refresh after the real replay guard, including the joined-401 case")

			messages, err := env.messages.List(ctx, sess.ID)
			require.NoError(t, err)
			userMessages, assistants, toolMessages := 0, 0, 0
			partialPersisted := false
			var failedAttemptAssistant message.Message
			var sawFailedAttemptAssistant bool
			originalPromptRows := 0
			toolStepAssistantID := ""
			for _, msg := range messages {
				if tc.noProgress && msg.ID == callResult.Resolve() {
					failedAttemptAssistant = msg
					sawFailedAttemptAssistant = true
				}
				if msg.Role == message.User {
					userMessages++
					if strings.Contains(msg.FullText(), "previous response was interrupted") {
						t.Fatalf("coordinator created an automatic continuation user message: %q", msg.FullText())
					}
					if msg.FullText() == coordinatorReplayFirstPrompt {
						originalPromptRows++
					}
				}
				if msg.Role == message.Tool {
					toolMessages++
				}
				if msg.Role == message.Assistant {
					assistants++
					if len(msg.ToolCalls()) > 0 {
						toolStepAssistantID = msg.ID
					}
					if strings.Contains(msg.FullText(), "partial answer before missing parent") {
						partialPersisted = msg.FinishPart() != nil && msg.FinishPart().Reason == message.FinishReasonError
					}
				}
			}
			require.Equal(t, 2, userMessages, "seeded history and the original coordinator user message should persist")
			require.Equal(t, 1, originalPromptRows, "the original prompt must be persisted exactly once")
			require.Equal(t, 3, assistants, "seeded history, real tool step and failed step are persisted")
			require.Equal(t, 1, toolMessages, "the first step's actual tool result is persisted exactly once")
			if tc.noProgress {
				require.True(t, sawFailedAttemptAssistant, "the recorder's owned assistant row must be present in the session")
				require.Equal(t, message.Assistant, failedAttemptAssistant.Role)
				require.NotEmpty(t, toolStepAssistantID, "the preceding successful tool step must remain present as separate attempt evidence")
				require.NotEqual(t, toolStepAssistantID, callResult.Resolve(), "the failed attempt recorder must not retain the prior tool step's assistant ID")
				finish := failedAttemptAssistant.FinishPart()
				require.NotNil(t, finish)
				require.Equal(t, message.FinishReasonError, finish.Reason)
				require.Empty(t, failedAttemptAssistant.FullText())
				require.Empty(t, failedAttemptAssistant.FullReasoningText())
				require.Empty(t, failedAttemptAssistant.ToolCalls())
				require.False(t, turnMadeProgress(failedAttemptAssistant), "the exact call-owned failed row contains no current-attempt progress")
				require.False(t, partialPersisted)
			} else {
				require.True(t, partialPersisted, "the actual partial assistant output is the continuation ownership oracle")
			}
		})
	}
}
