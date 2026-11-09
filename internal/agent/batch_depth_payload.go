package agent

import "encoding/json"

type batchExternalDepthPayload struct {
	Tool       string          `json:"tool"`
	Input      json.RawMessage `json:"input"`
	AgentDepth int             `json:"agent_depth"`
}
