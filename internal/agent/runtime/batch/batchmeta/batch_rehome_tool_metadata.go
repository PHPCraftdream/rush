package batchmeta

// AsyncToolMetadata is the tool-response metadata of a delegated or
// backgrounded call.
type AsyncToolMetadata struct {
	Async bool `json:"async"`
	// Inline marks the A14 immediate-answer form: this result IS the job's
	// outcome (a natural completed/failed finish that committed within the
	// inline window). Web must treat async:false as not-pending
	// (isPendingJob, ActionRow.tsx).
	Inline         bool   `json:"inline,omitempty"`
	JobID          string `json:"job_id"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Status         string `json:"status"`
	// ClaimID tags this as the job's OWN "started" result (ack tag): the
	// delivery gate fuses only a result carrying the job's claim, never one
	// that merely shares its tool_call_id.
	ClaimID string `json:"claim_id,omitempty"`
}

// AsyncCapMetadata is the JSON shape the async-cap refusal emits as its
// ClientMetadata tag.
type AsyncCapMetadata struct {
	AsyncCap struct {
		Running    int `json:"running"`
		Limit      int `json:"limit"`
		WaitTimers int `json:"wait_timers"`
	} `json:"async_cap"`
}
