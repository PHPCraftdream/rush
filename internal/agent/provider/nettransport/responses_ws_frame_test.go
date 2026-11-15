package nettransport

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesWSFullFrameProtocolSplitAndExactNumbers(t *testing.T) {
	payload := []byte(`{"model":"gpt-5","stream":true,"background":false,"store":false,"input":[{"type":"function_call","arguments":"{ \"n\": 9007199254740993 }","call_id":"call-1"}],"tools":[{"n":9007199254740992},{"n":9007199254740993}],"stream_options":{"include_usage":true}}`)
	for _, codex := range []bool{false, true} {
		var frame []byte
		var err error
		if codex {
			frame, err = ResponsesWSCodexFullFrame(payload)
		} else {
			frame, err = ResponsesWSFullFrame(payload)
		}
		require.NoError(t, err)
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(frame, &body))
		require.JSONEq(t, `"response.create"`, string(body["type"]))
		require.Contains(t, string(frame), `9007199254740992`)
		require.Contains(t, string(frame), `9007199254740993`)
		require.NotContains(t, body, "background")
		require.JSONEq(t, `false`, string(body["store"]))
		if codex {
			require.JSONEq(t, `true`, string(body["stream"]))
		} else {
			require.NotContains(t, body, "stream")
		}
		var input []struct {
			Arguments string `json:"arguments"`
		}
		require.NoError(t, json.Unmarshal(body["input"], &input))
		require.Equal(t, `{ "n": 9007199254740993 }`, input[0].Arguments)
	}
}

func TestResponsesWSFullFrameRejectsStoredOrBackgroundModes(t *testing.T) {
	for _, payload := range []string{
		`{"stream":true,"store":true}`, `{"stream":true,"background":true}`,
		`{"stream":false}`, `{"stream":true,"previous_response_id":"parent"}`,
		`{"stream":true,"type":"response.create"}`, `null`,
	} {
		_, err := ResponsesWSFullFrame([]byte(payload))
		require.Error(t, err, payload)
	}
}
