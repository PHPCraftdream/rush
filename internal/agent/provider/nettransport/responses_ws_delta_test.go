package nettransport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func responsesWSDeltaRegressionSeed(t *testing.T, request, completed string, requireStream bool) responsesWSDeltaState {
	t.Helper()
	state := responsesWSDeltaState{requireStream: requireStream}
	_, candidate := state.prepare([]byte(request), true)
	require.NotNil(t, candidate)
	state.observe([]byte(completed))
	state.commit()
	require.NotNil(t, state.seed)
	return state
}

func responsesWSDeltaRegressionValue(t *testing.T, raw []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	require.NoError(t, decoder.Decode(&value))
	return value
}

func responsesWSDeltaRegressionRequireID(t *testing.T, frame []byte, id string) {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(frame, &fields))
	if id == "" {
		require.NotContains(t, fields, "previous_response_id")
		return
	}
	var got string
	require.NoError(t, json.Unmarshal(fields["previous_response_id"], &got))
	require.Equal(t, id, got)
}

func responsesWSDeltaRegressionAssertReconstruction(t *testing.T, first, completed, second, wire []byte) {
	t.Helper()
	var firstFields, completedFields, responseFields, secondFields, wireFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(first, &firstFields))
	require.NoError(t, json.Unmarshal(completed, &completedFields))
	require.NoError(t, json.Unmarshal(completedFields["response"], &responseFields))
	require.NoError(t, json.Unmarshal(second, &secondFields))
	require.NoError(t, json.Unmarshal(wire, &wireFields))

	var firstInput, actualOutput, suffix []json.RawMessage
	require.NoError(t, json.Unmarshal(firstFields["input"], &firstInput))
	require.NoError(t, json.Unmarshal(responseFields["output"], &actualOutput))
	require.NoError(t, json.Unmarshal(wireFields["input"], &suffix))
	reconstructedInput, err := json.Marshal(append(append(append([]json.RawMessage(nil), firstInput...), actualOutput...), suffix...))
	require.NoError(t, err)
	require.Equal(t, responsesWSDeltaRegressionValue(t, secondFields["input"]), responsesWSDeltaRegressionValue(t, reconstructedInput))

	wireFields["input"] = reconstructedInput
	delete(wireFields, "previous_response_id")
	reconstructedFrame, err := json.Marshal(wireFields)
	require.NoError(t, err)
	require.Equal(t, responsesWSDeltaRegressionValue(t, second), responsesWSDeltaRegressionValue(t, reconstructedFrame))
}

func responsesWSDeltaRegressionReceive(t *testing.T, ctx context.Context, frames <-chan []byte) []byte {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-ctx.Done():
		t.Fatalf("timed out waiting for websocket frame: %v", ctx.Err())
	}
	return nil
}

func TestResponsesWSDeltaRegressionNormalizesExactAppend(t *testing.T) {
	const prior = `{"type":"response.create","model":"model-a","store":false,"instructions":"Keep the same instruction text.","reasoning":{"effort":"high"},"tools":[{"name":"lookup","description":"Read-only lookup tool","parameters":{"type":"object","properties":{"value":{"type":"integer"}}}}],"input":[{"role":"user","content":"A sufficiently detailed initial question that should be removed from the continuation frame.","sequence":9007199254740992},{"role":"assistant","content":"An earlier assistant message that is also part of the retained prefix."}]}`
	const completed = `{"type":"response.completed","response":{"id":"resp-seed","status":"completed","output":[{"type":"function_call","id":"call-1","name":"lookup","arguments":"{\"value\":1}","sequence":900719925474099312345}]}}`
	const second = `{ "tools" : [ { "parameters" : { "properties" : { "value" : { "type" : "integer" } }, "type" : "object" }, "description" : "Read-only lookup tool", "name" : "lookup" } ], "input" : [ { "sequence" : 9007199254740992, "content" : "A sufficiently detailed initial question that should be removed from the continuation frame.", "role" : "user" } , { "content":"An earlier assistant message that is also part of the retained prefix.","role":"assistant"} , { "sequence" : 900719925474099312345, "arguments" : "{\"value\":1}", "name" : "lookup", "id" : "call-1", "type" : "function_call" } , {"role":"user","content":"Follow-up question"} ], "reasoning" : { "effort" : "high" }, "instructions" : "Keep the same instruction text.", "store" : false, "model" : "model-a", "type" : "response.create" }`
	state := responsesWSDeltaRegressionSeed(t, prior, completed, false)
	wire, _ := state.prepare([]byte(second), true)
	responsesWSDeltaRegressionRequireID(t, wire, "resp-seed")
	require.Less(t, len(wire), len(second))
	responsesWSDeltaRegressionAssertReconstruction(t, []byte(prior), []byte(completed), []byte(second), wire)
}

