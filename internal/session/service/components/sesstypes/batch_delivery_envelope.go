package sesstypes

import "encoding/json"

// BatchDeliveryEnvelope is the persisted settled envelope of one node: the
// jobbatch envelope (state, code, summary, details) stored by the R11B commit
// hook in batch_nodes.response_envelope, loaded for sync consumption.
// (Staged here until the sessionbatch move; see MV1 report, deviation (c),
// resolving lane EX1/MV3 wire-type home.)
type BatchDeliveryEnvelope struct {
	State   string          `json:"state"`
	Code    string          `json:"code"`
	Summary string          `json:"summary"`
	Details json.RawMessage `json:"details,omitempty"`
	// Ref is the response_envelope_ref identity (node#sequence).
	Ref string `json:"ref"`
}
