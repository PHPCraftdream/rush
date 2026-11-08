package agent

import (
	"errors"
	"fmt"
	"time"
)

// PeakHoursGuidance returns the orchestrator-facing instruction paragraph
// for ANY peak-hours refusal — the "refused at the start of a new
// invocation" case (checkPeakHours' pre-flight call sites). The gate is
// pre-flight only (task #56): a window opening mid-turn never aborts a
// running stream. The refusal must carry the same guidance: this is not a
// crash, rush will not retry on its own, and an orchestrating agent
// should schedule a resume for the exact reopen time rather than retry
// immediately.
//
// This is the single source of truth for the guidance text — both app.go
// (stderr output before fang's ERROR box) and peakHoursStoppedFinishText
// (DB finish-message details) call it, so the operator-visible stderr
// text and the recorded DB details stay identical.
//
// err is whatever checkPeakHours returned. When it is (or wraps) a
// *PeakHoursError we pull the exact ReopensAt time.Time out of it — via
// errors.As, not string parsing — so the instruction gives an
// orchestrating agent an unambiguous, cron-ready timestamp instead of
// making it parse "refusing until 12:00" out of prose. Falls back to a
// generic RESUME AT line if the type assertion fails (defensive: keeps
// working even if the error ever gets wrapped differently upstream).
func PeakHoursGuidance(err error) string {
	resumeLine := "RESUME AT: (see the time above)"
	var pe *PeakHoursError
	if errors.As(err, &pe) {
		if pe.ReopensAt.IsZero() {
			resumeLine = "RESUME AT: unknown — the provider is blocked around the clock; do not schedule a timed resume."
		} else {
			resumeLine = fmt.Sprintf(
				"RESUME AT: %s (local time, RFC3339: %s)",
				pe.ReopensAt.Format("2006-01-02 15:04"),
				pe.ReopensAt.Format(time.RFC3339),
			)
		}
		// The configured timezone is appended only when one is set, so
		// legacy-shape output (no timezone configured) stays byte-identical.
		if pe.Timezone != "" {
			resumeLine += fmt.Sprintf(" (timezone: %s)", pe.Timezone)
		}
	}
	guidance := fmt.Sprintf(
		"%s\n\n"+
			"This is not a crash — rush is intentionally refusing/halting because "+
			"of the provider's peak-hours window. rush is exiting now; it will "+
			"not retry on its own.\n\n"+
			"If an orchestrating agent is driving this session: schedule a resume "+
			"for RESUME AT above (e.g. CronCreate / a scheduled wakeup) and "+
			"re-invoke `rush run --session <this session>` then — do not retry "+
			"immediately, and do not pass --allow-peak-hours unless a human "+
			"operator explicitly asked for that specific run.",
		resumeLine,
	)
	// Operator-authored custom message (config.PeakHoursWindow.Message),
	// appended after a blank-line separator, exactly as with the sections
	// above — empty by default, so this is a no-op unless configured.
	if pe != nil && pe.Message != "" {
		guidance += "\n\n" + pe.Message
	}
	return guidance
}

// peakHoursStoppedFinishText builds the (msg, details) pair recorded as
// the Finish part when a pre-flight peak-hours refusal renders through
// the turn-failure path (agent_turn_failure.go). The gate is pre-flight
// only (task #56): this renders a refusal that blocked the request before
// it reached the provider.
func peakHoursStoppedFinishText(err error) (msg, details string) {
	msg = "Stopped: provider entered its peak-hours window mid-session"
	details = fmt.Sprintf("%s\n\n%s", err.Error(), PeakHoursGuidance(err))
	return msg, details
}
