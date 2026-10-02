package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// TimeoutParams is the shared explicit per-call timeout shape for
// bash/run_command/agent (docs/plans/2026-09-27-wake-tools-contract.md §3).
// Timeouts are per-call only, never implicit -- no field here defaults to a
// non-zero value. Parsed generically (not via this type) in
// internal/agent's parseTimeoutParam; this type exists so all three tools'
// Params structs describe the identical JSON shape in their schema.
type TimeoutParams struct {
	Seconds int    `json:"seconds" description:"5-604800 (7 days). Seconds from call start."`
	Kind    string `json:"kind" description:"Required when seconds is set: \"wake_only\" or \"terminate_and_wake\"."`
}

// UnmarshalJSON accepts the documented object shape and, for models that
// send a bare number of seconds, also a JSON number -- read as
// {"seconds": N, "kind": "wake_only"}, the safe kind (it never kills
// running work).
func (p *TimeoutParams) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] == 'n' {
		return nil
	}
	if trimmed[0] == '{' {
		type plain TimeoutParams
		return json.Unmarshal(trimmed, (*plain)(p))
	}
	var seconds int
	if err := json.Unmarshal(trimmed, &seconds); err == nil {
		p.Seconds = seconds
		p.Kind = "wake_only"
		return nil
	}
	return fmt.Errorf(`timeout must be an object {"seconds": N, "kind": "wake_only"|"terminate_and_wake"} or a bare number of seconds`)
}