func TestResponsesWSDeltaRegressionIndependentMutationsFallBack(t *testing.T) {
	const prior = `{"type":"response.create","model":"model-a","store":false,"instructions":"Keep the same instruction text.","reasoning":{"effort":"high"},"tools":[{"name":"lookup","description":"Read-only lookup tool","parameters":{"type":"object","properties":{"value":{"type":"integer"}}}}],"input":[{"role":"user","content":"A sufficiently detailed initial question that should be removed from the continuation frame.","sequence":9007199254740992},{"role":"assistant","content":"An earlier assistant message that is also part of the retained prefix."}]}`
	const completed = `{"type":"response.completed","response":{"id":"resp-seed","status":"completed","output":[{"type":"function_call","id":"call-1","name":"lookup","arguments":"{\"value\":1}","sequence":900719925474099312345}]}}`
	const full = `{ "tools" : [ { "parameters" : { "properties" : { "value" : { "type" : "integer" } }, "type" : "object" }, "description" : "Read-only lookup tool", "name" : "lookup" } ], "input" : [ { "sequence" : 9007199254740992, "content" : "A sufficiently detailed initial question that should be removed from the continuation frame.", "role" : "user" } , { "role":"assistant","content":"An earlier assistant message that is also part of the retained prefix."} , { "sequence" : 900719925474099312345, "arguments" : "{\"value\":1}", "name" : "lookup", "id" : "call-1", "type" : "function_call" } , {"role":"user","content":"Follow-up question"} ], "reasoning" : { "effort" : "high" }, "instructions" : "Keep the same instruction text.", "store" : false, "model" : "model-a", "type" : "response.create" }`
	mutations := []struct {
		name  string
		frame string
	}{
		{"input integer", strings.Replace(full, "9007199254740992", "9007199254740993", 1)},
		{"output integer", strings.Replace(full, "900719925474099312345", "900719925474099312346", 1)},
		{"argument string whitespace", strings.Replace(full, `"{\"value\":1}"`, `"{ \"value\" : 1 }"`, 1)},
		{"output ID", strings.Replace(full, `"id" : "call-1"`, `"id" : "call-2"`, 1)},
		{"absent versus null output field", strings.Replace(full, `"id" : "call-1", "type"`, `"id" : "call-1", "status" : null, "type"`, 1)},
		{"array order", strings.Replace(full, `[ { "sequence" : 9007199254740992, "content" : "A sufficiently detailed initial question that should be removed from the continuation frame.", "role" : "user" } , { "role":"assistant","content":"An earlier assistant message that is also part of the retained prefix."} ,`, `[{ "role":"assistant","content":"An earlier assistant message that is also part of the retained prefix."} , { "sequence" : 9007199254740992, "content" : "A sufficiently detailed initial question that should be removed from the continuation frame.", "role" : "user" } ,`, 1)},
		{"model", strings.Replace(full, `"model" : "model-a"`, `"model" : "model-b"`, 1)},
		{"tools", strings.Replace(full, `"name" : "lookup"`, `"name" : "lookup-v2"`, 1)},
		{"instructions", strings.Replace(full, `"Keep the same instruction text."`, `"Use changed instructions."`, 1)},
		{"effort", strings.Replace(full, `"effort" : "high"`, `"effort" : "low"`, 1)},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutationBytes := []byte(mutation.frame)
			require.NotEqual(t, responsesWSDeltaRegressionValue(t, []byte(full)), responsesWSDeltaRegressionValue(t, mutationBytes))
			state := responsesWSDeltaRegressionSeed(t, prior, completed, false)
			wire, _ := state.prepare(mutationBytes, true)
			require.Equal(t, mutationBytes, wire)
			responsesWSDeltaRegressionRequireID(t, wire, "")
		})
	}
}

