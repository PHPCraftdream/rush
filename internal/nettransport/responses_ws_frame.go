package nettransport

import (
	"bytes"
	"encoding/json"
	"errors"
)

// ResponsesWSFullFrame preserves the pinned provider's JSON values, without
// float64 conversion. Server-stored chaining and background work stay on HTTP.
func ResponsesWSFullFrame(payload []byte) ([]byte, error) {
	return responsesWSFullFrame(payload, false)
}

// Codex keeps stream:true, unlike public OpenAI's WebSocket protocol.
// openai/codex 37103d7f01e82162472c6fba7b4088d5a1303321:
// codex-rs/codex-api/src/common.rs ResponseCreateWsRequest.
func ResponsesWSCodexFullFrame(payload []byte) ([]byte, error) {
	return responsesWSFullFrame(payload, true)
}

func responsesWSFullFrame(payload []byte, codex bool) ([]byte, error) {
	if len(payload) > responsesWSDefaultFrameBytes {
		return nil, errors.New("responses websocket: request exceeds frame budget")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil || body == nil {
		return nil, errors.New("responses websocket: invalid request object")
	}
	if !bytes.Equal(bytes.TrimSpace(body["stream"]), []byte("true")) {
		return nil, errors.New("responses websocket: streaming request required")
	}
	for _, key := range []string{"store", "background"} {
		value := bytes.TrimSpace(body[key])
		if len(value) != 0 && !bytes.Equal(value, []byte("false")) && !bytes.Equal(value, []byte("null")) {
			return nil, errors.New("responses websocket: unsupported request mode")
		}
	}
	if value := bytes.TrimSpace(body["previous_response_id"]); len(value) != 0 && !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte(`""`)) {
		return nil, errors.New("responses websocket: stored chaining unsupported")
	}
	if _, exists := body["type"]; exists {
		return nil, errors.New("responses websocket: request type already set")
	}
	if !codex {
		delete(body, "stream")
	}
	delete(body, "background")
	body["type"] = json.RawMessage(`"response.create"`)
	return json.Marshal(body)
}
