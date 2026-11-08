package server

// Peak-hours wire payload types beyond the legacy single-window form:
// weekly policy windows, pause/resume requests, and status snapshots.

// PeakHoursWindowWire is one weekly window inside the policy form of
// PeakHoursWirePayload. Days is a comma-separated weekday list ("mon,
// wed,fri"); Start and End are HH:MM wall-clock times in the policy's
// timezone.
type PeakHoursWindowWire struct {
	Days  string `json:"days,omitempty"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// PauseProviderPeakHoursPayload pauses peak-hours throttling for one
// provider. Duration ("2h", "90m", "7d", "6d12h") and Until (RFC3339) are
// alternatives; when both are empty the server pauses until the current
// peak window ends. Scope is "global" or "local"; empty defaults to
// "global".
type PauseProviderPeakHoursPayload struct {
	ID       string `json:"id"`
	Duration string `json:"duration,omitempty"`
	Until    string `json:"until,omitempty"`
	Scope    string `json:"scope,omitempty"`
}

// ResumeProviderPeakHoursPayload lifts an active pause for one provider in
// the chosen scope. Scope is "global" or "local"; empty defaults to
// "global".
type ResumeProviderPeakHoursPayload struct {
	ID    string `json:"id"`
	Scope string `json:"scope,omitempty"`
}

// GetPeakHoursStatusPayload asks for the current peak-hours status of one
// provider (by id) or of every configured provider when id is empty.
type GetPeakHoursStatusPayload struct {
	ID string `json:"id,omitempty"`
}

// PeakHoursStatusWire is the live peak-hours state of a single provider.
// RFC3339 fields are omitted when the underlying time is zero.
type PeakHoursStatusWire struct {
	ProviderID     string `json:"providerId"`
	CheckedAt      string `json:"checkedAt"`
	InPeak         bool   `json:"inPeak"`
	Paused         bool   `json:"paused,omitempty"`
	PausedUntil    string `json:"pausedUntil,omitempty"`
	Blocked        bool   `json:"blocked"`
	ReopensAt      string `json:"reopensAt,omitempty"`
	NextTransition string `json:"nextTransition,omitempty"`
	Timezone       string `json:"timezone,omitempty"`
	Message        string `json:"message,omitempty"`
}

// PeakHoursStatusesWire is the reply body of get_peak_hours_status.
type PeakHoursStatusesWire struct {
	CheckedAt string                `json:"checkedAt"`
	Statuses  []PeakHoursStatusWire `json:"statuses"`
}