func TestResponsesWSDeltaRegressionProtocolEligibility(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"id":"resp-protocol","status":"completed","output":[]}}`
	const publicPrior = `{"type":"response.create","model":"m","store":false,"input":[{"role":"user","content":"A long initial question provides a substantial retained prefix for protocol eligibility verification without changing any request options."}]}`
	const codexPrior = `{"type":"response.create","stream":true,"model":"m","store":false,"input":[{"role":"user","content":"A long initial question provides a substantial retained prefix for protocol eligibility verification without changing any request options."}]}`
	const publicRequest = `{"type":"response.create","model":"m","store":false,"input":[{"role":"user","content":"A long initial question provides a substantial retained prefix for protocol eligibility verification without changing any request options."},{"role":"user","content":"next"}]}`
	const codexRequest = `{"type":"response.create","stream":true,"model":"m","store":false,"input":[{"role":"user","content":"A long initial question provides a substantial retained prefix for protocol eligibility verification without changing any request options."},{"role":"user","content":"next"}]}`
	for _, test := range []struct {
		name      string
		protocol  string
		headers   http.Header
		prior     string
		stream    bool
		request   string
		wantDelta bool
	}{
		{"public without stream", "public-openai-responses", nil, publicPrior, false, publicRequest, true},
		{"codex exact beta and stream", "codex-responses", http.Header{"Openai-Beta": []string{responsesWSDeltaBeta}}, codexPrior, true, codexRequest, true},
		{"codex without stream", "codex-responses", http.Header{"Openai-Beta": []string{responsesWSDeltaBeta}}, codexPrior, true, `{"type":"response.create","model":"m","store":false,"input":[{"role":"user","content":"A long initial question provides a substantial retained prefix for protocol eligibility verification without changing any request options."},{"role":"user","content":"next"}]}`, false},
		{"codex missing beta", "codex-responses", nil, codexPrior, true, codexRequest, false},
		{"codex other beta", "codex-responses", http.Header{"Openai-Beta": []string{"responses=v1"}}, codexPrior, true, codexRequest, false},
		{"other protocol", "other", http.Header{"Openai-Beta": []string{responsesWSDeltaBeta}}, publicPrior, false, publicRequest, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := responsesWSDeltaRegressionSeed(t, test.prior, completed, test.stream)
			allowed := responsesWSDeltaAllowed(test.protocol, test.headers)
			wire, candidate := state.prepare([]byte(test.request), allowed)
			if test.wantDelta {
				responsesWSDeltaRegressionRequireID(t, wire, "resp-protocol")
				return
			}
			require.Equal(t, []byte(test.request), wire)
			responsesWSDeltaRegressionRequireID(t, wire, "")
			if !allowed || test.name == "codex without stream" {
				require.Nil(t, candidate)
			}
		})
	}
}

func TestResponsesWSDeltaRegressionCompletionLifecycle(t *testing.T) {
	const request = `{"type":"response.create","model":"m","stream":true,"store":false,"input":[{"role":"user","content":"ask"}]}`
	const completed = `{"type":"response.completed","response":{"id":"resp-life","status":"completed","output":[]}}`
	t.Run("completion commits only when marked", func(t *testing.T) {
		state := responsesWSDeltaState{}
		_, candidate := state.prepare([]byte(request), true)
		require.NotNil(t, candidate)
		state.observe([]byte(completed))
		require.Nil(t, state.seed)
		state.commit()
		require.NotNil(t, state.seed)
	})
	t.Run("contradictory output does not seed", func(t *testing.T) {
		state := responsesWSDeltaState{}
		_, _ = state.prepare([]byte(request), true)
		state.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[]}}`))
		state.observe([]byte(completed))
		state.commit()
		require.Nil(t, state.seed)
	})
	for _, event := range []string{`{"type":"response.incomplete"}`, `{"type":"error"}`} {
		t.Run(event, func(t *testing.T) {
			state := responsesWSDeltaRegressionSeed(t, request, completed, true)
			_, _ = state.prepare([]byte(request), true)
			state.observe([]byte(event))
			state.commit()
			require.Nil(t, state.seed)
		})
	}
}

func TestResponsesWSDeltaRegressionParserRejectsMalformedJSON(t *testing.T) {
	for _, frame := range [][]byte{
		[]byte(`{"type":"response.create","type":"response.create"}`),
		[]byte(`{"text":"\uD800"}`),
		{0xff},
	} {
		state := responsesWSDeltaState{seed: &responsesWSDeltaSeed{responseID: "old"}}
		wire, candidate := state.prepare(frame, true)
		require.Equal(t, frame, wire)
		require.Nil(t, candidate)
		require.Nil(t, state.seed)
		_, err := parseResponsesWSDeltaJSON(frame)
		require.Error(t, err)
	}
	paired, err := parseResponsesWSDeltaJSON([]byte(`{"text":"\uD83D\uDE00"}`))
	require.NoError(t, err)
	require.NotNil(t, paired)
}

