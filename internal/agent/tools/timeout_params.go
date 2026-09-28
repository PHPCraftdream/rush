package tools

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