func TestResponsesWSDeltaRegressionParserEnforcesBudgets(t *testing.T) {
	tooDeep := []byte(strings.Repeat("[", responsesWSDeltaMaxDepth+2) + `0` + strings.Repeat("]", responsesWSDeltaMaxDepth+2))
	_, err := parseResponsesWSDeltaJSON(tooDeep)
	require.Error(t, err)
	tooMany := []byte("[" + strings.TrimSuffix(strings.Repeat("0,", responsesWSDeltaMaxNodes), ",") + "]")
	_, err = parseResponsesWSDeltaJSON(tooMany)
	require.Error(t, err)
	tooLarge := bytes.Repeat([]byte(" "), responsesWSDeltaMaxBytes+1)
	_, err = parseResponsesWSDeltaJSON(tooLarge)
	require.Error(t, err)
}

func TestResponsesWSDeltaRegressionSequentialNetworkContinuation(t *testing.T) {
	const first = `{"type":"response.create","model":"model-a","store":false,"tools":[{"name":"long_tool_name","description":"A deliberately repeated long tool description keeps options identical while making the complete follow-up request larger than its continuation frame.","parameters":{"type":"object"}}],"input":[{"role":"user","content":"initial question"}]}`
	const completed = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"type":"function_call","id":"call-1","name":"long_tool_name","arguments":"{ \"value\" : 1 }"}]}}`
	const second = `{"type":"response.create","model":"model-a","store":false,"tools":[{"name":"long_tool_name","description":"A deliberately repeated long tool description keeps options identical while making the complete follow-up request larger than its continuation frame.","parameters":{"type":"object"}}],"input":[{"role":"user","content":"initial question"},{"type":"function_call","id":"call-1","name":"long_tool_name","arguments":"{ \"value\" : 1 }"},{"type":"function_call_output","call_id":"call-1","output":"tool result"},{"role":"user","content":"follow-up"}]}`
	requests := make(chan []byte, 2)
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < 2; i++ {
			_, frame, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			requests <- frame
			requestCount.Add(1)
			event := completed
			if i == 1 {
				event = `{"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[]}}`
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	options := responsesWSTestOptions(server.URL)
	options.Protocol = "public-openai-responses"
	conn, err := DialResponsesWebSocket(ctx, options)
	require.NoError(t, err)
	defer conn.Close()
	for _, frame := range [][]byte{[]byte(first), []byte(second)} {
		require.NoError(t, conn.BeginExchange())
		require.NoError(t, conn.SendRaw(ctx, frame))
		_, err = conn.ReceiveRaw(ctx)
		require.NoError(t, err)
		require.NoError(t, conn.MarkCompleted())
	}
	firstWire := responsesWSDeltaRegressionReceive(t, ctx, requests)
	secondWire := responsesWSDeltaRegressionReceive(t, ctx, requests)
	require.Equal(t, []byte(first), firstWire)
	responsesWSDeltaRegressionRequireID(t, secondWire, "resp-1")
	require.Less(t, len(secondWire), len(second))
	responsesWSDeltaRegressionAssertReconstruction(t, []byte(first), []byte(completed), []byte(second), secondWire)
	require.Equal(t, 2, int(requestCount.Load()))
}

func TestResponsesWSDeltaRegressionReconnectStartsFull(t *testing.T) {
	requests := make(chan []byte, 2)
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, frame, err := conn.ReadMessage()
		if err != nil {
			return
		}
		requests <- frame
		requestCount.Add(1)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-reconnect","status":"completed","output":[]}}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	options := responsesWSTestOptions(server.URL)
	options.Protocol = "public-openai-responses"
	initial := []byte(`{"type":"response.create","model":"m","store":false,"input":[{"role":"user","content":"start"}]}`)
	first, err := DialResponsesWebSocket(ctx, options)
	require.NoError(t, err)
	require.NoError(t, first.BeginExchange())
	require.NoError(t, first.SendRaw(ctx, initial))
	_, err = first.ReceiveRaw(ctx)
	require.NoError(t, err)
	require.NoError(t, first.MarkCompleted())
	_ = first.Close()
	require.Equal(t, initial, responsesWSDeltaRegressionReceive(t, ctx, requests))

	second, err := DialResponsesWebSocket(ctx, options)
	require.NoError(t, err)
	defer second.Close()
	reconnected := []byte(`{"type":"response.create","model":"m","store":false,"input":[{"role":"user","content":"start"},{"role":"user","content":"after reconnect"}]}`)
	require.NoError(t, second.BeginExchange())
	require.NoError(t, second.SendRaw(ctx, reconnected))
	_, err = second.ReceiveRaw(ctx)
	require.NoError(t, err)
	require.NoError(t, second.MarkCompleted())
	secondWire := responsesWSDeltaRegressionReceive(t, ctx, requests)
	require.Equal(t, reconnected, secondWire)
	require.Equal(t, 2, int(requestCount.Load()))
}
